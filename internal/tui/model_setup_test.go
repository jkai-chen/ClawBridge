package tui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/dispatchermodel"
	tea "github.com/charmbracelet/bubbletea"
)

const modelTestKey = "sk-ant-api03-SECRETSECRETSECRET1234"

// modelAuthority answers the model operations as the server would for a
// project owner (or, with denied, for anyone else).
type modelAuthority struct {
	mu       sync.Mutex
	denied   bool
	noSeal   bool
	outcomes []string
	fail     error
	calls    []bus.Request
}

func (a *modelAuthority) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, r)
	if a.denied {
		return bus.Result{}, errors.New("project owner permission required")
	}
	switch r.Operation {
	case "dispatcher_model_doctor":
		return bus.Result{Source: "server", Data: map[string]any{"seal_configured": !a.noSeal, "model_on": false, "holders": []any{
			map[string]any{"principal": "human@example.test", "you": true, "key": "active", "config_revision": int64(2), "agent": "running"},
		}}}, nil
	case "dispatcher_model_setup":
		if a.fail != nil {
			return bus.Result{}, a.fail
		}
		return bus.Result{Source: "server", Data: map[string]any{"config_revision": int64(3), "test": "pending"}}, nil
	case "dispatcher_model_status":
		outcome := a.outcomes[0]
		if len(a.outcomes) > 1 {
			a.outcomes = a.outcomes[1:]
		}
		return bus.Result{Source: "server", Data: map[string]any{"config_revision": int64(3), "test": outcome}}, nil
	}
	return bus.Result{}, errors.New("unexpected " + r.Operation)
}

func (a *modelAuthority) ops() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []string{}
	for _, r := range a.calls {
		out = append(out, r.Operation)
	}
	return out
}

func (a *modelAuthority) setup() (bus.Request, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.calls {
		if r.Operation == "dispatcher_model_setup" {
			return r, true
		}
	}
	return bus.Request{}, false
}

func modelScreen(t *testing.T, a *modelAuthority) Model {
	t.Helper()
	old := modelTestPoll
	modelTestPoll = time.Millisecond
	t.Cleanup(func() { modelTestPoll = old })
	m, err := New(testConfig(a))
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(140, 60)
	m, cmd := m.Show(ScreenModel)
	return settle(t, m, cmd)
}

// framed presses keys one by one and keeps every frame drawn on the way.
func framed(t *testing.T, m Model, frames *[]string, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		m = press(t, m, k)
		*frames = append(*frames, Render(m))
	}
	return m
}

// An owner sees their status, then sets up in the CLI's order. The key is
// in no frame, the one setup request is exact, and once it is sent the
// typed key is zeroed where it lay.
func TestModelSetupScreen(t *testing.T) {
	a := &modelAuthority{outcomes: []string{"pending", "active"}}
	m := modelScreen(t, a)
	frames := []string{Render(m)}
	if s := m.ModelSetupState(); !s.Owner || s.Step != "" {
		t.Fatalf("%+v", s)
	}
	for _, want := range []string{"Dispatcher model", "6 Dispatcher", "active (revision 2)", "running", "s set up the model"} {
		if !strings.Contains(frames[0], want) {
			t.Fatalf("no %q in %s", want, frames[0])
		}
	}
	m = framed(t, m, &frames, "s", "enter")
	// The model step lists the agent's catalogue for the provider.
	shown, more := dispatchermodel.ModelHints("anthropic")
	for _, want := range []string{"1. " + dispatchermodel.HintLine(shown[0]), dispatchermodel.HintsNote(more)} {
		if !strings.Contains(frames[len(frames)-1], want) {
			t.Fatalf("no %q in\n%s", want, frames[len(frames)-1])
		}
	}
	m = framed(t, m, &frames, strings.Split("claude-sonnet-4-5", "")...)
	m = framed(t, m, &frames, "enter", modelTestKey)
	typedKey := m.modelSetup.wizard.key
	if s := m.ModelSetupState(); s.Step != "key" || !s.KeyEntered || string(typedKey) != modelTestKey {
		t.Fatalf("%+v", s)
	}
	// The caps: runs per day 40, the ceiling 500,000; the rest kept.
	m = framed(t, m, &frames, "enter", "enter", "4", "0", "enter", "enter", "enter")
	m = framed(t, m, &frames, strings.Split("500,000", "")...)
	m = framed(t, m, &frames, "enter")
	if s := m.ModelSetupState(); s.Step != "disclosure" {
		t.Fatalf("%+v %s", s, m.status.text)
	}
	m = framed(t, m, &frames, "y", "e", "s", "enter")

	r, ok := a.setup()
	if !ok {
		t.Fatal(a.ops())
	}
	caps := dispatchermodel.DefaultCaps()
	caps.RunsPerDay, caps.DailyTokens = 40, 500000
	want := map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-sonnet-4-5",
		"key": modelTestKey, "caps": caps.Args(), "egress_text_version": dispatchermodel.DisclosureVersion, "egress_confirmed": true}
	if !reflect.DeepEqual(r.Args, want) || r.Workspace != "ws_1" || r.Project != "prj_1" {
		t.Fatalf("%v", r)
	}
	if got := a.ops(); !reflect.DeepEqual(got, []string{"dispatcher_model_doctor", "dispatcher_model_setup", "dispatcher_model_status", "dispatcher_model_status", "dispatcher_model_doctor"}) {
		t.Fatal(got)
	}
	for i, f := range frames {
		if strings.Contains(f, "SECRET") || strings.Contains(f, modelTestKey[:8]) {
			t.Fatalf("frame %d shows the key:\n%s", i, f)
		}
	}
	for _, r := range typedKey {
		if r != 0 {
			t.Fatal("the typed key was not zeroed")
		}
	}
	s := m.ModelSetupState()
	if s.Step != "" || s.KeyEntered || m.modelSetup.wizard.key != nil || !strings.Contains(s.Outcome, "the model is set up") {
		t.Fatalf("%+v", s)
	}
	if !strings.Contains(Render(m), "dispatcher enable --engine model") {
		t.Fatal(Render(m))
	}
}

// Esc, leaving the screen, and quitting each end a setup and zero the key.
func TestModelSetupWipesTheKey(t *testing.T) {
	start := func(t *testing.T) (Model, []rune) {
		m := modelScreen(t, &modelAuthority{})
		m = press(t, m, "s", "enter")
		m = typed(t, m, "m1")
		m = press(t, m, "enter", modelTestKey)
		return m, m.modelSetup.wizard.key
	}
	zeroed := func(t *testing.T, m Model, key []rune) {
		t.Helper()
		for _, r := range key {
			if r != 0 {
				t.Fatal("the key was left in memory")
			}
		}
		if s := m.ModelSetupState(); s.Step != "" || s.KeyEntered {
			t.Fatalf("%+v", s)
		}
	}
	m, key := start(t)
	m = press(t, m, "esc")
	zeroed(t, m, key)
	if !strings.Contains(m.status.text, "Nothing was set up") {
		t.Fatal(m.status.text)
	}

	m, key = start(t)
	m, _ = m.Show(ScreenOverview)
	zeroed(t, m, key)

	m, key = start(t)
	m, cmd := m.UpdateModel(tea.KeyMsg{Type: tea.KeyCtrlC})
	zeroed(t, m, key)
	if cmd == nil {
		t.Fatal("ctrl+c did not quit")
	}

	// Ctrl-U clears the field; backspace zeroes what it removes.
	m, key = start(t)
	m, _ = m.UpdateModel(tea.KeyMsg{Type: tea.KeyBackspace})
	if key[len(key)-1] != 0 || len(m.modelSetup.wizard.key) != len(modelTestKey)-1 {
		t.Fatal("backspace left the rune")
	}
	m, _ = m.UpdateModel(tea.KeyMsg{Type: tea.KeyCtrlU})
	zeroed2 := m.modelSetup.wizard.key == nil
	for _, r := range key {
		zeroed2 = zeroed2 && r == 0
	}
	if !zeroed2 {
		t.Fatal("ctrl+u left the key")
	}

	// R143-L1: a paste's trailing newline is dropped, not the paste; a
	// paste with another control character is refused, and says so.
	m = modelScreen(t, &modelAuthority{})
	m = press(t, m, "s", "enter")
	m = typed(t, m, "m1")
	m = press(t, m, "enter", modelTestKey+"\r\n")
	if string(m.modelSetup.wizard.key) != modelTestKey {
		t.Fatal("the pasted key with a newline was not taken")
	}
	m, _ = m.UpdateModel(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = press(t, m, "ab\x1bc")
	if len(m.modelSetup.wizard.key) != 0 || !strings.Contains(m.status.text, "control character") {
		t.Fatal(len(m.modelSetup.wizard.key), m.status.text)
	}

	// A key that outgrows its buffer leaves no copy behind.
	small := []rune("abc")[:3:3]
	grown := appendKey(small, []rune("def"))
	if string(grown) != "abcdef" || small[0] != 0 || small[2] != 0 {
		t.Fatal(string(grown), small)
	}
}

// Fields take every key: a digit or q in a field neither switches screens
// nor quits. A model ID that looks like a key is refused and not redrawn;
// a refused setup sends nothing; a declined disclosure ends the setup.
func TestModelSetupFields(t *testing.T) {
	a := &modelAuthority{}
	m := modelScreen(t, a)
	m = press(t, m, "s", "enter")
	m = typed(t, m, "q9-model")
	if m.CurrentScreen() != ScreenModel || m.modelSetup.wizard.input != "q9-model" {
		t.Fatal(m.CurrentScreen(), m.modelSetup.wizard.input)
	}
	for range "q9-model" {
		m, _ = m.UpdateModel(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	m = press(t, m, modelTestKey, "enter")
	if s := m.ModelSetupState(); s.Step != "model" || !strings.Contains(m.status.text, "looks like a key") || strings.Contains(Render(m), "SECRET") {
		t.Fatalf("%+v %s", s, m.status.text)
	}
	m = typed(t, m, "m1")
	m = press(t, m, "enter", modelTestKey, "enter", "enter", "enter", "enter", "enter", "enter")
	if s := m.ModelSetupState(); s.Step != "caps" || !strings.Contains(m.status.text, "daily") {
		t.Fatalf("%+v %q", s, m.status.text)
	}
	m = typed(t, m, "9000")
	m = press(t, m, "enter", "n", "o", "enter")
	if s := m.ModelSetupState(); s.Step != "" || s.KeyEntered || !strings.Contains(m.status.text, "not confirmed") {
		t.Fatalf("%+v %s", s, m.status.text)
	}
	if _, sent := a.setup(); sent {
		t.Fatal("a refused setup was sent")
	}

	// The custom provider asks for its base URL first, under the shared rule.
	m = press(t, m, "s")
	for range dispatchermodel.Providers()[1:] {
		m = press(t, m, "down")
	}
	m = press(t, m, "enter")
	m = typed(t, m, "http://example.test/v1")
	m = press(t, m, "enter")
	if s := m.ModelSetupState(); s.Step != "base_url" || s.Provider != "openai-compatible" || m.status.text == "" {
		t.Fatalf("%+v %s", s, m.status.text)
	}
}

// A server error that echoes the key is replaced, and the key is gone.
func TestModelSetupRedactsServerErrors(t *testing.T) {
	a := &modelAuthority{fail: errors.New("invalid: " + modelTestKey)}
	m := modelScreen(t, a)
	m = press(t, m, "s", "enter")
	m = typed(t, m, "m1")
	m = press(t, m, "enter", modelTestKey, "enter", "enter", "enter", "enter", "enter")
	m = typed(t, m, "9000")
	m = press(t, m, "enter")
	m = typed(t, m, "yes")
	m = press(t, m, "enter")
	if s := m.ModelSetupState(); s.Step != "" || s.KeyEntered || !strings.Contains(m.status.text, "Setup failed") || strings.Contains(Render(m), "SECRET") {
		t.Fatalf("%+v %s", s, m.status.text)
	}
}

// Only an owner gets a status, and only an owner on a server with a seal
// key can start a setup.
func TestModelSetupOwnerOnly(t *testing.T) {
	m := modelScreen(t, &modelAuthority{denied: true})
	m = press(t, m, "s")
	if s := m.ModelSetupState(); s.Owner || s.Step != "" || !strings.Contains(Render(m), "Only a project owner") {
		t.Fatalf("%+v\n%s", s, Render(m))
	}
	m = modelScreen(t, &modelAuthority{noSeal: true})
	m = press(t, m, "s")
	if s := m.ModelSetupState(); s.Step != "" || !strings.Contains(m.status.text, "seal key") {
		t.Fatalf("%+v %s", s, m.status.text)
	}
}

// The key is gone once sent, and a reload while the test runs keeps its
// answer.
func TestModelSetupReloadDuringTest(t *testing.T) {
	a := &modelAuthority{outcomes: []string{"no_tool_use"}}
	m := modelScreen(t, a)
	m = press(t, m, "s", "enter")
	m = typed(t, m, "m1")
	m = press(t, m, "enter", modelTestKey, "enter", "enter", "enter", "enter", "enter")
	m = typed(t, m, "9000")
	m = press(t, m, "enter")
	m = typed(t, m, "yes")
	typedKey := m.modelSetup.wizard.key
	m, sendCmd := m.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
	// The key is wiped as it is sent, not when the test ends.
	if s := m.ModelSetupState(); s.Step != "sending" || s.KeyEntered || len(typedKey) == 0 || typedKey[0] != 0 {
		t.Fatalf("%+v", s)
	}
	m, testCmd := m.UpdateModel(sendCmd())
	if s := m.ModelSetupState(); s.Step != "testing" || s.Revision != 3 {
		t.Fatalf("%+v", s)
	}
	m, reloadCmd := m.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = settle(t, m, reloadCmd)
	m = settle(t, m, testCmd)
	if s := m.ModelSetupState(); s.Step != "" || !strings.Contains(s.Outcome, "didn't call the tool") {
		t.Fatalf("%+v", s)
	}
}
