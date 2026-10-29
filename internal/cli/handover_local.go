package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/secretenv"
)

// The hand-over on this machine or the LAN (docs/operations/local-host.md):
// the same shape as on a cloud. The agent checks Docker, clones ClawBridge's
// source at this release into <provision>/clawbridge and runs
// deploy/local-host.sh up; it creates no cloud resource. Then ClawBridge runs
// cloud check --local itself, and the owner makes the sign-in code in their
// own terminal: the script never prints it without one.

var errLocalOnWindows = errors.New("this machine or your LAN needs macOS, Linux or WSL: run clawbridge onboard inside WSL, where deploy/local-host.sh runs")

// localSoloURL is shape A's server: local-host.sh's default port, which the
// onboarding never changes.
const localSoloURL = "http://127.0.0.1:8443"

var (
	// localWorkspacePattern is local-host.sh's workspace NAME rule.
	localWorkspacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	// releaseTagPattern is a canonical release version, as
	// scripts/build-release.sh accepts it; anything else is a dev build.
	releaseTagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// localWrittenSteps is what "n" to the hand-over prints for this machine or
// the LAN: the same steps, with the user's values, to run themselves.
func localWrittenSteps(s *onboardingState) string {
	source := "git clone --depth 1 --branch " + buildinfo.Version + " https://github.com/jkai-chen/ClawBridge clawbridge"
	if !releaseTagPattern.MatchString(buildinfo.Version) {
		source = "a development build: clone the release tag you want, or use your own checkout, as ./clawbridge"
	}
	trust := ""
	if s.Shape == "lan" {
		trust = "     sh clawbridge/deploy/local-host.sh trust, then the commands it prints for your OS.\n"
	}
	return fmt.Sprintf(`
On this machine, in order. Each step is done when its check passes:
  1. Get the source at this version:   %s
  2. Start the server:                 sh clawbridge/deploy/local-host.sh %s
  3. Check it:                         clawbridge cloud check server version --local --server %s
  4. Sign in, in your own terminal:
%s     sh clawbridge/deploy/local-host.sh invite-owner --workspace %s (if up showed no code),
     then clawbridge join --server %s --email %s
Then run clawbridge onboard again. The full guide is docs/operations/local-host.md.
`, source, localUpArgs(s), localSoloURL, trust, s.Workspace, localServerURL(s), s.Owner)
}

// validLocalHost is local-host.sh's HOST rule: a DNS name or an IPv4
// address, letters, digits, '.' and '-', no leading '.' or '-', no trailing
// '.', no "..", at most 253 characters.
func validLocalHost(v string) bool {
	return len(v) <= 253 && localDomainPattern.MatchString(v) && !strings.Contains(v, "..")
}

// validLocalBind is the LAN address to publish on: a dotted IPv4 address,
// neither every address (0.0.0.0) nor loopback.
func validLocalBind(v string) bool {
	ip := net.ParseIP(v)
	return ip != nil && ip.To4() != nil && ip.String() == v && !ip.IsUnspecified() && !ip.IsLoopback()
}

// localQuestions are the inputs after the shape: the owner and the
// workspace, and on the LAN the host and the address.
func localQuestions(shape string) []inputQuestion {
	questions := []inputQuestion{
		ownerQuestion,
		{"Workspace name", func(s *onboardingState) *string { return &s.Workspace }, fixed("my-team"), matches(localWorkspacePattern),
			"a workspace name is lowercase letters, digits, '_' and '-', starting with a letter or digit, at most 63"},
	}
	if shape == "lan" {
		questions = append(questions,
			inputQuestion{"LAN host name or IPv4 address (like clawbridge.lan)", func(s *onboardingState) *string { return &s.Domain }, fixed(""),
				func(_ *onboardingState, v string) bool { return validLocalHost(v) },
				"the host is a DNS name or IPv4 address: letters, digits, '.' and '-', no leading '.' or '-', no trailing '.', no '..'"},
			inputQuestion{"This machine's LAN address to publish on (like 192.0.2.10)", func(s *onboardingState) *string { return &s.Bind }, fixed(""),
				func(_ *onboardingState, v string) bool { return validLocalBind(v) },
				"the address is this machine's IPv4 address on the LAN, not 0.0.0.0 or 127.0.0.1"},
		)
	}
	return questions
}

func askLocalInputs(prompt *wizardPrompt, stdout io.Writer) (*onboardingState, error) {
	fmt.Fprintln(stdout, "\nYour choices for a server on this machine. None of these is a secret.")
	shape, err := askUntil(prompt.ask, stdout, "Who uses it? 1. Just me, on this machine 2. My team, on our LAN [1]: ", func(a string) (string, error) {
		if v, ok := pick(a, "1", shapeChoices); ok {
			return v, nil
		}
		return "", errors.New("choose 1 or 2")
	})
	if err != nil {
		return nil, err
	}
	s := &onboardingState{Cloud: "local", Shape: shape}
	if err = askQuestions(prompt, stdout, s, localQuestions(shape)); err != nil {
		return nil, err
	}
	// local-host.sh accepts either case; one form lets signedIn compare.
	s.Domain = strings.ToLower(s.Domain)
	return s, nil
}

// localServerURL is where the server answers: loopback in shape A, the LAN
// host over https (Caddy's internal CA) in shape B.
func localServerURL(s *onboardingState) string {
	if s.Shape == "lan" {
		return "https://" + s.Domain
	}
	return localSoloURL
}

// localUpArgs is the up command's arguments, after the script. The saved
// values passed validInputs, so none needs quoting.
func localUpArgs(s *onboardingState) string {
	if s.Shape == "lan" {
		return "up --lan " + s.Domain + " --bind " + s.Bind + " --owner " + s.Owner + " --workspace " + s.Workspace
	}
	return "up --owner " + s.Owner + " --workspace " + s.Workspace
}

// localGateArgs is the hand-over's gate, in both shapes: the stack
// publishes the api on 127.0.0.1:8443 in the LAN shape too, so the server is
// checked there, before anyone trusts the LAN certificate or resolves the
// host. --local still reads the env file and the containers.
var localGateArgs = []string{"server", "version", "--local", "--server", localSoloURL}

// localTrustArgs is shape B's informational trust check, never a gate.
var localTrustArgs = []string{"trust", "--local"}

const localControls = `
What your agent will do: load the clawbridge-provision-local playbook and follow it in your onboarding
folder. It checks Docker, gets the server's source at this ClawBridge version, and runs
deploy/local-host.sh up with your choices. It creates no cloud resources and costs nothing.
`

const localSecrets = `
The agent holds this terminal. It never sees a secret: the script generates them into its own env file,
and you make the owner's sign-in code in your own terminal after the agent exits.
`

// localTaskText is the TASK.md the agent starts from: the choices, the
// exact commands, and where to stop. It holds no secret.
func localTaskText(s *onboardingState) string {
	who, lan := "just me, on this machine (solo)", ""
	if s.Shape == "lan" {
		who = "my team, on our LAN (lan)"
		lan = "- LAN host: " + s.Domain + "\n- LAN address to publish on: " + s.Bind + "\n"
	}
	source := fmt.Sprintf(`ClawBridge's version is %s. Get the source at that release tag, in this
directory (on a re-run, reuse a clawbridge checkout already at that tag):

    git clone --depth 1 --branch %s https://github.com/jkai-chen/ClawBridge clawbridge
`, buildinfo.Version, buildinfo.Version)
	if !releaseTagPattern.MatchString(buildinfo.Version) {
		source = fmt.Sprintf(`ClawBridge's version is %s, a development build with no release tag. Stop
before step 2 and ask the user for a release tag (vX.Y.Z) to clone, or for the
path of a ClawBridge checkout to use as ./clawbridge.
`, buildinfo.Version)
	}
	trust := ""
	if s.Shape == "lan" {
		trust = `
Then run, and report verbatim too:

    clawbridge cloud check trust --local

It is expected to fail with untrusted_ca until the user trusts the server's
root certificate (or with dns if this machine doesn't resolve the LAN host).
Say so, and fix neither: trusting the root, and pointing the name at the LAN
address, are the user's sign-in steps.
`
	}
	return fmt.Sprintf(`# ClawBridge onboarding: run the server on this machine

Load the clawbridge-provision-local skill and follow its hard rules; say them to
the user first. Work through its steps 1 to 4 with these inputs:

- Who uses it: %s
- Owner email: %s
- Workspace: %s
%s- Server URL: %s

%s
Then start the server, from this directory, exactly as written: stdin from
/dev/null, never in an interactive terminal, so the script makes no sign-in
code (that code is the user's, made in their own terminal later):

    sh clawbridge/deploy/local-host.sh %s </dev/null

Finish by running, and reporting verbatim:

    clawbridge cloud check server version --local --server %s
%s
Stop there. The user does the sign-in (the skill's references/signin.md)
themselves, in their own terminal, after you exit.
`, who, s.Owner, s.Workspace, lan, localServerURL(s), source, localUpArgs(s), localSoloURL, trust)
}

// handOverLocal is handOver for this machine or the LAN.
func handOverLocal(ctx context.Context, s *onboardingState, bin, skillAgent, path string, stdout, stderr io.Writer) error {
	if localGOOS == "windows" {
		return errLocalOnWindows
	}
	if err := installPlaybook("local", skillAgent); err != nil {
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
	if err = os.WriteFile(filepath.Join(dir, "TASK.md"), []byte(localTaskText(s)), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\nStarting %s in %s. Exit it when it is done; ClawBridge then runs the checks.\n\n", bin, dir)
	if err = runAgent(ctx, bin, dir, secretenv.Scrub(os.Environ()), "Read TASK.md in this directory and follow it."); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	// Whatever the agent said or how it exited, the checks decide.
	fmt.Fprintln(stdout, "\nChecking the server…")
	err = runHandoverChecks(ctx, localGateArgs, stdout, stderr)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		fmt.Fprintln(stdout, "\nNot done yet. Run clawbridge onboard to hand over again (your agent reuses the checkout),")
		fmt.Fprintln(stdout, "or follow docs/operations/local-host.md yourself.")
		return nil
	}
	if s.Shape == "lan" {
		// Informational only: the gate above decided.
		fmt.Fprintln(stdout, "\nChecking this machine's trust in https://"+s.Domain+"…")
		_ = runHandoverChecks(ctx, localTrustArgs, stdout, stderr)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintln(stdout, "untrusted_ca here is expected until you trust the server's root: the first sign-in step below.")
	}
	s.Step = "signin"
	if err = saveOnboarding(s); err != nil {
		return err
	}
	return finishSignin(ctx, s, nil, path, stdout, stderr)
}

// localSigninText is the owner's sign-in steps with this machine's paths,
// quoted for a shell. No cloud bootstrap: that runs over SSM.
func localSigninText(s *onboardingState) (string, error) {
	base, err := onboardingDir()
	if err != nil {
		return "", err
	}
	script := shellQuote(filepath.Join(base, "provision", "clawbridge", "deploy", "local-host.sh"))
	var b strings.Builder
	b.WriteString("\nThe server is up. Last steps, in your own terminal (not your agent's):\n\n")
	n := 1
	if s.Shape == "lan" {
		fmt.Fprintf(&b, "  %d. Trust the server's certificate on this machine, then run the commands it prints for your OS:\n       sh %s trust\n", n, script)
		fmt.Fprintf(&b, "     If %s doesn't resolve on a computer, point it at %s: in your router's DNS, or with the hosts-file entry %s %s\n", s.Domain, s.Bind, s.Bind, s.Domain)
		n++
	}
	fmt.Fprintf(&b, "  %d. Make the owner's one-time sign-in code (it is shown only in your terminal):\n       sh %s invite-owner --workspace %s\n", n, script, shellQuote(s.Workspace))
	fmt.Fprintf(&b, "  %d. Join with that code:\n       clawbridge join --server %s --email %s\n", n+1, shellQuote(localServerURL(s)), shellQuote(s.Owner))
	b.WriteString("\nThen run clawbridge onboard again: it goes on to your first project and opens ClawBridge.\n")
	return b.String(), nil
}

// shellQuote quotes v for a POSIX shell.
func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }
