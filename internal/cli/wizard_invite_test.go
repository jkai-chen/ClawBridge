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
	"clawbridge/internal/devicekey"
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
	// After the code: Linear no, then yes to opening ClawBridge.
	w := newInviteWizard(t, "https://service.test\nada@example.com\nAda's laptop\ny\nn\n\n", "google", "device")
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
	for _, want := range []string{"signs in with an invite code", "Email: ada@example.com", "Key: the system key store", "not shown as you type", "Joined workspace pilot as ada@example.com", "Project: api"} {
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

// Every server that takes device keys goes straight to the invite (v1.0
// removed Google sign-in); one that takes none can't be joined.
func TestWizardOffersOnlyWhatTheServerTakes(t *testing.T) {
	for _, methods := range [][]string{{"device"}, {"google", "device"}} {
		w := newInviteWizard(t, "https://service.test\nada@example.com\n\nn\n", methods...)
		out, err := w.run()
		if err != nil || strings.Contains(out, "Google") || !strings.Contains(out, "signs in with an invite code") || len(w.joined) != 0 || w.codeRead {
			t.Fatal(methods, err, out)
		}
	}
	for _, methods := range [][]string{{"saml"}, {"google"}, nil} {
		w := newInviteWizard(t, "https://service.test\n", methods...)
		if _, err := w.run(); err == nil || !strings.Contains(err.Error(), "doesn't take device keys") {
			t.Fatal(methods, err)
		}
	}
	// Settings that can't be read: the invite is tried anyway.
	w := newInviteWizard(t, "https://service.test\nada@example.com\n\nn\n")
	wizardMethods = func(context.Context, string) ([]string, error) { return nil, errors.New("unreachable") }
	if out, err := w.run(); err != nil || !strings.Contains(out, "Couldn't read the service's sign-in settings (unreachable); trying an invite anyway") || !strings.Contains(out, "Canceled; no key or configuration changed.") {
		t.Fatal(err, out)
	}
}

// Without a system key store, the wizard explains the file store and asks;
// a no changes nothing, and a yes joins with the key in a file.
func TestWizardAsksBeforeAFileKeyStore(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\nada@example.com\n\nn\n", "google", "device")
	wizardKeyStore = func(context.Context) error { return errors.New("Linux Secret Service is unavailable") }
	out, err := w.run()
	if err != nil || !strings.Contains(out, devicesign.FileWarning) || !strings.Contains(out, "Canceled; nothing was changed") || w.codeRead || len(w.joined) != 0 {
		t.Fatal(err, out)
	}
	if _, e := os.Stat(w.path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("a profile was written")
	}
	w = newInviteWizard(t, "https://service.test\nada@example.com\n\ny\ny\n", "google", "device")
	wizardKeyStore = func(context.Context) error { return errors.New("Linux Secret Service is unavailable") }
	out, err = w.run()
	if err != nil || len(w.joined) != 1 || w.joined[0].KeyStore != devicesign.StoreFile || !strings.Contains(out, "Key: a file (0600)") || !strings.Contains(out, "Note: "+devicesign.FileWarning) {
		t.Fatal(err, out, w.joined)
	}
}

// A no at the summary reads no code and joins nothing.
func TestWizardInviteDeclineChangesNothing(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\nada@example.com\n\nn\n", "google", "device")
	out, err := w.run()
	if err != nil || w.codeRead || len(w.joined) != 0 || !strings.Contains(out, "Canceled; no key or configuration changed.") {
		t.Fatal(err, out)
	}
	w = newInviteWizard(t, "https://service.test\nnot-an-email\n", "device", "google")
	if _, err = w.run(); err == nil || w.codeRead {
		t.Fatal(err)
	}
}

// With several projects the person picks one; with none, or a failed
// signed read, the wizard says what to do and opens nothing.
func TestWizardInviteProjectChoice(t *testing.T) {
	// A numbered list: a number or a slug picks; a wrong answer asks again.
	for answer, want := range map[string]string{"2": "web", "web": "web", "": "api", "3\nother\n2": "web"} {
		w := newInviteWizard(t, "https://service.test\nada@example.com\n\ny\n"+answer+"\nn\n\n", "device", "google")
		w.projects = []string{"api", "web"}
		out, err := w.run()
		if err != nil || w.profile()["project"] != want || !strings.Contains(out, "Your projects:\n  1. api\n  2. web\n") || !w.tui {
			t.Fatalf("%q: %v %s", answer, err, out)
		}
	}
	w := newInviteWizard(t, "https://service.test\nada@example.com\n\ny\nother\n0\n9\n", "device", "google")
	w.projects = []string{"api", "web"}
	if _, err := w.run(); err == nil || !strings.Contains(err.Error(), "choose a number from 1 to 2") || w.tui {
		t.Fatal(err)
	}
	// U47: with none, the profile is kept, the person is told what it waits
	// for, and ClawBridge opens on its waiting screen (PR D); not an error.
	w = newInviteWizard(t, "https://service.test\nada@example.com\n\ny\n\n", "device", "google")
	w.projects = nil
	out, err := w.run()
	if err != nil || !strings.Contains(out, "no project in this workspace yet") || !strings.Contains(out, "waiting screen") || !w.tui || w.profile()["project"] != nil || w.profile()["workspace"] != "pilot" {
		t.Fatal(err, out)
	}
	w = newInviteWizard(t, "https://service.test\nada@example.com\n\ny\n", "device", "google")
	wizardProjects = func(context.Context, string) ([]string, error) { return nil, errors.New("401") }
	if _, err = w.run(); err == nil || !strings.Contains(err.Error(), "signed with the new key failed") || w.tui {
		t.Fatal(err)
	}
}

// A lost join answer is the join's own error, which says the key was kept;
// nothing after it runs.
func TestWizardInviteUncertainJoinStops(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\nada@example.com\n\ny\n", "device", "google")
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
	r := wizardHooks(t, f.url+"\nada@example.com\nAda's laptop\ny\nn\n\n")
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
	if len(f.exec.requests) < 1 || f.exec.requests[0].Operation != "list_projects" || f.exec.ids[0].Issuer != bus.DeviceIssuer || f.exec.ids[0].Subject != f.keys[0] {
		t.Fatalf("%+v %+v", f.exec.ids, f.exec.requests)
	}
	// After it, only the dispatcher offer's reads (G5): nothing is written.
	for _, r := range f.exec.requests[1:] {
		if r.Operation != "resolve_project" && r.Operation != "dispatcher_get" {
			t.Fatalf("onboarding asked for %s", r.Operation)
		}
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
	// The Model screen is Dispatcher now; both names open it.
	for _, name := range []string{"dispatcher", "model"} {
		if err := runTUI(context.Background(), Config{}, []string{"--screen", name}, io.Discard, io.Discard); err == nil || err.Error() != "terminal checked" {
			t.Fatal(name, err)
		}
	}
}

// R108-L3: a failed code read says why.
func TestWizardInviteCodeReadFailureIsShown(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\nada@example.com\n\ny\n", "device", "google")
	wizardInviteCode = func() (string, error) { return "", errors.New("inappropriate ioctl for device") }
	if _, err := w.run(); err == nil || !strings.Contains(err.Error(), "inappropriate ioctl") || len(w.joined) != 0 {
		t.Fatal(err)
	}
}

// The code field takes the whole invite link (§17.1): with no email typed,
// the link's email is used; a link for another server or email is refused
// and nothing is joined.
func TestWizardTakesAnInviteLink(t *testing.T) {
	link, err := devicekey.FormatLink("https://service.test", "pilot", "ada@example.com", wizardCode)
	if err != nil {
		t.Fatal(err)
	}
	w := newInviteWizard(t, "https://service.test\n\nAda's laptop\ny\n", "google", "device")
	wizardInviteCode = func() (string, error) { return link, nil }
	out, err := w.run()
	if err != nil || len(w.joined) != 1 || w.joined[0].Email != "ada@example.com" || w.joined[0].Code != "CBJ-"+strings.ReplaceAll(strings.TrimPrefix(wizardCode, "CBJ-"), "-", "") || !strings.Contains(out, "Email: (from the invite link)") || strings.Contains(out, "CBJ-") {
		t.Fatal(err, out, w.joined)
	}
	other, _ := devicekey.FormatLink("https://elsewhere.test", "pilot", "ada@example.com", wizardCode)
	for _, c := range []struct{ input, link, want string }{
		{"https://service.test\n\n\ny\n", other, "for https://elsewhere.test"},
		{"https://service.test\nbob@example.com\n\ny\n", link, "for ada@example.com, not bob@example.com"},
		{"https://service.test\n\n\ny\n", wizardCode, "paste the whole invite link"},
	} {
		w = newInviteWizard(t, c.input, "google", "device")
		pasted := c.link
		wizardInviteCode = func() (string, error) { return pasted, nil }
		if _, err = w.run(); err == nil || !strings.Contains(err.Error(), c.want) || len(w.joined) != 0 {
			t.Fatal(c.want, err)
		}
	}
}

// TUI-UX-AUDIT PR D, R175-M1: the TUI waits for a project only when the
// signed project list answers; a revoked device, a server down or a lost
// membership fails as before, and never says "ask the owner".
func TestTUIWaitsOnlyForAMissingProject(t *testing.T) {
	failed := errors.New("project not resolved")
	listed := func() error { return nil }
	for _, c := range []struct {
		name                              string
		err                               error
		mode, workspace, flag, configured string
		list                              func() error
		wait                              bool
	}{
		{"no project yet", failed, "cloud", "acme", "", "", listed, true},
		{"resolved", nil, "cloud", "acme", "", "", listed, false},
		{"local", failed, "local", "acme", "", "", listed, false},
		{"no workspace", failed, "cloud", "", "", "", listed, false},
		{"--project", failed, "cloud", "acme", "api", "", listed, false},
		{"saved project", failed, "cloud", "acme", "", "api", listed, false},
		{"device revoked", failed, "cloud", "acme", "", "", func() error { return errors.New("this device was revoked") }, false},
		{"server down", failed, "cloud", "acme", "", "", func() error { return errors.New("503 Service Unavailable") }, false},
		{"tls or dns", failed, "cloud", "acme", "", "", func() error { return errors.New("x509: certificate signed by unknown authority") }, false},
		{"membership removed", failed, "cloud", "acme", "", "", func() error { return errors.New("workspace membership required") }, false},
		{"offline copy", failed, "cloud", "acme", "", "", func() error { return errors.New("offline") }, false},
	} {
		if got := waitForProject(c.err, c.mode, c.workspace, c.flag, c.configured, c.list); got != c.wait {
			t.Errorf("%s: waited %v", c.name, got)
		}
	}
}

// R175-L1: only a slug reaches the profile.
func TestFoundProjectMustBeASlug(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("mode = \"cloud\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "-x", "a b", "a\"\nmode = \"local", "../x", strings.Repeat("a", 129)} {
		if err := saveFoundProject(path, bad); err == nil {
			t.Fatalf("saved %q", bad)
		}
	}
	if err := saveFoundProject(path, "api-v2"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "api-v2") || strings.Contains(string(raw), "local") {
		t.Fatalf("%s", raw)
	}
}

type listExecutor struct {
	source string
	err    error
	got    bus.Request
}

func (e *listExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	e.got = r
	return bus.Result{Source: e.source}, e.err
}

func (e *listExecutor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "", "", errors.New("unused")
}

// The list that decides is the server's own, for this workspace.
func TestSignedProjectList(t *testing.T) {
	ok := &listExecutor{source: "server"}
	if err := signedProjectList(context.Background(), ok, "acme"); err != nil || ok.got.Operation != "list_projects" || ok.got.Workspace != "acme" {
		t.Fatal(err, ok.got)
	}
	if err := signedProjectList(context.Background(), &listExecutor{source: "offline-mirror"}, "acme"); err == nil {
		t.Fatal("a cached list counted")
	}
	if err := signedProjectList(context.Background(), &listExecutor{err: errors.New("revoked")}, "acme"); err == nil {
		t.Fatal("a refusal counted")
	}
}
