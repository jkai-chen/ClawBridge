package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/secretenv"
	"clawbridge/internal/skill"
)

// The own-server path's hand-over (live/ONBOARDING-DESIGN.md §6, §10): the
// user's own coding agent provisions with the playbook, in this terminal,
// and the checks, not the agent, decide when it is done. Progress is saved
// so `clawbridge onboard` resumes it; it holds names and choices only,
// never a secret.

type onboardingState struct {
	Version string `json:"version"`
	Cloud   string `json:"cloud"`
	Step    string `json:"step"` // provision, then signin
	Account string `json:"account"`
	Project string `json:"project"`
	Region  string `json:"region"`
	Zone    string `json:"zone"`
	Domain  string `json:"domain"`
	Owner   string `json:"owner"`
	Size    string `json:"size"`
	Bucket  string `json:"bucket"`
	// AWS only: the named profile, and the instance the checks found.
	Profile  string `json:"profile,omitempty"`
	Instance string `json:"instance,omitempty"`
}

const gcloudConfiguration = "clawbridge-onboarding"

var (
	regionPattern = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	emailPattern  = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+\.)+[A-Za-z]{2,63}$`)
	vmSizes       = map[string]bool{"e2-small": true, "e2-medium": true}
	awsSizes      = map[string]bool{"t4g.small": true, "t4g.medium": true}
)

// handoverAgents are the agent CLIs a hand-over can start, with the skill
// target each one reads.
var handoverAgents = []struct{ bin, name, skillAgent string }{
	{"claude", "Claude Code", "claude-code"},
	{"codex", "Codex", "codex"},
}

var (
	lookAgent = exec.LookPath
	// runAgent runs the agent CLI in the foreground of this terminal.
	runAgent = func(ctx context.Context, bin, dir string, env []string, prompt string) error {
		cmd := exec.CommandContext(ctx, bin, prompt)
		cmd.Dir, cmd.Env = dir, env
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	runGcloud = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "gcloud", args...).Output()
	}
	installPlaybook = func(cloud, agent string) error {
		targets, err := skill.TargetsFor(skill.Playbooks[cloud], agent, "")
		if err != nil {
			return err
		}
		results, err := skill.Install(targets, false)
		for _, r := range results {
			if r.Refused != "" && err == nil {
				err = errors.New(r.Refused)
			}
		}
		return err
	}
	runHandoverChecks = func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		return runCloudCheck(ctx, args, stdout, stderr)
	}
)

func onboardingDir() (string, error) {
	home, err := stateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "onboarding"), nil
}

func loadOnboarding() (*onboardingState, error) {
	dir, err := onboardingDir()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "onboarding.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s onboardingState
	if err = json.Unmarshal(raw, &s); err != nil {
		return nil, errors.New("the saved onboarding progress is unreadable; remove it to start over")
	}
	return &s, nil
}

func saveOnboarding(s *onboardingState) error {
	dir, err := onboardingDir()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	s.Version = buildinfo.Version
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".onboarding.json.tmp")
	if err = os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "onboarding.json"))
}

// validInputs reports the first input that is not well formed.
func validInputs(s *onboardingState) error {
	switch s.Cloud {
	case "aws":
		return validAWSInputs(s)
	case "gcp":
	default:
		return errors.New("the saved progress names an unknown cloud; remove it to start over")
	}
	switch {
	case !emailPattern.MatchString(s.Account):
		return errors.New("the Google account must be an email address")
	case !projectPattern.MatchString(s.Project):
		return errors.New("the project must be a Google Cloud project ID")
	case !regionPattern.MatchString(s.Region):
		return errors.New("the region must look like europe-west1")
	case !zonePattern.MatchString(s.Zone) || !strings.HasPrefix(s.Zone, s.Region+"-"):
		return errors.New("the zone must be in the region, like europe-west1-b")
	case !domainPattern.MatchString(s.Domain):
		return errors.New("the domain must be a DNS name you control, like clawbridge.example.com")
	case !emailPattern.MatchString(s.Owner):
		return errors.New("the owner must be an email address")
	case !vmSizes[s.Size]:
		return errors.New("the VM size must be e2-small or e2-medium")
	case !bucketPattern.MatchString(s.Bucket):
		return errors.New("the bucket must be a valid, globally unique bucket name")
	}
	return nil
}

// validAWSInputs is validInputs for AWS. The instance is set only after the
// checks found it.
func validAWSInputs(s *onboardingState) error {
	switch {
	case awsGOOS == "windows":
		return errAWSOnWindows
	case !awsProfilePattern.MatchString(s.Profile):
		return errors.New("the profile must be a named AWS CLI profile")
	case !accountPattern.MatchString(s.Account):
		return errors.New("the account must be a 12-digit AWS account ID")
	case !awsRegionPattern.MatchString(s.Region):
		return errors.New("the region must look like eu-west-1")
	case len(s.Zone) != len(s.Region)+1 || !strings.HasPrefix(s.Zone, s.Region) || s.Zone[len(s.Region)] < 'a' || s.Zone[len(s.Region)] > 'z':
		return errors.New("the zone must be in the region, like eu-west-1a")
	case !domainPattern.MatchString(s.Domain):
		return errors.New("the domain must be a DNS name you control, like clawbridge.example.com")
	case !emailPattern.MatchString(s.Owner):
		return errors.New("the owner must be an email address")
	case !awsSizes[s.Size]:
		return errors.New("the instance size must be t4g.small or t4g.medium")
	case !s3BucketPattern.MatchString(s.Bucket):
		return errors.New("the bucket must be a valid, globally unique bucket name")
	case s.Instance != "" && !instancePattern.MatchString(s.Instance):
		return errors.New("the saved instance is not an EC2 instance ID")
	}
	return nil
}

// handoverControls is what the user reads before the agent takes over.
func handoverControls(cloud string) string {
	what := `
What your agent will do: load the clawbridge-provision-gcp playbook and follow it in your project. It
creates a VM, a static IP, a data disk, firewall rules, a backup bucket and a service account, after
showing you the cost and getting your yes, then installs the server.
`
	if cloud == "aws" {
		what = `
What your agent will do: load the clawbridge-provision-aws playbook and follow it in your account. It
creates a network, an HTTPS-only security group, a backup bucket, a role, a data volume, an instance
reached over SSM (no SSH) and an Elastic IP, after showing you the cost and getting your yes, then
installs the server.
`
	}
	return what + handoverRules
}

const handoverRules = `
The real controls: the playbook's rules guide the agent, but what protects you is that you approve
each of its commands, and that ClawBridge's checks, not the agent, decide when a step is done. Keep the
agent in its normal approval mode; avoid bypass or auto-approve modes for this hand-over.

The agent holds this terminal. When it asks you to enter a secret (clawbridge cloud secret set KEY), run that
in a second terminal: secrets never go through the agent.
`

// summary names the saved progress in one line.
func (s *onboardingState) summary() string {
	if s.Cloud == "aws" {
		return "AWS account " + wizardSafe(s.Account) + ", domain " + wizardSafe(s.Domain) + ", step " + wizardSafe(s.Step)
	}
	return "project " + wizardSafe(s.Project) + ", domain " + wizardSafe(s.Domain) + ", step " + wizardSafe(s.Step)
}

// startOwnServer is Start's third choice.
func startOwnServer(ctx context.Context, prompt *wizardPrompt, stdout, stderr io.Writer) error {
	var agents []int
	for i, a := range handoverAgents {
		if _, err := lookAgent(a.bin); err == nil {
			agents = append(agents, i)
		}
	}
	if len(agents) == 0 {
		_, err := io.WriteString(stdout, ownServerText+"\nNo Claude Code or Codex found on PATH, so follow the written guide.\n")
		return err
	}
	state, err := loadOnboarding()
	if err != nil {
		return err
	}
	if state != nil {
		fmt.Fprintf(stdout, "\nSaved progress: %s.\n", state.summary())
		if state.Version != buildinfo.Version {
			fmt.Fprintln(stdout, "It was saved by another ClawBridge version, so the step starts again.")
		}
		answer, err := prompt.ask("Resume it? [Y/n] (n starts over): ")
		if err != nil {
			return err
		}
		if a := strings.ToLower(strings.TrimSpace(answer)); a == "n" || a == "no" {
			state = nil
		} else if err = validInputs(state); err != nil {
			// Saved values reach gcloud's argv and TASK.md: check them again.
			return fmt.Errorf("the saved progress is not valid (%w); run clawbridge onboard and start over", err)
		}
	}
	if state == nil {
		answer, err := prompt.ask("Which cloud? 1. Google Cloud or 2. AWS [1]: ")
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "", "1", "gcp":
			state, err = askInputs(ctx, prompt, stdout)
		case "2", "aws":
			// Before any question, and long before the agent creates anything.
			if awsGOOS == "windows" {
				return errAWSOnWindows
			}
			state, err = askAWSInputs(ctx, prompt, stdout)
		default:
			return errors.New("answer 1 or 2 to the cloud question; run clawbridge onboard to start again")
		}
		if err != nil {
			return err
		}
		state.Step = "provision"
		if err = saveOnboarding(state); err != nil {
			return err
		}
	}
	if state.Step == "signin" {
		return finishSignin(state, stdout)
	}
	agent := handoverAgents[agents[0]]
	if len(agents) > 1 {
		answer, err := prompt.ask("Hand over to 1. Claude Code or 2. Codex? [1]: ")
		if err != nil {
			return err
		}
		if strings.TrimSpace(answer) == "2" {
			agent = handoverAgents[agents[1]]
		}
	}
	fmt.Fprint(stdout, handoverControls(state.Cloud))
	answer, err := prompt.ask("Hand over to " + agent.name + " now? [y/N] (n shows the written steps): ")
	if err != nil {
		return err
	}
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		_, err = io.WriteString(stdout, ownServerText)
		return err
	}
	if err = prompt.close(); err != nil {
		return err
	}
	return handOver(ctx, state, agent.bin, agent.skillAgent, stdout, stderr)
}

func askInputs(ctx context.Context, prompt *wizardPrompt, stdout io.Writer) (*onboardingState, error) {
	fmt.Fprintln(stdout, "\nYour Google Cloud choices. None of these is a secret.")
	account := ""
	if out, err := runGcloud(ctx, "config", "get", "account"); err == nil {
		account = strings.TrimSpace(string(out))
	}
	s := &onboardingState{}
	for _, q := range []struct {
		label, fallback string
		dest            *string
	}{
		{"Google account", account, &s.Account},
		{"Project ID", "", &s.Project},
		{"Region", "europe-west1", &s.Region},
		{"Zone", "", &s.Zone},
		{"Domain for the server", "", &s.Domain},
		{"Owner's email (becomes workspace owner)", "", &s.Owner},
		{"VM size, e2-small or e2-medium", "e2-small", &s.Size},
		{"Backup bucket", "", &s.Bucket},
	} {
		fallback := q.fallback
		switch q.dest {
		case &s.Zone:
			fallback = s.Region + "-b"
		case &s.Owner:
			fallback = s.Account
		case &s.Bucket:
			fallback = s.Project + "-clawbridge-backups"
		}
		label := q.label + ": "
		if fallback != "" {
			label = q.label + " [" + fallback + "]: "
		}
		answer, err := prompt.ask(label)
		if err != nil {
			return nil, err
		}
		if *q.dest = strings.TrimSpace(answer); *q.dest == "" {
			*q.dest = fallback
		}
	}
	s.Cloud = "gcp"
	if err := validInputs(s); err != nil {
		return nil, fmt.Errorf("%w; run clawbridge onboard to start again", err)
	}
	return s, nil
}

// taskText is the TASK.md the agent starts from: the step and the user's
// choices, never a secret or a token.
func taskText(s *onboardingState) string {
	return fmt.Sprintf(`# ClawBridge onboarding: provision on Google Cloud

Load the clawbridge-provision-gcp skill and follow its hard rules; say them to
the user first. Work through its steps 1 to 6 with these inputs:

- Google account: %s
- Project: %s
- Region: %s
- Zone: %s
- Domain: %s
- Owner email: %s
- VM size: %s
- Backup bucket: %s

The gcloud configuration %s is active in this shell and already names the
account, project, region and zone; still name them in every command.

Record every resource you create in created.txt in this directory.

Finish by running, and reporting verbatim:

    clawbridge cloud check infra server version --server https://%s --vm clawbridge-1 --project %s --zone %s

Stop there. The user runs the sign-in step (the skill's step 7) themselves,
after you exit.
`, s.Account, s.Project, s.Region, s.Zone, s.Domain, s.Owner, s.Size, s.Bucket, gcloudConfiguration, s.Domain, s.Project, s.Zone)
}

// handOver prepares the agent's working directory, the playbook and a
// dedicated gcloud configuration, runs the agent in this terminal, then runs
// the step's checks itself.
func handOver(ctx context.Context, s *onboardingState, bin, skillAgent string, stdout, stderr io.Writer) error {
	if s.Cloud == "aws" {
		return handOverAWS(ctx, s, bin, skillAgent, stdout, stderr)
	}
	if err := installPlaybook("gcp", skillAgent); err != nil {
		return fmt.Errorf("the playbook could not be installed for the agent: %w", err)
	}
	if _, err := runGcloud(ctx, "config", "configurations", "describe", gcloudConfiguration); err != nil {
		if _, err = runGcloud(ctx, "config", "configurations", "create", gcloudConfiguration, "--no-activate"); err != nil {
			return errors.New("could not create the gcloud configuration " + gcloudConfiguration + "; is gcloud installed and signed in?")
		}
	}
	for _, kv := range [][2]string{{"account", s.Account}, {"project", s.Project}, {"compute/region", s.Region}, {"compute/zone", s.Zone}} {
		if _, err := runGcloud(ctx, "--configuration="+gcloudConfiguration, "config", "set", kv[0], kv[1]); err != nil {
			return errors.New("could not set " + kv[0] + " in the gcloud configuration " + gcloudConfiguration)
		}
	}
	base, err := onboardingDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "provision")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "TASK.md"), []byte(taskText(s)), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nStarting %s in %s. Exit it when it is done; ClawBridge then runs the checks.\n\n", bin, dir)
	// G9 F3: the agent gets the user's environment without ClawBridge's
	// own secrets.
	env := append(secretenv.Scrub(os.Environ()), "CLOUDSDK_ACTIVE_CONFIG_NAME="+gcloudConfiguration)
	if err = runAgent(ctx, bin, dir, env, "Read TASK.md in this directory and follow it."); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	// Whatever the agent said or how it exited, the checks decide.
	fmt.Fprintln(stdout, "\nChecking the server…")
	args := []string{"infra", "server", "version", "--server", "https://" + s.Domain, "--vm", "clawbridge-1", "--project", s.Project, "--zone", s.Zone}
	// The checks' gcloud calls use the same configuration as the agent's.
	restore := setEnv("CLOUDSDK_ACTIVE_CONFIG_NAME", gcloudConfiguration)
	err = runHandoverChecks(ctx, args, stdout, stderr)
	restore()
	if err != nil {
		fmt.Fprintln(stdout, "\nNot done yet. Run clawbridge onboard to hand over again (your agent resumes from created.txt),")
		fmt.Fprintln(stdout, "or follow docs/operations/first-deployment.md yourself.")
		return nil
	}
	s.Step = "signin"
	if err = saveOnboarding(s); err != nil {
		return err
	}
	return finishSignin(s, stdout)
}

// setEnv sets one variable for this process and its children, and returns
// how to put it back.
func setEnv(key, value string) func() {
	old, had := os.LookupEnv(key)
	os.Setenv(key, value)
	return func() {
		if had {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	}
}

// finishSignin names the sign-in step the user runs in their own terminal.
func finishSignin(s *onboardingState, stdout io.Writer) error {
	if s.Cloud == "aws" {
		_, err := fmt.Fprintf(stdout, `
The server is up. Last step, in your own terminal (not your agent's): create the workspace as its owner,
following the sign-in section of the clawbridge-provision-aws skill (references/signin.md). Then check:

  clawbridge cloud check workspace
  clawbridge cloud check backups --bucket %s --region %s --profile %s
`, s.Bucket, s.Region, s.Profile)
		return err
	}
	_, err := fmt.Fprintf(stdout, `
The server is up. Last step, in your own terminal (not your agent's): create the workspace as its owner,
following the sign-in section of the clawbridge-provision-gcp skill (references/signin.md). Then check:

  clawbridge cloud check workspace
  clawbridge cloud check backups --bucket %s --project %s
`, s.Bucket, s.Project)
	return err
}
