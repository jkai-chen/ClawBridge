package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"clawbridge/internal/bus"
)

// overviewFor loads Overview for a profile with these doctor checks and
// these server answers.
func overviewFor(t *testing.T, mode Mode, session bool, checks []Check, results map[string]bus.Result, errs map[string]error, linked bool) Model {
	t.Helper()
	a := &fakeAuthority{results: results, errs: errs}
	cfg := testConfig(a)
	cfg.Mode = mode
	if !session {
		cfg.Session, cfg.Instance = "", ""
	}
	cfg.Doctor = DoctorFunc(func(context.Context) ([]Check, error) { return checks, nil })
	source := &testLinearSource{}
	if linked {
		source.binding = bus.TaskboardBinding{Configured: true, Revision: 1}
	}
	cfg.Linear = source
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(80, 24)
	m, cmd := m.Show(ScreenOverview)
	return quickSettle(m, cmd)
}

var passing = []Check{{ID: "authority.health", Title: "Server reachable", Status: Pass}}

// U19, U23: Overview says what needs the person, each line naming its tab;
// Enter goes there.
func TestOverviewNeedsAttention(t *testing.T) {
	owner := map[string]bus.Result{
		"owner_transfer_get": {Source: "server", Data: map[string]any{"pending": map[string]any{"from": "ada@example.test", "to": "human@example.test"}}},
		"invite_list":        {Source: "server", Data: map[string]any{"invites": []any{map[string]any{"status": "pending"}, map[string]any{"status": "used"}}}},
	}
	checks := append([]Check{{ID: "linear.binding", Title: "Linear board", Status: Fail}}, passing...)
	m := overviewFor(t, ModeCloud, true, checks, owner, nil, false)
	view := Render(m)
	for _, want := range []string{
		"Doctor: 1 check fails", "8 Doctor",
		"This project's Linear board isn't linked (optional)", "5 Linear",
		"Ownership offered to you by ada@example.test", "1 invite is waiting to be used", "7 Account",
		"The dispatcher's model isn't set up (optional)", "6 Dispatcher",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	golden(t, "overview-owner-80x24", view)
	if got := do(m, "enter"); got.screen != ScreenDoctor {
		t.Fatal("enter on the doctor line:", got.screen)
	}
	if got := do(m, "down", "enter"); got.screen != ScreenLinear {
		t.Fatal("enter on the Linear line:", got.screen)
	}
	// Once linked, the Linear line goes (U23); with nothing due, it says so.
	quiet := overviewFor(t, ModeCloud, true, passing, nil, map[string]error{"dispatcher_model_doctor": errors.New("project owner permission required")}, true)
	view = Render(quiet)
	if !strings.Contains(view, "Nothing needs you.") || strings.Contains(view, "Linear") && strings.Contains(view, "isn't linked") {
		t.Fatalf("a member with nothing due:\n%s", view)
	}
	golden(t, "overview-member-80x24", view)
	// With nothing due, the one row is the project's intake line.
	if after := do(quiet, "enter"); after.screen != ScreenLinear || after.linear.step != "intake" {
		t.Fatal("enter on the intake line:", after.screen, after.linear.step)
	}
}

// A line for a tab this profile lacks is dropped.
func TestAttentionOnlyForShownTabs(t *testing.T) {
	m := overviewFor(t, ModeLocal, false, passing, nil, nil, true)
	m = m.applyAttention(attentionMsg{request: m.request, items: []attentionItem{{Text: "an offer", Screen: ScreenAccount}, {Text: "a check", Screen: ScreenDoctor}}})
	if len(m.attention) != 1 || m.attention[0].Screen != ScreenDoctor {
		t.Fatal(m.attention)
	}
}

// U20, U21: identity in words; without a session, no unread count.
func TestOverviewSaysWhoAndWhere(t *testing.T) {
	m := overviewFor(t, ModeLocal, false, passing, nil, nil, true)
	view := Render(m)
	for _, want := range []string{"Signed in       human@example.test (Google", "Inbox           no session on this profile (agents' inboxes are per session)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"Unread", "google-id-token", "pinned", "Account"} {
		if strings.Contains(view, unwanted) {
			t.Fatalf("%q in\n%s", unwanted, view)
		}
	}
	golden(t, "overview-local-80x24", view)
}

// U38, U39: Doctor lists failures first, folds the passes, jumps to a
// failing check's screen on Enter, and keeps the last results while it
// runs again.
func TestDoctorFailuresFirstAndJumps(t *testing.T) {
	m := screenFixtures(t)[ScreenDoctor]
	m.checks = append(m.checks, Check{ID: "linear.markers", Title: "Linear markers", Status: Pass})
	if c, ok := m.selectedCheck(); !ok || c.ID != "linear.binding" {
		t.Fatal("not first:", c)
	}
	// The rows are the ones shown: with the passes folded there's one,
	// so down stays on it.
	if got := do(m, "down", "enter"); got.screen != ScreenLinear {
		t.Fatal("enter on a Linear warning:", got.screen)
	}
	m = do(m, "p")
	if rows := doctorRows(m); len(rows) != 3 || rows[1].Status != Pass {
		t.Fatal(rows)
	}
	m = do(m, "down", "down")
	if c, _ := m.selectedCheck(); c.ID != "linear.markers" {
		t.Fatal(c)
	}
	if after := do(m, "enter"); after.screen != ScreenDoctor {
		t.Fatal("enter on a Linear pass moved")
	}
	m = do(m, "up")
	m = do(m, "down")
	if after := do(m, "enter"); after.screen != ScreenDoctor {
		t.Fatal("enter on a pass moved")
	}
	// A check whose screen this profile lacks doesn't jump.
	local := m
	local.cfg.Mode = ModeLocal
	local.checks = []Check{{ID: "auth.device", Title: "Device key", Status: Fail}}
	local.selected = 0
	if after := do(local, "enter"); after.screen != ScreenDoctor {
		t.Fatal("jumped to a screen local mode lacks")
	}
	// Running again keeps the last results in view.
	m.loading = true
	if view := Render(m); !strings.Contains(view, "Running the checks again…") || !strings.Contains(view, "Linear board") {
		t.Fatalf("the last results went while running:\n%s", view)
	}
}

// U60: a check reads as the CLI doctor's line: the answer on it with the
// message muted under it, or the message alone on the line, escaped.
func TestDoctorShowsTheAnswer(t *testing.T) {
	m := screenFixtures(t)[ScreenDoctor]
	m.checks = []Check{
		{ID: "authority.health", Title: "Workspace", Status: Fail, Answer: "Your workspace can't be reached.", Detail: "dial \x1b[2Jtcp: refused"},
		{ID: "auth.clock", Title: "Clock", Status: Warn, Detail: "the clock is 3m off"},
	}
	view := Render(m)
	for _, want := range []string{"FAIL  Workspace: Your workspace can't be reached.", `dial \x1b[2Jtcp: refused`, "WARN  Clock: the clock is 3m off"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if strings.Count(view, "the clock is 3m off") != 1 || strings.Count(view, "refused") != 1 {
		t.Fatalf("a message shown twice:\n%s", view)
	}
}

// #213 L1: a check without an answer puts its message on the line; a long
// one at 80 columns wraps inside the frame, never past it.
func TestDoctorLongMessageStaysInTheFrame(t *testing.T) {
	m := screenFixtures(t)[ScreenDoctor].SetSize(80, 24)
	long := "the clock is 3m off from the server's, so signed requests may be refused until the clock is set right again"
	m.checks = []Check{{ID: "auth.clock", Title: "Clock", Status: Warn, Detail: long}}
	view := Render(m)
	if !strings.Contains(view, "WARN  Clock: the clock is 3m off") || !strings.Contains(view, "set right again") {
		t.Fatalf("the message isn't all there:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("a line %d cells wide:\n%s", w, view)
		}
	}
}
