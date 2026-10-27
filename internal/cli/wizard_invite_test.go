package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/remote"
	"github.com/pelletier/go-toml/v2"
)

const wizardCode = "CBJ-4K7P-WQ9D-2MXH-T6RB"

// inviteWizard runs the wizard with input against a server offering
// methods, and a join that writes a device profile as JoinDevice does.
type inviteWizard struct {
	t        *testing.T
	r        *wizardReader
	path     string
	joined   []JoinParams
	codeRead bool
	tui      bool
	projects []string
}

func newInviteWizard(t *testing.T, input string, methods ...string) *inviteWizard {
	t.Helper()
	isolated(t)
	w := &inviteWizard{t: t, r: wizardHooks(t, input), path: filepath.Join(t.TempDir(), "client.toml"), projects: []string{"api"}}
	wizardMethods = func(_ context.Context, server string) ([]string, error) {
		if server != "https://service.test" {
			t.Fatal("discovery of", server)
		}
		return methods, nil
	}
	wizardInviteCode = func() (string, error) {
		if !w.r.closed {
			t.Fatal("the code was read while the prompts held the terminal")
		}
		w.codeRead = true
		return wizardCode, nil
	}
	wizardDeviceJoin = func(_ context.Context, p JoinParams) (JoinResult, error) {
		w.joined = append(w.joined, p)
		route := map[string]any{"mode": "cloud", "server": p.Server, "workspace": "pilot", "principal": p.Email, "credential_source": "device", "device_id": "dev_1", "key_store": p.KeyStore, "clock_offset_seconds": 3}
		b, _ := toml.Marshal(route)
		if err := os.WriteFile(p.ConfigPath, b, 0o600); err != nil {
			t.Fatal(err)
		}
		out := JoinResult{DeviceID: "dev_1", Principal: p.Email, Workspace: "pilot", Label: p.Label, KeyStore: p.KeyStore, ConfigPath: p.ConfigPath}
		if p.KeyStore == devicesign.StoreFile {
			out.Warning = devicesign.FileWarning
		}
		return out, nil
	}
	wizardProjects = func(_ context.Context, path string) ([]string, error) {
		if path != w.path {
			t.Fatal("projects read from", path)
		}
		return w.projects, nil
	}
	wizardTUI = func(_ context.Context, c Config, _, _ io.Writer) error {
		if c.ClientConfig != w.path {
			t.Fatal("selector lost")
		}
		w.tui = true
		return nil
	}
	return w
}

func (w *inviteWizard) run() (string, error) {
	var out bytes.Buffer
	err := Run(context.Background(), []string{"--client-config", w.path, "setup", "wizard"}, &out, io.Discard)
	return out.String(), err
}

func (w *inviteWizard) profile() map[string]any {
	w.t.Helper()
	values := map[string]any{}
	b, err := os.ReadFile(w.path)
	if err != nil {
		w.t.Fatal(err)
	}
	if err = toml.Unmarshal(b, &values); err != nil {
		w.t.Fatal(err)
	}
	return values
}

// With an invite code: the email and device name, a summary, a yes, and
// only then the code, unechoed; the join, the first signed read, the
// project saved beside the device route, onboarding and the TUI.
func TestWizardJoinsWithAnInviteCode(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\n1\nada@example.com\nAda's laptop\ny\n", "google", "device")
	out, err := w.run()
	if err != nil {
		t.Fatal(err, out)
	}
	if len(w.joined) != 1 {
		t.Fatal(w.joined)
	}
	p := w.joined[0]
	if p.Server != "https://service.test" || p.Email != "ada@example.com" || p.Code != wizardCode || p.Label != "Ada's laptop" || p.KeyStore != devicesign.StoreKeyring || p.ConfigPath != w.path {
		t.Fatalf("%+v", p)
	}
	for _, want := range []string{"1. I have an invite code", "2. Sign in with Google", "Email: ada@example.com", "Key: the system key store", "not shown as you type", "Joined workspace pilot as ada@example.com", "Project: api"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, wizardCode) {
		t.Fatal("the code was printed")
	}
	got := w.profile()
	if got["project"] != "api" || got["device_id"] != "dev_1" || got["clock_offset_seconds"] != int64(3) {
		t.Fatal("profile:", got)
	}
	if !w.tui {
		t.Fatal("the TUI didn't open")
	}
}

// A server that takes only device keys goes straight to the invite; one
// that takes both still offers Google, which keeps the old path.
func TestWizardOffersOnlyWhatTheServerTakes(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\nada@example.com\n\nn\n", "device")
	out, err := w.run()
	if err != nil || strings.Contains(out, "Sign in with Google") || !strings.Contains(out, "signs in with an invite code") || len(w.joined) != 0 || w.codeRead {
		t.Fatal(err, out)
	}
	w = newInviteWizard(t, "https://service.test\n2\nws\nprj\ndevice\nn\n", "google", "device")
	out, err = w.run()
	if err != nil || !strings.Contains(out, "Sign-in: device") || !strings.Contains(out, "Canceled; no credentials or configuration changed.") || len(w.joined) != 0 {
		t.Fatal(err, out)
	}
	w = newInviteWizard(t, "https://service.test\n", "saml")
	if _, err = w.run(); err == nil || !strings.Contains(err.Error(), "no sign-in this client supports") {
		t.Fatal(err)
	}
	// Settings that can't be read offer both ways in.
	w = newInviteWizard(t, "https://service.test\n2\nws\nprj\ndevice\nn\n")
	wizardMethods = func(context.Context, string) ([]string, error) { return nil, errors.New("unreachable") }
	if out, err = w.run(); err != nil || !strings.Contains(out, "Couldn't read the service's sign-in settings (unreachable)") || !strings.Contains(out, "1. I have an invite code") || !strings.Contains(out, "Canceled; no credentials") {
		t.Fatal(err, out)
	}
}

// Without a system key store, the wizard explains the file store and asks;
// a no changes nothing, and a yes joins with the key in a file.
func TestWizardAsksBeforeAFileKeyStore(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\n1\nada@example.com\n\nn\n", "google", "device")
	wizardKeyStore = func(context.Context) error { return errors.New("Linux Secret Service is unavailable") }
	out, err := w.run()
	if err != nil || !strings.Contains(out, devicesign.FileWarning) || !strings.Contains(out, "Canceled; nothing was changed") || w.codeRead || len(w.joined) != 0 {
		t.Fatal(err, out)
	}
	if _, e := os.Stat(w.path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("a profile was written")
	}
	w = newInviteWizard(t, "https://service.test\n1\nada@example.com\n\ny\ny\n", "google", "device")
	wizardKeyStore = func(context.Context) error { return errors.New("Linux Secret Service is unavailable") }
	out, err = w.run()
	if err != nil || len(w.joined) != 1 || w.joined[0].KeyStore != devicesign.StoreFile || !strings.Contains(out, "Key: a file (0600)") || !strings.Contains(out, "Note: "+devicesign.FileWarning) {
		t.Fatal(err, out, w.joined)
	}
}

// A no at the summary reads no code and joins nothing.
func TestWizardInviteDeclineChangesNothing(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\n1\nada@example.com\n\nn\n", "google", "device")
	out, err := w.run()
	if err != nil || w.codeRead || len(w.joined) != 0 || !strings.Contains(out, "Canceled; no key or configuration changed.") {
		t.Fatal(err, out)
	}
	w = newInviteWizard(t, "https://service.test\n1\nnot-an-email\n", "device", "google")
	if _, err = w.run(); err == nil || w.codeRead {
		t.Fatal(err)
	}
}

// With several projects the person picks one; with none, or a failed
// signed read, the wizard says what to do and opens nothing.
func TestWizardInviteProjectChoice(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\n1\nada@example.com\n\ny\nweb\n", "device", "google")
	w.projects = []string{"api", "web"}
	out, err := w.run()
	if err != nil || w.profile()["project"] != "web" || !strings.Contains(out, "Your projects: api, web") {
		t.Fatal(err, out)
	}
	w = newInviteWizard(t, "https://service.test\n1\nada@example.com\n\ny\nother\n", "device", "google")
	w.projects = []string{"api", "web"}
	if _, err = w.run(); err == nil || !strings.Contains(err.Error(), "not one of your projects") || w.tui {
		t.Fatal(err)
	}
	w = newInviteWizard(t, "https://service.test\n1\nada@example.com\n\ny\n", "device", "google")
	w.projects = nil
	if _, err = w.run(); err == nil || !strings.Contains(err.Error(), "no project in this workspace yet") || w.tui {
		t.Fatal(err)
	}
	w = newInviteWizard(t, "https://service.test\n1\nada@example.com\n\ny\n", "device", "google")
	wizardProjects = func(context.Context, string) ([]string, error) { return nil, errors.New("401") }
	if _, err = w.run(); err == nil || !strings.Contains(err.Error(), "signed with the new key failed") || w.tui {
		t.Fatal(err)
	}
}

// A lost join answer is the join's own error, which says the key was kept;
// nothing after it runs.
func TestWizardInviteUncertainJoinStops(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\n1\nada@example.com\n\ny\n", "device", "google")
	wizardDeviceJoin = func(context.Context, JoinParams) (JoinResult, error) {
		return JoinResult{}, &JoinUncertainError{DeviceID: "dev_1"}
	}
	wizardProjects = func(context.Context, string) ([]string, error) { t.Fatal("read after a lost join"); return nil, nil }
	_, err := w.run()
	if !errors.Is(err, remote.ErrJoinUncertain) || w.tui {
		t.Fatal(err)
	}
}

// End to end: the wizard's invite path reads the real server's sign-in
// settings, joins through the real handler, and its first signed request
// (the project list) comes from the new device as its person.
func TestWizardInviteJoinsARealServer(t *testing.T) {
	f := newJoinFixture(t)
	methods, join, projects := wizardMethods, wizardDeviceJoin, wizardProjects
	path := filepath.Join(t.TempDir(), "client.toml")
	r := wizardHooks(t, f.url+"\nada@example.com\nAda's laptop\ny\n")
	wizardMethods, wizardDeviceJoin, wizardProjects = methods, join, projects
	wizardInviteCode = func() (string, error) {
		if !r.closed {
			t.Fatal("the code was read while the prompts held the terminal")
		}
		return "cbj-4k7p-wq9d-2mxh-t6rb", nil
	}
	opened := false
	wizardTUI = func(context.Context, Config, io.Writer, io.Writer) error { opened = true; return nil }
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"--client-config", path, "setup", "wizard"}, &out, io.Discard); err != nil {
		t.Fatal(err, out.String())
	}
	p := profile(t, path)
	if len(f.keys) != 1 || p["device_id"] != f.keys[0] || p["workspace"] != "acme" || p["project"] != "api" || p["credential_source"] != "device" || !opened {
		t.Fatal(p, out.String())
	}
	if len(f.exec.requests) != 1 || f.exec.requests[0].Operation != "list_projects" || f.exec.ids[0].Issuer != bus.DeviceIssuer || f.exec.ids[0].Subject != f.keys[0] {
		t.Fatalf("%+v %+v", f.exec.ids, f.exec.requests)
	}
}

// R108-M1: the TUI's Account screen can be opened directly.
func TestTUIOpensTheAccountScreen(t *testing.T) {
	old := checkTUITerminal
	t.Cleanup(func() { checkTUITerminal = old })
	checkTUITerminal = func(io.Reader, io.Writer) error { return errors.New("terminal checked") }
	if err := runTUI(context.Background(), Config{}, []string{"--screen", "account"}, io.Discard, io.Discard); err == nil || err.Error() != "terminal checked" {
		t.Fatal(err)
	}
	if err := runTUI(context.Background(), Config{}, []string{"--screen", "accounts"}, io.Discard, io.Discard); err == nil || err.Error() != "invalid TUI screen" {
		t.Fatal(err)
	}
}

// R108-L3: a failed code read says why.
func TestWizardInviteCodeReadFailureIsShown(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\n1\nada@example.com\n\ny\n", "device", "google")
	wizardInviteCode = func() (string, error) { return "", errors.New("inappropriate ioctl for device") }
	if _, err := w.run(); err == nil || !strings.Contains(err.Error(), "inappropriate ioctl") || len(w.joined) != 0 {
		t.Fatal(err)
	}
}
