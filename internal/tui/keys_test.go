package tui

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden views in testdata/golden")

// keyMsg is the message a key string comes from.
func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "delete":
		return tea.KeyMsg{Type: tea.KeyDelete}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

// quickSettle runs a model's commands until none is left or one waits (the
// archive tick waits a minute): that one is dropped.
func quickSettle(m Model, cmd tea.Cmd) Model {
	for i := 0; cmd != nil && i < 10; i++ {
		got := make(chan tea.Msg, 1)
		go func(c tea.Cmd) { got <- c() }(cmd)
		select {
		case msg := <-got:
			m, cmd = m.UpdateModel(msg)
		case <-time.After(200 * time.Millisecond):
			return m
		}
	}
	return m
}

// do presses keys, settling quickly after each.
func do(m Model, keys ...string) Model {
	for _, k := range keys {
		var cmd tea.Cmd
		m, cmd = m.UpdateModel(keyMsg(k))
		m = quickSettle(m, cmd)
	}
	return m
}

// screenFixtures are one loaded model per screen, as a person sees it.
func screenFixtures(t *testing.T) map[Screen]Model {
	t.Helper()
	out := map[Screen]Model{}
	inbox := &fakeAuthority{messages: []map[string]any{
		{"id": "msg_1", "sender": "reviewer", "body": "Can you take the parser?", "thread_id": "msg_1", "created_at": "2026-10-27T09:12:44Z", "delivered_at": "2026-10-27T09:12:45Z"},
		{"id": "msg_2", "sender": "lead", "body": "Yes, after lunch.", "thread_id": "msg_1", "reply_to": "msg_1", "created_at": "2026-10-27T09:20:00Z", "delivered_at": "2026-10-27T09:20:01Z", "read_at": "2026-10-27T09:30:00Z"},
	}}
	cfg := testConfig(inbox)
	cfg.Doctor = DoctorFunc(func(context.Context) ([]Check, error) {
		return []Check{{ID: "server", Title: "Server reachable", Status: "pass"}, {ID: "linear", Title: "Linear board", Status: "warn", Detail: "not linked"}}, nil
	})
	cfg.Linear = &testLinearSource{}
	for _, s := range []Screen{ScreenOverview, ScreenProjects, ScreenSessions, ScreenInbox, ScreenDoctor, ScreenLinear} {
		m, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		m = m.SetSize(120, 40)
		m, cmd := m.Show(s)
		out[s] = quickSettle(m, cmd)
	}
	out[ScreenAccount] = accountModel(t, &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"})
	out[ScreenModel] = modelScreen(t, &modelAuthority{outcomes: []string{"passed"}})
	return out
}

// §3.1: a screen's keys never reuse a global one, and none is listed twice.
func TestKeymapHasNoClashes(t *testing.T) {
	for _, mode := range []Mode{ModeCloud, ModeLocal} {
		m := Model{cfg: Config{Mode: mode}}
		global := map[string]string{}
		for _, b := range globalBindings(m) {
			for _, k := range b.Keys {
				if global[k] != "" {
					t.Fatalf("global %q twice: %s and %s", k, global[k], b.ID)
				}
				global[k] = b.ID
			}
		}
		for _, s := range orderedScreens {
			seen := map[string]string{}
			for _, b := range screenBindings(s) {
				if b.ID == "" || (b.Label == "" && b.Labeler == nil) || b.Action == nil || len(b.Keys) == 0 {
					t.Fatalf("%s: incomplete binding %+v", s, b)
				}
				for _, k := range b.Keys {
					if global[k] != "" {
						t.Fatalf("%s: %q is %s here but %s everywhere", s, k, b.ID, global[k])
					}
					if seen[k] != "" {
						t.Fatalf("%s: %q twice: %s and %s", s, k, seen[k], b.ID)
					}
					seen[k] = b.ID
				}
			}
		}
	}
}

// candidateKeys are the keys a person might press.
func candidateKeys() []string {
	out := []string{"enter", "esc", "tab", "shift+tab", "up", "down", "left", "right", "pgup", "pgdown", "home", "end", "backspace", "delete", " "}
	for r := '!'; r <= '~'; r++ {
		out = append(out, string(r))
	}
	return out
}

// The dispatch, the footer and the overlay read one table: a key that no
// live binding has does nothing on any screen, and every live binding's
// keys reach it.
func TestKeysAreTheKeymap(t *testing.T) {
	for s, m := range screenFixtures(t) {
		for _, k := range candidateKeys() {
			b, ok := m.binding(k)
			if ok {
				for _, key := range b.Keys {
					if again, _ := m.binding(key); again.ID != b.ID {
						t.Fatalf("%s: %q reaches %s, not %s", s, key, again.ID, b.ID)
					}
				}
				continue
			}
			after, cmd := m.UpdateModel(keyMsg(k))
			if cmd != nil || Render(after) != Render(m) {
				t.Fatalf("%s: %q isn't in the keymap but did something:\n%s", s, k, Render(after))
			}
		}
		// Wide enough for every key; narrower, the last ones give way to
		// "… · ? help" (help lists them all).
		footer, help := footerText(m.SetSize(200, 40)), renderHelp(m)
		for _, b := range screenBindings(s) {
			if !strings.Contains(help, b.key()) {
				t.Fatalf("%s: help lacks %s", s, b.ID)
			}
			if b.live(m) && !strings.Contains(footer, b.key()+" "+b.label(m)) {
				t.Fatalf("%s: footer %q lacks %s", s, footer, b.ID)
			}
		}
	}
}

func TestTabsFollowTheMode(t *testing.T) {
	cloud, err := New(testConfig(&fakeAuthority{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := cloud.tabs(); len(got) != 8 || got[4] != ScreenLinear || got[5] != ScreenModel || got[6] != ScreenAccount || got[7] != ScreenDoctor {
		t.Fatal(got)
	}
	cfg := testConfig(&fakeAuthority{})
	cfg.Mode = ModeLocal
	local, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range local.tabs() {
		if s == ScreenAccount || s == ScreenCloud {
			t.Fatal("local mode shows", s)
		}
	}
	local = do(local, "7")
	if local.screen != ScreenDoctor {
		t.Fatal("7 in local mode:", local.screen)
	}
	// Left and Right are left for panes.
	local = do(local, "1")
	if after, _ := local.UpdateModel(keyMsg("right")); after.screen != ScreenOverview {
		t.Fatal("right switched screens")
	}
	if got := keyHint(cloud, ScreenOverview, "tab:doctor"); got != "8 Doctor" {
		t.Fatal(got)
	}
	if got := keyHint(local, ScreenOverview, "tab:doctor"); got != "7 Doctor" {
		t.Fatal(got)
	}
	if keyHint(cloud, ScreenInbox, "no-such") != "" {
		t.Fatal("a hint for nothing")
	}
}

func TestHelpOverlay(t *testing.T) {
	m := screenFixtures(t)[ScreenInbox]
	m = do(m, "?")
	view := Render(m)
	for _, want := range []string{"Help · Inbox", "This screen", "Enter", "open", "m          mark read", "Everywhere", "Esc        back", "Esc or ? closes help", "› Inbox › Help"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	// Keys read off the overlay act only once it's closed.
	if after, cmd := m.UpdateModel(keyMsg("m")); cmd != nil || !after.help {
		t.Fatal("a key went through the overlay")
	}
	m = do(m, "esc")
	if m.help || strings.Contains(Render(m), "This screen") {
		t.Fatal("esc left help open")
	}
	m = do(m, "?", "?")
	if m.help {
		t.Fatal("? didn't close help")
	}
}

// q quits at once, unless work would be lost: then it asks, and only y
// quits. In an editor q is a letter.
func TestQAsksWhileWorkIsHeld(t *testing.T) {
	m := screenFixtures(t)[ScreenOverview]
	if _, cmd := m.UpdateModel(keyMsg("q")); cmd == nil || cmd() != tea.Quit() {
		t.Fatal("q didn't quit")
	}
	l := screenFixtures(t)[ScreenLinear]
	l = do(l, "enter")
	if l.linearStep() != "teams" {
		t.Fatal(l.linearStep())
	}
	l, cmd := l.UpdateModel(keyMsg("q"))
	if cmd != nil || !l.quitAsk || !strings.Contains(Render(l), "Quit and lose the board you're linking?") {
		t.Fatal("q quit mid-link:", Render(l))
	}
	l, cmd = l.UpdateModel(keyMsg("n"))
	if cmd != nil || l.quitAsk || l.linearStep() != "teams" {
		t.Fatal("another key didn't stay")
	}
	l, _ = l.UpdateModel(keyMsg("q"))
	if _, cmd = l.UpdateModel(keyMsg("y")); cmd == nil || cmd() != tea.Quit() {
		t.Fatal("y didn't quit")
	}
	in := screenFixtures(t)[ScreenInbox]
	in = do(in, "n", "q")
	if in.draft != "q" {
		t.Fatal("q in an editor:", in.draft)
	}
}

// A key that applies on another step does nothing here: + connects only
// on the profiles step, and c (who closes) only in the link review.
func TestKeysOnlyWhereTheyApply(t *testing.T) {
	m := screenFixtures(t)[ScreenLinear]
	if after := do(m, "c"); after.inputMode != inputNone {
		t.Fatal("c connected")
	}
	if after := do(m, "+"); after.inputMode != inputLinearName {
		t.Fatal("+ didn't connect")
	}
	m = do(m, "enter")
	if m.linearStep() != "teams" {
		t.Fatal(m.linearStep())
	}
	for _, k := range []string{"+", "w", "x", "c"} {
		if after, cmd := m.UpdateModel(keyMsg(k)); cmd != nil || after.inputMode != inputNone || Render(after) != Render(m) {
			t.Fatalf("%q acted on the teams step:\n%s", k, Render(after))
		}
	}
}

// Esc on Linear goes back one step and keeps what was chosen.
func TestLinearEscGoesBackOneStep(t *testing.T) {
	m := screenFixtures(t)[ScreenLinear]
	m = do(m, "enter", " ", "enter")
	if m.linearStep() != "default" || len(m.linear.chosen) != 1 {
		t.Fatal(m.linearStep(), m.linear.chosen)
	}
	if !strings.Contains(footerText(m), "Esc back to teams") {
		t.Fatal(footerText(m))
	}
	m = do(m, "esc")
	if m.linearStep() != "teams" || len(m.linear.chosen) != 1 {
		t.Fatal("esc from default:", m.linearStep(), m.linear.chosen)
	}
	m = do(m, "esc")
	if m.linearStep() != "profiles" || m.screen != ScreenLinear {
		t.Fatal("esc from teams:", m.linearStep())
	}
	m = do(m, "esc")
	if m.linearStep() != "profiles" || m.screen != ScreenLinear {
		t.Fatal("esc at the first step moved:", m.linearStep())
	}
}

// Golden views of every screen, and its help, at the common sizes. Run
// with -update to rewrite them after a deliberate change.
func TestGoldenViews(t *testing.T) {
	sizes := [][2]int{{80, 24}, {80, 40}, {120, 40}}
	for s, m := range screenFixtures(t) {
		for _, size := range sizes {
			golden(t, fmt.Sprintf("%s-%dx%d", s, size[0], size[1]), Render(m.SetSize(size[0], size[1])))
		}
		h := m
		h.help = true
		for _, size := range sizes[1:] {
			golden(t, fmt.Sprintf("%s-help-%dx%d", s, size[0], size[1]), Render(h.SetSize(size[0], size[1])))
		}
	}
}

// NO_COLOR, or any terminal without colour, still says what a status is
// in words, and no escape reaches the screen.
func TestViewsSayStatusInWords(t *testing.T) {
	m := screenFixtures(t)[ScreenProjects].SetSize(80, 24)
	m.errText = "remote: 503 Service Unavailable"
	view := Render(m)
	if strings.Contains(view, "\x1b") || !strings.Contains(view, "Error: remote: 503") {
		t.Fatalf("%q", view)
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run go test ./internal/tui -run TestGoldenViews -update)", name, err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from %s:\n%s", name, path, got)
	}
}
