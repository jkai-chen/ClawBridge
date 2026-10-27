package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	tea "github.com/charmbracelet/bubbletea"
)

// unavailable is the transport error a load meets behind a failing proxy.
var unavailable = errors.New("remote: 503 Service Unavailable: upstream connect error or disconnect/reset before headers")

// loadOnce runs a screen's load and applies its answer, and nothing after
// it (the archive tick would wait for a minute).
func loadOnce(m Model, cmd tea.Cmd) Model {
	if cmd == nil {
		return m
	}
	m, _ = m.UpdateModel(cmd())
	return m
}

// statesFixture is a model on screen s whose authority answers as asked:
// with rows, with no rows, or with a 503.
func statesFixture(t *testing.T, s Screen, state string, width int) Model {
	t.Helper()
	a := &fakeAuthority{results: map[string]bus.Result{}, errs: map[string]error{}}
	doctor := DoctorFunc(func(context.Context) ([]Check, error) {
		return []Check{{ID: "server", Title: "Server reachable", Status: Pass}}, nil
	})
	switch state {
	case "error":
		for _, op := range []string{"whoami", "list_projects", "list_sessions", "inbox"} {
			a.errs[op] = unavailable
		}
		doctor = func(context.Context) ([]Check, error) { return nil, unavailable }
	case "empty":
		a.results["list_projects"] = bus.Result{Source: "server", Data: map[string]any{"projects": []any{}}}
		a.results["list_sessions"] = bus.Result{Source: "server", Data: map[string]any{"sessions": []any{}}}
		doctor = func(context.Context) ([]Check, error) { return nil, nil }
	}
	cfg := testConfig(a)
	cfg.Doctor = doctor
	cfg.Now = func() time.Time { return time.Date(2026, 10, 27, 16, 0, 0, 0, time.UTC) }
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(width, 24)
	m, cmd := m.Show(s)
	if state != "loading" {
		m = loadOnce(m, cmd)
	}
	return m
}

// TUI-UX-AUDIT PR B: each screen loading, failing (503) and empty, at 80
// and 120 columns. The goldens are the frames; regenerate them with
// CLAWBRIDGE_UPDATE_TUI_GOLDEN=1.
func TestScreenStatesGolden(t *testing.T) {
	for _, width := range []int{80, 120} {
		for _, s := range []Screen{ScreenOverview, ScreenProjects, ScreenSessions, ScreenInbox, ScreenDoctor} {
			var got strings.Builder
			for _, state := range []string{"loading", "error", "empty"} {
				got.WriteString("### " + state + "\n" + Render(statesFixture(t, s, state, width)) + "\n")
			}
			golden := filepath.Join("testdata", "states", string(s)+"-"+strconv.Itoa(width)+".golden")
			if os.Getenv("CLAWBRIDGE_UPDATE_TUI_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got.String()), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != string(want) {
				t.Errorf("%s at %d changed:\n--- got\n%s\n--- want\n%s", s, width, got.String(), want)
			}
		}
	}
}

// The rules the goldens show, asserted on their own so a regenerated
// golden can't quietly lose them.
func TestScreenStatesRules(t *testing.T) {
	for _, s := range []Screen{ScreenProjects, ScreenSessions, ScreenDoctor} {
		loading := Render(statesFixture(t, s, "loading", 80))
		if !strings.Contains(loading, "Loading ") || strings.Contains(loading, "No ") {
			t.Fatalf("%s loading shows the empty text (U8):\n%s", s, loading)
		}
		failed := Render(statesFixture(t, s, "error", 80))
		if !strings.Contains(failed, "ERROR  Couldn't load") || !strings.Contains(failed, "The server can't be reached.") ||
			!strings.Contains(failed, "r ") || !strings.Contains(failed, "503 Service Unavailable") || strings.Contains(failed, "No ") {
			t.Fatalf("%s error is not the error block (U9, U10):\n%s", s, failed)
		}
		if s != ScreenDoctor && !strings.Contains(failed, "Doctor") {
			t.Fatalf("%s error doesn't point at Doctor:\n%s", s, failed)
		}
		if empty := Render(statesFixture(t, s, "empty", 80)); !strings.Contains(empty, "No ") {
			t.Fatalf("%s empty:\n%s", s, empty)
		}
	}
	// The header keeps its height while a load runs (U8).
	loading := strings.Split(Render(statesFixture(t, ScreenProjects, "loading", 80)), "\n")
	loaded := strings.Split(Render(statesFixture(t, ScreenProjects, "full", 80)), "\n")
	if bodyTop(loading) != bodyTop(loaded) {
		t.Fatalf("the body moved:\n%s\n\n%s", strings.Join(loading, "\n"), strings.Join(loaded, "\n"))
	}
}

// bodyTop is the row the body's frame starts on.
func bodyTop(lines []string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, "╭") {
			return i
		}
	}
	return -1
}

// U36, Q1: without a session the Inbox explains whose inboxes these are; it
// is not an error, and nothing is asked of the server.
func TestInboxWithoutSession(t *testing.T) {
	a := &fakeAuthority{}
	cfg := testConfig(a)
	cfg.Session, cfg.Instance = "", ""
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.SetSize(80, 24).Show(ScreenInbox)
	m = loadOnce(m, cmd)
	frame := Render(m)
	if !strings.Contains(frame, "Inboxes belong to agent sessions") || strings.Contains(frame, "ERROR") || m.State().Error != "" || m.loading {
		t.Fatal(frame)
	}
	for _, c := range a.calls {
		if c.request.Operation == "inbox" {
			t.Fatal("asked the server for an inbox with no session")
		}
	}
	m, cmd = m.Show(ScreenOverview)
	if frame := Render(loadOnce(m, cmd)); !strings.Contains(frame, "no session on this profile") || strings.Contains(frame, "Unread 0") {
		t.Fatalf("U21:\n%s", frame)
	}
}

// U29: a send that works says so, and the line outlives the reload the
// send starts; the next reload clears it.
func TestSendSaysSentThroughReload(t *testing.T) {
	a := &fakeAuthority{}
	m, err := New(testConfig(a))
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.SetSize(100, 30).Show(ScreenInbox)
	m = loadOnce(m, cmd)
	m, cmd = m.Send("engineer-backend", "hello", "op-1")
	m, cmd = m.UpdateModel(cmd()) // the send's answer, which starts a reload
	m = loadOnce(m, cmd)          // the reload's answer
	if s := m.State(); s.Status != clirender.OK || !strings.Contains(Render(m), "OK  Sent to engineer-backend.") {
		t.Fatalf("%+v\n%s", s, Render(m))
	}
	m, cmd = m.Reload()
	if m = loadOnce(m, cmd); strings.Contains(Render(m), "Sent to") {
		t.Fatal("the OK line outlived a second reload")
	}
	// A refusal is a NOTE; a failed write an ERROR, named in words.
	m = m.BeginReply()
	if s := m.State(); s.Status != clirender.Info || !strings.Contains(Render(m), "NOTE  ") {
		t.Fatalf("%+v", s)
	}
	a.errs = map[string]error{"send": unavailable}
	m, cmd = m.Send("engineer-backend", "hello", "op-2")
	m, _ = m.UpdateModel(cmd())
	if frame := Render(m); m.State().Status != clirender.Error || !strings.Contains(frame, "ERROR  The server can't be reached.") {
		t.Fatal(frame)
	}
}

// U57: the level is a word, so it reads without colour.
func TestStatusWordsWithoutColour(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for level, word := range map[status]string{okStatus("x"): "OK  x", noteStatus("x"): "NOTE  x", failStatus("x"): "ERROR  x", busyStatus("x"): "…  x"} {
		if got := level.line(); got != word {
			t.Fatalf("%q, want %q", got, word)
		}
	}
	if (status{}).line() != "" || okStatus("x").kept().kept().text != "" || noteStatus("x").kept().text != "" {
		t.Fatal("kept")
	}
}

// describeError names the kind, and leaves an unknown error as it is.
func TestDescribeError(t *testing.T) {
	for raw, want := range map[string]string{
		"dial tcp 10.0.0.1:443: connect: connection refused": "The server can't be reached.",
		unavailable.Error():                                   "The server can't be reached.",
		"remote: 401 Unauthorized: credential expired":        "You're signed out. Run clawbridge login.",
		"remote: 403 Forbidden: owner permission required":    "You don't have permission for this.",
		"remote: 403 owner only":                              "You don't have permission for this.",
		"unsupported operation list_projects":                 "The server doesn't offer this. Check that the ClawBridge client and server versions match.",
		"the confirmation doesn't match; nothing was changed": "the confirmation doesn't match; nothing was changed",
	} {
		what, rest := describeError(raw)
		if what != want || (what != raw && rest != raw) || (what == raw && rest != "") {
			t.Errorf("%q: %q %q", raw, what, rest)
		}
	}
}

// U41: at 80×24 the model step's field is on screen, below the catalogue.
func TestModelFieldInView(t *testing.T) {
	m := modelScreen(t, &modelAuthority{outcomes: []string{"active"}})
	m = press(t, m.SetSize(80, 24), "s", "enter")
	if s := m.ModelSetupState(); s.Step != "model" {
		t.Fatalf("%+v", s)
	}
	frame := Render(m)
	if !strings.Contains(frame, "> █") {
		t.Fatalf("the field is below the fold:\n%s", frame)
	}
	// While it tests, a digit doesn't leave the screen (U42), and Esc
	// stops waiting with a note.
	m.modelSetup.wizard.step = modelStepTesting
	m = press(t, m, "1")
	if m.screen != ScreenModel || m.modelSetup.wizard.step != modelStepTesting {
		t.Fatal("a key left the screen during the test")
	}
	m = press(t, m, "esc")
	if m.modelSetup.wizard.step != modelStepNone || m.State().Status != clirender.Info || !strings.Contains(m.status.text, "Stopped waiting") {
		t.Fatalf("%+v", m.State())
	}
}
