package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/remote"
)

// cloud bootstrap (ONBOARDING-DESIGN §13, G8 W3, ANY-VM-PLAN §5): the
// owner's first device on a new server with device sign-in. This process
// makes the one-time owner invite's code and hands it to the server's
// `operator bootstrap-invite --code-stdin`: on AWS through a short-lived
// SecureString (as cloud secret set does), so it is in no command's
// parameters or SSM history; over plain SSH, or gcloud compute ssh over
// IAP on Google Cloud, on that session's stdin. It joins with
// the code in memory. The server stores only its salted hash; the code is
// never printed.

const cloudBootstrapUsage = `clawbridge cloud bootstrap (--ssh HOST | --vm NAME --project P --zone Z | --instance ID --region R [--profile P] [--account N])
                          --server URL --workspace NAME [--email OWNER] [--label L]
                          [--key-store keyring|file]
                          [--client-config PATH]`

const bootstrapRoute = "cloud bootstrap needs the server's route: --ssh HOST, --vm NAME --project P --zone Z on Google Cloud, or --instance ID and --region on AWS"

// bootstrapComposeFile is the deployment's Compose file on the instance, as
// the AWS playbook checks it out.
const bootstrapComposeFile = "/opt/clawbridge/repo/deploy/compose.yaml"

// joinDevice and probeJoin are JoinDevice and the lost-answer probe; tests
// replace them.
var (
	joinDevice = JoinDevice
	probeJoin  = probeUncertainJoin
)

// bootstrapRefusals maps the operator's fixed refusals to what to do.
var bootstrapRefusals = []struct{ text, reason, advice string }{
	{"another email than --expect-owner", "owner_mismatch", "the server's CLAWBRIDGE_BOOTSTRAP_OWNER is another email than --email; nothing was created: fix the env file, or bootstrap as that owner"},
	{"has 3 pending invites", "pending_invites", "the owner has 3 unused invites: wait for them to expire (24 hours) or revoke one with clawbridge invite revoke"},
	{"owner is not the bootstrap owner", "not_owner", "that workspace is owned by someone else than CLAWBRIDGE_BOOTSTRAP_OWNER; its owner joins with their own device, or name another --workspace"},
	{"names the bootstrap owner", "no_owner", "CLAWBRIDGE_BOOTSTRAP_OWNER is not set in the server's env file; set it (docs/operations/first-deployment.md)"},
	{"needs the hosted authority", "not_hosted", "the server is not a hosted deployment"},
}

func runCloudBootstrap(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var target cloudTarget
	f := flag.NewFlagSet("cloud bootstrap", flag.ContinueOnError)
	f.SetOutput(stderr)
	bindTarget(f, &target)
	server := f.String("server", "", "the server's public URL (its --public-origin)")
	workspace := f.String("workspace", "", "the workspace to create, owned by the server's CLAWBRIDGE_BOOTSTRAP_OWNER")
	label := f.String("label", "", "this device's label (default: this machine's name)")
	owner := f.String("email", "", "the owner's email, checked against the server's CLAWBRIDGE_BOOTSTRAP_OWNER (default: whatever it names)")
	store := f.String("key-store", "", "where the key is kept: keyring (the default), or file where there is no OS key store")
	clientConfig := f.String("client-config", "", "the profile to write (default: the selected one)")
	ordered, err := interspersedFlags(f, args)
	if err == nil {
		err = f.Parse(ordered)
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case f.NArg() != 0:
		return errors.New("usage: " + cloudBootstrapUsage)
	case !target.remote():
		return errors.New(bootstrapRoute)
	case *server == "":
		return errors.New("cloud bootstrap needs --server, the server's public URL")
	case !bootstrapWorkspace(*workspace):
		return errors.New("--workspace names the workspace: letters, digits, '.', '_' and '-', starting with a letter or digit")
	case *owner != "" && !bootstrapEmail.MatchString(*owner):
		return errors.New("--email must be an email address")
	}
	if _, ok := serverURL(*server); !ok {
		return errors.New("--server must be the server's https URL")
	}
	if *store, err = newKeyStore(*store, ""); err != nil {
		return err
	}
	tr, err := target.transport()
	if err != nil {
		return errors.New("cloud bootstrap did not run: " + err.Error())
	}
	code := devicekey.NewCode()
	if sh, ok := tr.(shellTransport); ok {
		if target.vm != "" {
			if note := osLoginNote(ctx, target); note != "" {
				fmt.Fprintln(stderr, note)
			}
		}
		out, err := sh.exchange(ctx, sshBootstrapScript(*workspace, *owner), strings.NewReader(code+"\n"), stderr)
		if err != nil {
			advice := sh.describe() + "; the invite may or may not exist, so run cloud bootstrap again (a second invite does no harm)"
			switch {
			case errors.Is(err, errHostKey):
				advice = hostKeyAdvice
			case errors.Is(err, errSudoPassword):
				advice = sudoAdvice
			case errors.Is(err, errIAPFirewall):
				advice = iapFirewallAdvice
			case exitCode(err) == 6:
				advice = "the server has no " + serverEnvFile + " naming COMPOSE_PROJECT_NAME; install the server first"
			}
			return errors.New("the server did not make the invite; " + advice + " (nothing was printed)")
		}
		return joinBootstrapped(ctx, out, code, *server, *workspace, *owner, *label, *store, *clientConfig, stdout, stderr)
	}
	ssm, ok := tr.(ssmTransport)
	if !ok {
		return errors.New(bootstrapRoute)
	}
	out, err := ssm.throughParameter(ctx, "bootstrap-code", code, "cloud bootstrap",
		"the server did not answer in time, so the invite may or may not exist; run cloud bootstrap again (a second invite does no harm)",
		func(name string) string { return ssmBootstrapScript(name, target.region, *workspace, *owner) }, stderr)
	// R128-M1: the invite exists even when only the delete failed; joining
	// uses the code up, so go on, and say where the parameter is.
	var left *leftParameter
	if out != nil && errors.As(err, &left) {
		defer fmt.Fprintln(stderr, "warning: "+left.Error()+" (it expires within 15 minutes)")
		err = nil
	}
	if err != nil {
		var specific secretAdvice
		advice := ssm.describe()
		if errors.As(err, &specific) {
			advice = string(specific)
		}
		var failed *ssmRunError
		if errors.As(err, &failed) && failed.code == 6 {
			advice = "the instance has no " + serverEnvFile + " naming COMPOSE_PROJECT_NAME; install the server first"
		}
		msg := "the server did not make the invite; " + advice + " (nothing was printed)"
		if i := strings.Index(err.Error(), "parameter left behind: "); i >= 0 {
			msg += "\n" + err.Error()[i:]
		}
		return errors.New(msg)
	}
	return joinBootstrapped(ctx, out, code, *server, *workspace, *owner, *label, *store, *clientConfig, stdout, stderr)
}

// joinBootstrapped reads the server's answer and, given an invite for the
// expected owner, joins this device with the code, whichever route made it.
func joinBootstrapped(ctx context.Context, out []byte, code, server, workspace, owner, label, store, clientConfig string, stdout, stderr io.Writer) error {
	invite, err := bootstrapInvite(out)
	if err != nil {
		return err
	}
	email, _ := invite["email"].(string)
	if owner != "" && !strings.EqualFold(strings.TrimSpace(owner), email) {
		return fmt.Errorf("the server's bootstrap owner is %s, not %s: its invite (unused, expires %v) is for %s; fix CLAWBRIDGE_BOOTSTRAP_OWNER, or bootstrap as that owner", email, owner, invite["expires_at"], email)
	}
	params := JoinParams{Server: server, Email: email, Code: code, Label: label, KeyStore: store, ConfigPath: clientConfig}
	joined, err := joinDevice(ctx, params)
	var uncertain *JoinUncertainError
	if errors.As(err, &uncertain) {
		// The answer was lost: the device may have joined. Ask the server,
		// signing with the kept key, before anyone asks for a new invite.
		joined, err = probeJoin(ctx, uncertain, workspace, params)
	}
	if err != nil {
		return fmt.Errorf("the server made the invite for %s (it expires %v), but joining failed: %w", email, invite["expires_at"], err)
	}
	if joined.Warning != "" {
		fmt.Fprintln(stderr, "warning: "+joined.Warning+".")
	}
	return emit(ctx, stdout, "cloud_bootstrap", map[string]any{"source": "cloud_bootstrap", "data": map[string]any{
		"workspace": joined.Workspace, "workspace_created": invite["workspace_created"], "principal": joined.Principal,
		"device_id": joined.DeviceID, "label": joined.Label, "key_store": joined.KeyStore, "config": joined.ConfigPath,
	}})
}

// bootstrapEmail is an --email safe in the script's single quotes.
var bootstrapEmail = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,253}$`)

// bootstrapWorkspace is a workspace name the server takes, safe inside the
// script's single quotes.
func bootstrapWorkspace(name string) bool {
	if name == "" || len(name) > 128 || !isAlnum(name[0]) {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; !isAlnum(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// ssmBootstrapScript reads the code with the instance's role and pipes it
// into the server's bootstrap-invite --code-stdin in a one-off api
// container. It prints one line: "invite JSON", or "refused" and the
// operator's own error line, which never holds the code. It exits 3
// without the AWS CLI, 5 when the role cannot read the parameter, and 6
// without the env file's Compose project. Every input is pattern-checked.
func ssmBootstrapScript(name, region, workspace, owner string) string {
	return ssmBootstrapScriptFor(name, region, workspace, owner, serverEnvFile, bootstrapComposeFile)
}

func ssmBootstrapScriptFor(name, region, workspace, owner, envFile, composeFile string) string {
	return strings.Join([]string{
		"command -v aws >/dev/null || exit 3",
		"aws ssm get-parameter --name " + name + " --region " + region + " --query Parameter.Name --output text >/dev/null || exit 5",
		bootstrapScriptBody("aws ssm get-parameter --with-decryption --name "+name+" --region "+region+" --query Parameter.Value --output text", workspace, owner, envFile, composeFile),
	}, "\n")
}

// sshBootstrapScript reads the code from its stdin, which is ssh's: over
// plain SSH the code travels nowhere else (ANY-VM-PLAN §5).
func sshBootstrapScript(workspace, owner string) string {
	return sshBootstrapScriptFor(workspace, owner, serverEnvFile, bootstrapComposeFile)
}

func sshBootstrapScriptFor(workspace, owner, envFile, composeFile string) string {
	return bootstrapScriptBody("head -n 1", workspace, owner, envFile, composeFile)
}

// bootstrapScriptBody pipes the code from codeSource into a one-off api
// container's bootstrap-invite --code-stdin, in the env file's Compose
// project, and prints only the invite line or the operator's own error
// line. A deployment that pulls the published image (CLAWBRIDGE_IMAGE set)
// adds compose.image.yaml beside composeFile, so api is never built.
func bootstrapScriptBody(codeSource, workspace, owner, envFile, composeFile string) string {
	expect := ""
	if owner != "" {
		expect = " --expect-owner " + owner
	}
	return strings.Join([]string{
		"f=" + envFile,
		`p=$(sed -n "s/^COMPOSE_PROJECT_NAME=//p" "$f" 2>/dev/null | tail -n 1)`,
		`[ -n "$p" ] || exit 6`,
		`i=""`,
		`if grep -q "^CLAWBRIDGE_IMAGE=." "$f"; then i="-f ` + strings.TrimSuffix(composeFile, "compose.yaml") + `compose.image.yaml"; fi`,
		`e=$(mktemp)`,
		`trap "rm -f \"\$e\"" EXIT`,
		"if out=$(" + codeSource + " |" +
			` docker compose --env-file "$f" --project-name "$p" -f ` + composeFile + ` $i run --rm --no-deps -T api operator bootstrap-invite --workspace ` + workspace + expect + ` --code-stdin 2>"$e"); then`,
		`  printf "invite %s\n" "$(printf "%s\n" "$out" | tail -n 1)"`,
		"else",
		`  printf "refused %s\n" "$(grep "^clawbridge: " "$e" | tail -n 1 | cut -c1-300)"`,
		"fi",
	}, "\n")
}

// bootstrapInvite reads the script's answer: the invite, without its code,
// or the operator's refusal turned into a fixed reason.
func bootstrapInvite(out []byte) (map[string]any, error) {
	line := strings.TrimSpace(string(out))
	if rest, ok := strings.CutPrefix(line, "invite "); ok {
		var invite map[string]any
		if json.Unmarshal([]byte(rest), &invite) != nil || invite["email"] == nil || invite["code"] != nil {
			return nil, errors.New("the server's answer was unreadable; check that it runs this release (clawbridge cloud check version)")
		}
		return invite, nil
	}
	if rest, ok := strings.CutPrefix(line, "refused"); ok {
		for _, r := range bootstrapRefusals {
			if strings.Contains(rest, r.text) {
				return nil, fmt.Errorf("the server refused the invite (%s): %s", r.reason, r.advice)
			}
		}
		if strings.Contains(rest, "code-stdin") || strings.Contains(rest, "flag provided but not defined") {
			return nil, errors.New("the server refused the invite (old_server): it runs a release without bootstrap-invite --code-stdin; update it first (docs/operations/deployment.md)")
		}
		return nil, errors.New("the server refused the invite (refused): run operator bootstrap-invite on the instance to see why")
	}
	return nil, errors.New("the server's answer was unreadable; check that it runs this release (clawbridge cloud check version)")
}

// probeUncertainJoin asks the server, signing with the key a lost join
// answer kept, whether the device joined. If it did, it writes the profile
// JoinDevice would have; otherwise the key stays and u is returned.
func probeUncertainJoin(ctx context.Context, u *JoinUncertainError, workspace string, p JoinParams) (JoinResult, error) {
	path, err := joinConfigPath(p.ConfigPath)
	if err != nil {
		return JoinResult{}, err
	}
	cache := filepath.Join(filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+".cache.db")
	client, err := remote.New(remote.Config{URL: u.Server, Principal: u.Principal, CachePath: cache, CredentialSource: remote.CredentialSourceDevice, DeviceID: u.DeviceID, KeyStore: u.KeyStore, KeyHome: u.KeyHome})
	if err != nil {
		return JoinResult{}, u
	}
	r, err := client.Execute(ctx, bus.Request{Operation: "whoami", Workspace: workspace, Args: map[string]any{}})
	who, _ := r.Data.(map[string]any)
	if err != nil || who == nil || who["principal"] != u.Principal {
		// Still unknown (a refusal may be the clock, not a missing device):
		// the key stays, and the error says how to go on.
		return JoinResult{}, u
	}
	values := map[string]any{
		"mode": "cloud", "server": u.Server, "workspace": workspace, "principal": u.Principal,
		"credential_source": remote.CredentialSourceDevice, "device_id": u.DeviceID, "key_store": u.KeyStore, "cache": cache,
	}
	if err = writeDeviceRoute(path, values); err != nil {
		return JoinResult{}, fmt.Errorf("joined as device %s, but the profile %s could not be written; set %s in it by hand: %w", u.DeviceID, path, routeValues(values), err)
	}
	out := JoinResult{DeviceID: u.DeviceID, Principal: u.Principal, Workspace: workspace, KeyStore: u.KeyStore, ConfigPath: path}
	if u.KeyStore == devicesign.StoreFile {
		out.Warning = devicesign.FileWarning
	}
	return out, nil
}
