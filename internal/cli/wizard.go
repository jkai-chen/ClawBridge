package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"clawbridge/internal/auth"
	"clawbridge/internal/clirender"
	"clawbridge/internal/setup"
	"github.com/muesli/cancelreader"
)

type guidedRouteKey struct{}

// Adapter seams keep terminal/credential/authority boundaries hermetic in tests.
var wizardTerminal = func(out io.Writer) error { return checkTUITerminal(os.Stdin, out) }
var wizardInput = func() (cancelreader.CancelReader, error) { return newWizardInput(runtime.GOOS, os.Stdin) }

// newWizardInput opens the guided prompts' reader. On Windows, cancelreader
// switches a console *os.File to raw mode (no echo, Enter sends a lone CR,
// Ctrl+C arrives as a byte), so it is given a plain reader instead: its
// fallback leaves the console's normal line editing in place, at the cost of
// not interrupting a blocked read, which ask already treats as unconfirmed.
func newWizardInput(goos string, stdin *os.File) (cancelreader.CancelReader, error) {
	if goos == "windows" {
		return cancelreader.NewReader(struct{ io.Reader }{stdin})
	}
	return cancelreader.NewReader(stdin)
}

var wizardKeyring = auth.CheckNativeAvailability
var wizardJoin = runJoin
var wizardTUI = func(ctx context.Context, c Config, out, errout io.Writer) error {
	return runTUI(ctx, c, nil, out, errout)
}

func selectedClientPath(c Config) (string, error) {
	p := firstConfigPath(c.ClientConfig, os.Getenv("CLAWBRIDGE_CONFIG"))
	if p == "" {
		home, e := stateHome()
		if e != nil {
			return "", e
		}
		p = filepath.Join(home, "config.toml")
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("trusted client config must use an absolute path")
	}
	return filepath.Clean(p), nil
}

func selectorOnly(args []string) bool {
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--client-config=") {
			continue
		}
		if args[i] == "--client-config" {
			i++
			if i < len(args) {
				continue
			}
		}
		return false
	}
	return true
}

func runStartup(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	if !selectorOnly(args) {
		return errors.New("without a command only --client-config is accepted; use help")
	}
	if wizardTerminal(stdout) != nil {
		// With no saved profile, Start prints its text and commands.
		if path, e := selectedClientPath(flags); e == nil {
			if _, e = os.Lstat(path); errors.Is(e, os.ErrNotExist) {
				return runStart(ctx, path, stdout, stderr)
			}
		}
		_, e := io.WriteString(stdout, Usage)
		return e
	}
	path, err := selectedClientPath(flags)
	if err != nil {
		return err
	}
	stat, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return runStart(ctx, path, stdout, stderr)
	}
	// U53: a broken profile says what is wrong and how to start again.
	shown := clirender.Safe(path)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 4<<20 {
		return fmt.Errorf("your ClawBridge settings at %s can't be used: they must be a plain file (not a link or a folder) under 4 MB. Move it aside and run clawbridge onboard to set up again, or choose another file with --client-config", shown)
	}
	c, err := readConfig(path, true)
	if err != nil {
		return fmt.Errorf("your ClawBridge settings at %s are damaged and can't be read. Repair the file, or move it aside and run clawbridge onboard to set up again", shown)
	}
	if c.Mode == "local" && c.Workspace != "" && c.Project == "" {
		return errors.New("this local workspace has no default project yet: run clawbridge project create NAME (the first project becomes the default), then clawbridge again")
	}
	// A member with no project yet: the TUI waits for one (U47, PR D).
	waiting := c.Mode == "cloud" && c.Workspace != "" && c.Project == ""
	var missing []string
	for _, f := range [][2]string{{"mode", c.Mode}, {"workspace", c.Workspace}, {"project", c.Project}} {
		if f[1] == "" {
			missing = append(missing, f[0])
		}
	}
	if len(missing) > 0 && !waiting {
		return fmt.Errorf("your ClawBridge settings at %s are incomplete (missing: %s). Move the file aside and run clawbridge onboard to set up again, or join with clawbridge setup join", shown, strings.Join(missing, ", "))
	}
	return wizardTUI(context.WithValue(ctx, guidedRouteKey{}, true), Config{ClientConfig: path}, stdout, stderr)
}

// finishJoin saves the chosen project, then the optional setup and the
// offer to open ClawBridge.
func finishJoin(ctx context.Context, path, project string, stdout, stderr io.Writer) error {
	if err := setProfileValue(path, "project", project); err != nil {
		return fmt.Errorf("joined, but the project could not be saved; set project = %q in %s: %w", project, path, err)
	}
	isolated := context.WithValue(ctx, guidedRouteKey{}, true)
	if err := runOnboarding(isolated, path, stdout, stderr); err != nil {
		return fmt.Errorf("joined; saved route preserved, but guided setup stopped: %w", err)
	}
	return openNow(isolated, path, stdout, stderr)
}

func runWizard(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) (retErr error) {
	_, err := commandFlags("setup wizard", args, stderr, func(*flag.FlagSet) {})
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if flags != (Config{ClientConfig: flags.ClientConfig}) {
		return errors.New("setup wizard accepts only --client-config; enter routing in the prompts")
	}
	if wizardTerminal(stdout) != nil {
		return errors.New("setup wizard requires terminal stdin and stdout; use setup join with explicit flags for automation")
	}
	path, err := selectedClientPath(flags)
	if err != nil {
		return err
	}
	prompt, err := newWizardPrompt(ctx, stdout, "guided terminal input cleanup failed; no OAuth acquisition or join was started, restore the terminal before retrying")
	if err != nil {
		return err
	}
	defer func() {
		if e := prompt.close(); e != nil && !errors.Is(retErr, e) {
			retErr = errors.Join(retErr, e)
		}
	}()
	ask := prompt.ask
	fmt.Fprintln(stdout, "Join an existing ClawBridge service. No infrastructure is created.")
	// Each answer is checked as it is given (U46).
	url, err := askUntil(ask, stdout, "Service HTTPS URL: ", func(a string) (string, error) {
		switch {
		case a == "":
			return "", errors.New("the service URL is required, like https://clawbridge.example.com")
		case setup.ValidateJoinTarget(path, a, "") != nil:
			return "", errors.New("that is not a valid service URL, or these settings already belong to another service (choose a separate --client-config)")
		}
		return a, nil
	})
	if err != nil {
		return err
	}
	methods, e := wizardMethods(ctx, url)
	if e != nil {
		// Each way in reports its own failure once chosen; nothing is
		// written before the confirmation.
		fmt.Fprintf(stdout, "Couldn't read the service's sign-in settings (%s); both ways in are offered.\n", clirender.Safe(e.Error()))
		methods = []string{"device", "google"}
	}
	invite, e := wizardSignIn(ask, methods, stdout)
	if e != nil {
		return e
	}
	if invite {
		return wizardInvite(ctx, prompt, path, url, stdout, stderr)
	}
	required := func(what string) func(string) (string, error) {
		return func(a string) (string, error) {
			if a == "" {
				return "", errors.New("the " + what + " is required")
			}
			return a, nil
		}
	}
	workspace, err := askUntil(ask, stdout, "Workspace: ", required("workspace"))
	if err != nil {
		return err
	}
	project, err := askUntil(ask, stdout, "Project: ", required("project"))
	if err != nil {
		return err
	}
	// U49: "device" here was OAuth's device flow, not a device key.
	fmt.Fprintln(stdout, "Sign in:\n  1. open a browser here\n  2. show a code to enter on another device (for SSH)")
	flow, err := askUntil(ask, stdout, "Choose 1 or 2 [1]: ", func(a string) (string, error) {
		if v, ok := pick(a, "1", signInChoices); ok {
			return v, nil
		}
		return "", errors.New("choose 1 or 2")
	})
	if err != nil {
		return err
	}
	how := map[string]string{"browser": "a browser here", "device": "a code on another device"}[flow]
	fmt.Fprintf(stdout, "Service: %s\nWorkspace: %s\nProject: %s\nConfig: %s\nSign-in: %s\n", clirender.Safe(url), clirender.Safe(workspace), clirender.Safe(project), clirender.Safe(path), how)
	answer, err := ask("Continue with sign-in and save after verified join? [y/N]: ")
	if err != nil {
		return err
	}
	if answer != "y" && answer != "yes" {
		fmt.Fprintln(stdout, "Canceled; no credentials or configuration changed.")
		return nil
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = prompt.close(); err != nil {
		return err
	} // No wizard reader survives into acquisition or the TUI.
	if err = wizardKeyring(ctx); err != nil {
		return err
	}
	fmt.Fprintln(stdout, credentialStoreNote(runtime.GOOS))
	candidate := Config{ClientConfig: path, Mode: "cloud", CredentialSource: "oauth", Server: url, Workspace: workspace, Project: project}
	joinArgs := []string{"--login"}
	if flow == "device" {
		joinArgs = []string{"--device"}
	}
	isolated := context.WithValue(ctx, guidedRouteKey{}, true)
	if err = wizardJoin(isolated, candidate, joinArgs, io.Discard, stderr); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Join completed and trusted route saved.")
	if err = runOnboarding(isolated, path, stdout, stderr); err != nil {
		return fmt.Errorf("join completed; saved route preserved, but guided setup stopped: %w", err)
	}
	return openNow(isolated, path, stdout, stderr)
}

// wizardPrompt reads answers from a cancellable terminal reader. Close it
// before any credential acquisition or the TUI, so neither shares stdin.
type wizardPrompt struct {
	ctx      context.Context
	out      io.Writer
	input    cancelreader.CancelReader
	closeMsg string
	closed   bool
	closeErr error
}

func newWizardPrompt(ctx context.Context, out io.Writer, closeMsg string) (*wizardPrompt, error) {
	input, err := wizardInput()
	if err != nil {
		return nil, errors.New("cannot open cancellable terminal input")
	}
	return &wizardPrompt{ctx: ctx, out: out, input: input, closeMsg: closeMsg}, nil
}

func (p *wizardPrompt) close() error {
	if !p.closed {
		p.closed = true // A failed native close may already have closed handles.
		if p.input.Close() != nil {
			p.closeErr = errors.New(p.closeMsg)
		}
	}
	return p.closeErr
}

func (p *wizardPrompt) ask(prompt string) (string, error) {
	if err := p.ctx.Err(); err != nil {
		return "", err
	}
	fmt.Fprint(p.out, prompt)
	type answer struct {
		text string
		err  error
	}
	done := make(chan answer, 1)
	go func() { text, e := readWizardLine(p.input); done <- answer{text, e} }()
	select {
	case <-p.ctx.Done():
		if !p.input.Cancel() {
			p.close()
		}
		select {
		case <-done:
			return "", p.ctx.Err()
		case <-time.After(time.Second):
			p.close()
			return "", errors.New("guided setup canceled; terminal input cleanup could not be confirmed, restart the command before retrying")
		}
	case a := <-done:
		return strings.TrimSpace(a.text), a.err
	}
}

// wizardAfterCR records that the last line ended with a CR, so the LF of a
// CRLF pair is not read as an empty answer to the next prompt. It is shared
// because every prompt reads the same stdin, and prompts are reopened.
var wizardAfterCR atomic.Bool

// Read one byte at a time so no buffered read consumes later TUI keystrokes.
// A line ends at LF, CR or CRLF.
func readWizardLine(r io.Reader) (string, error) {
	var b strings.Builder
	var one [1]byte
	for b.Len() <= 4096 {
		n, err := r.Read(one[:])
		if n > 0 {
			afterCR := wizardAfterCR.Swap(false)
			switch {
			case one[0] == '\n' && afterCR && b.Len() == 0:
				continue
			case one[0] == '\n':
				return b.String(), nil
			case one[0] == '\r':
				wizardAfterCR.Store(true)
				return b.String(), nil
			}
			b.WriteByte(one[0])
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("guided input exceeds 4096 bytes")
}

// credentialStoreNote says, in plain words, where the sign-in is kept and that
// the system may ask first.
func credentialStoreNote(goos string) string {
	if goos == "darwin" {
		return "ClawBridge keeps your sign-in in the macOS Keychain. macOS may ask you to allow access; choose Allow or Always Allow."
	}
	return "ClawBridge keeps your sign-in in your system's credential store. You may be asked to unlock it or allow access."
}
