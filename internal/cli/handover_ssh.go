package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"

	"clawbridge/internal/clirender"
	"clawbridge/internal/secretenv"
)

// The hand-over for a server the user already has (ANY-VM-PLAN §7): the
// same shape as on Google Cloud and AWS, with the SSH host as the scope
// instead of a cloud CLI's configuration. ClawBridge never touches the
// user's ~/.ssh; after the agent exits it runs the checks over the user's
// own SSH, and they decide.

var storeRegionPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

func askSSHInputs(prompt *wizardPrompt, stdout io.Writer) (*onboardingState, error) {
	fmt.Fprintln(stdout, "\nYour server and backup store. None of these is a secret. The host is reached with your own ssh, as a")
	fmt.Fprintln(stdout, "user with passwordless sudo; the backup store is S3-compatible storage off the host (B2, R2, S3 and the like).")
	s := &onboardingState{Cloud: "ssh"}
	if err := askQuestions(prompt, stdout, s, sshQuestions()); err != nil {
		return nil, err
	}
	return s, nil
}

// sshQuestions are the SSH route's inputs. The endpoint is what
// backup-postgres.sh takes: https://HOST[:PORT], nothing else, never this
// machine by name.
func sshQuestions() []inputQuestion {
	return []inputQuestion{
		{"SSH host (user@host, or an alias from ~/.ssh/config)", func(s *onboardingState) *string { return &s.Host }, fixed(""), matches(sshHostPattern),
			"the SSH host must be user@host or an alias, starting with a letter or digit"},
		domainQuestion,
		ownerQuestion,
		{"Backup store's S3 endpoint (https://HOST)", func(s *onboardingState) *string { return &s.Endpoint }, fixed(""),
			func(_ *onboardingState, v string) bool { return backupEndpoint(v) },
			"the backup endpoint must be https://HOST or https://HOST:PORT, off this server, with nothing after it"},
		{"Backup bucket", func(s *onboardingState) *string { return &s.Bucket }, fixed(""), matches(s3BucketPattern),
			"the bucket must be a valid bucket name"},
		{"Backup store's region (auto on R2)", func(s *onboardingState) *string { return &s.Region }, fixed(""), matches(storeRegionPattern),
			"the region must be the store's own region name, like eu-central-003 or auto"},
	}
}

func backupEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || strings.HasSuffix(raw, "?") || strings.HasSuffix(raw, "#") {
		return false
	}
	host := u.Hostname()
	if strings.HasSuffix(u.Host, ":") {
		return false // an empty port
	}
	switch {
	case host == "localhost", strings.HasSuffix(host, ".localhost"), strings.HasPrefix(host, "127."), strings.HasPrefix(host, "0."):
		return false
	}
	return domainPattern.MatchString(host) || ipv4Pattern.MatchString(host)
}

var ipv4Pattern = regexp.MustCompile(`^([0-9]{1,3}\.){3}[0-9]{1,3}$`)

// sshTaskText is the TASK.md the agent starts from on the SSH route.
func sshTaskText(s *onboardingState) string {
	return fmt.Sprintf(`# ClawBridge onboarding: install on your own server

Load the clawbridge-provision-vm skill and follow its hard rules; say them to
the user first. Work through its steps 1 to 6 with these inputs:

- SSH host: %s
- Domain: %s
- Owner email: %s
- Backup endpoint: %s
- Backup bucket: %s
- Backup region: %s

Every command on the server goes through ssh to exactly this host, with the
options in the skill's commands.md. You run commands there as root: never
print or read a value from /etc/clawbridge/clawbridge.env.

Record everything you create in created.txt in this directory.

Finish by running, and reporting verbatim:

    clawbridge cloud check infra server version backups --ssh %s --server https://%s

Stop there. The user runs the sign-in step (the skill's step 5, Sign-in) themselves,
after you exit.
`, s.Host, s.Domain, s.Owner, s.Endpoint, s.Bucket, s.Region, s.Host, s.Domain)
}

// handOverSSH is handOver for a server reached over SSH.
func handOverSSH(ctx context.Context, s *onboardingState, bin, skillAgent, path string, stdout, stderr io.Writer) (retErr error) {
	if err := installPlaybook("vm", skillAgent); err != nil {
		return fmt.Errorf("the playbook could not be installed for the agent: %w", err)
	}
	base, err := onboardingDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "provision")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "TASK.md"), []byte(sshTaskText(s)), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nStarting %s in %s. Exit it when it is done; ClawBridge then runs the checks.\n\n", bin, dir)
	if err = runAgent(ctx, bin, dir, secretenv.Scrub(os.Environ()), "Read TASK.md in this directory and follow it."); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	// Whatever the agent said or how it exited, the checks decide.
	fmt.Fprintln(stdout, "\nChecking the server…")
	results, err := sshHandoverChecks(ctx, s, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stdout, "\nNot done yet. Run clawbridge onboard to hand over again (your agent resumes from created.txt),")
		fmt.Fprintln(stdout, "or follow docs/operations/first-deployment.md yourself.")
		return nil
	}
	s.Step = "signin"
	if err = saveOnboarding(s); err != nil {
		return err
	}
	prompt, err := newWizardPrompt(ctx, stdout, "guided terminal input cleanup failed; the server is up, restore the terminal before retrying")
	if err != nil {
		return err
	}
	defer func() {
		if e := prompt.close(); e != nil && !errors.Is(retErr, e) {
			retErr = errors.Join(retErr, e)
		}
	}()
	answer, err := prompt.ask("\nYour backup store's key, on the server: can it put and list but not delete, with versioning or a\nlock on the bucket? (backup-and-recovery.md has the table per store) [y/N]: ")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("the server is up, but the backup key question wasn't answered (%w); run clawbridge onboard again to finish the sign-in", err)
	}
	a := strings.ToLower(strings.TrimSpace(answer))
	fmt.Fprint(stdout, sshSummary(s, results, a == "y" || a == "yes"))
	return finishSignin(ctx, s, prompt, path, stdout, stderr)
}

// sshHandoverChecks runs infra, server, version and backups over the
// user's ssh, shows them as the check command does, and returns them.
func sshHandoverChecks(ctx context.Context, s *onboardingState, stdout, stderr io.Writer) ([]cloudResult, error) {
	var buf bytes.Buffer
	args := []string{"infra", "server", "version", "backups", "--ssh", s.Host, "--server", "https://" + s.Domain, "--json"}
	err := runHandoverChecks(ctx, args, &buf, stderr)
	var got struct {
		Data []cloudResult `json:"data"`
	}
	if json.Unmarshal(buf.Bytes(), &got) != nil || len(got.Data) == 0 {
		if err == nil {
			err = errors.New("the checks gave no results")
		}
		fmt.Fprintln(stdout, "The checks did not run: "+clirender.Safe(fmt.Sprint(err)))
		return nil, err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tSTATUS\tREASON\tADVICE")
	for _, r := range got.Data {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", clirender.Safe(r.Check), clirender.Safe(r.Status), clirender.Safe(r.Reason), clirender.Safe(r.Advice))
	}
	if e := tw.Flush(); e != nil {
		return nil, e
	}
	return got.Data, err
}

// sshSummary is the hand-over summary: what was set up, the key scope as
// the user confirmed it (no check can see a key's rights), and every
// warning, so none stays only in the log.
func sshSummary(s *onboardingState, results []cloudResult, scoped bool) string {
	var b strings.Builder
	b.WriteString("\nHand-over summary\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  Host\t%s\n", clirender.Safe(s.Host))
	fmt.Fprintf(tw, "  Domain\t%s\n", clirender.Safe(s.Domain))
	fmt.Fprintf(tw, "  Backup store\ts3://%s at %s\n", clirender.Safe(s.Bucket), clirender.Safe(s.Endpoint))
	scope := "put and list only, confirmed by you"
	if !scoped {
		scope = "NOT confirmed: give the host a key that can't delete (docs/operations/backup-and-recovery.md)"
	}
	fmt.Fprintf(tw, "  Backup key\t%s\n", scope)
	var passed []string
	for _, r := range results {
		if r.Status == "pass" {
			passed = append(passed, clirender.Safe(r.Check))
		}
	}
	fmt.Fprintf(tw, "  Checks passed\t%s\n", strings.Join(passed, ", "))
	for _, r := range results {
		if r.Status == "warn" {
			fmt.Fprintf(tw, "  WARN %s\t%s: %s\n", clirender.Safe(r.Check), clirender.Safe(r.Reason), clirender.Safe(r.Advice))
		}
	}
	tw.Flush()
	return b.String()
}
