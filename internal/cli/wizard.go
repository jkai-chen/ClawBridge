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
	"unicode"

	"clawbridge/internal/auth"
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
		_, e := io.WriteString(stdout, Usage)
		return e
	}
	path, err := selectedClientPath(flags)
	if err != nil {
		return err
	}
	stat, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return runWizard(ctx, Config{ClientConfig: path}, nil, stdout, stderr)
	}
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 4<<20 {
		return errors.New("trusted config is inaccessible or not a regular nonsymlink file; choose a separate --client-config")
	}
	c, err := readConfig(path, true)
	if err != nil {
		return errors.New("trusted config is malformed; repair it or choose a separate --client-config")
	}
	if c.Mode == "" || c.Workspace == "" || c.Project == "" {
		return errors.New("trusted config is incomplete; use explicit setup join or repair the selected profile")
	}
	return wizardTUI(context.WithValue(ctx, guidedRouteKey{}, true), Config{ClientConfig: path}, stdout, stderr)
}

func wizardSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || r == 0x061c || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
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
	fmt.Fprintln(stdout, "Join an existing ClawBridge service (OAuth). No infrastructure or membership is created.")
	url, err := ask("Service HTTPS URL: ")
	if err != nil {
		return err
	}
	workspace, err := ask("Workspace: ")
	if err != nil {
		return err
	}
	project, err := ask("Project: ")
	if err != nil {
		return err
	}
	if url == "" || workspace == "" || project == "" {
		return errors.New("service URL, workspace and project are required")
	}
	flow, err := ask("Sign-in browser/device [browser] (device recommended for SSH): ")
	if err != nil {
		return err
	}
	if flow == "" {
		flow = "browser"
	}
	if flow != "browser" && flow != "device" {
		return errors.New("choose browser or device")
	}
	if err = setup.ValidateJoinTarget(path, url, ""); err != nil {
		return errors.New("invalid service URL or incompatible trusted target; use a valid service and separate profile when needed")
	}
	fmt.Fprintf(stdout, "Service: %s\nWorkspace: %s\nProject: %s\nConfig: %s\nSign-in: %s\n", wizardSafe(url), wizardSafe(workspace), wizardSafe(project), wizardSafe(path), flow)
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
	fmt.Fprintln(stdout, "Native credential storage is required; availability, unlock, and write readiness are not guaranteed.")
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
	fmt.Fprintln(stdout, "Opening TUI…")
	if err = wizardTUI(isolated, Config{ClientConfig: path}, stdout, stderr); err != nil {
		return fmt.Errorf("join completed; saved route preserved, but TUI startup failed: %w", err)
	}
	return nil
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
