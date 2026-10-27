package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// overviewAuthority lists two projects, the profile's own (core) and one
// it only reads (web), and describes each through project_overview.
func overviewAuthority() *fakeAuthority {
	core := map[string]any{
		"coordinator": map[string]any{"name": "coordinator", "status": "active"},
		"counts":      map[string]any{"active": float64(2), "retired": float64(1), "moved": float64(0), "stale": float64(1)},
		"sessions": []any{
			map[string]any{"id": "ses_c", "name": "coordinator", "status": "active", "presence": "active", "coordinator": true, "agent_kind": "claude-code"},
			map[string]any{"id": "ses_1", "name": "engineer", "status": "active", "presence": "idle", "stale": true,
				"assignment": map[string]any{"state": "confirmed", "issue_identifier": "SYN-5", "task_role": "implementer"}},
			map[string]any{"id": "ses_r", "name": "old", "status": "retired"},
		},
		"linear": map[string]any{"configured": true, "mode": "projects",
			"teams":            []any{map[string]any{"id": "team-1", "name": "Eng", "default": true}, map[string]any{"id": "team-2", "name": ""}},
			"projects":         []any{map[string]any{"id": "proj-1", "name": "Payments \x1b[2J", "closes": "agent"}},
			"names_updated_at": "2026-10-27T08:00:00Z"},
	}
	web := map[string]any{"counts": map[string]any{"active": float64(1)}, "linear": map[string]any{"configured": false},
		"sessions": []any{map[string]any{"id": "ses_w", "name": "web-dev", "status": "active"}}}
	a := &fakeAuthority{results: map[string]bus.Result{
		"list_projects": {Source: "server", Data: map[string]any{"projects": []any{
			map[string]any{"id": "prj_1", "slug": "core", "name": "Core", "role": "owner"},
			map[string]any{"id": "prj_2", "slug": "web", "name": "Web", "role": "reader"},
			map[string]any{"id": "prj_3", "slug": "gone", "name": "Gone", "role": "member"},
		}}},
	}}
	a.overviews = map[string]map[string]any{"core": core, "web": web}
	return a
}

func projectsView(t *testing.T, a *fakeAuthority) Model {
	t.Helper()
	cfg := testConfig(a)
	cfg.Linear = &testLinearSource{}
	cfg.Now = func() time.Time { return time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC) }
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(100, 40)
	m, cmd := m.Show(ScreenProjects)
	return quickSettle(m, cmd)
}

// U11: each project with its sessions, coordinator and board; ● on the
// profile's own; read as the person, never through the session.
func TestProjectsShowWhatEachProjectHolds(t *testing.T) {
	a := overviewAuthority()
	m := projectsView(t, a)
	view := Render(m)
	for _, want := range []string{"PROJECT", "COORDINATOR", "● core", "2 of 3 active", "coordinator", "2 teams · 1 project", "web", "1 of 1 active", "not linked"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if body := renderProjects(m); strings.Contains(body, "● web") || strings.Contains(body, "prj_1") || strings.Contains(body, "workspace-scoped") {
		t.Fatalf("own marker, ID or old subtitle:\n%s", view)
	}
	n := 0
	for _, c := range a.calls {
		if c.request.Operation == "project_overview" {
			n++
			if c.request.Session != "" || len(c.request.Args) != 0 {
				t.Fatal("overview read through a session:", c.request)
			}
		}
	}
	if n != 3 {
		t.Fatal("overview reads:", n)
	}
}

// U12, U13: Enter opens a project read-only; the profile's own offers its
// sessions and board, another says whose it is; Esc returns.
func TestProjectDetailOpensAndLinks(t *testing.T) {
	m := projectsView(t, overviewAuthority())
	m = do(m, "enter")
	view := Render(m)
	for _, want := range []string{"core · Core", "Your profile works in this project.", "Coordinator  coordinator (active)",
		"Sessions     1 active · 1 stale · 1 retired", "teams Eng (default), team-2", `projects Payments \x1b[2J (agents close)`,
		"names as of 2 hours ago", "★ coordinator", "SYN-5", "s its sessions", "l its Linear board"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if after := do(m, "esc"); after.projectOpen || !strings.Contains(Render(after), "PROJECT") {
		t.Fatal("esc didn't return to the list")
	}
	if after := do(m, "s"); after.screen != ScreenSessions {
		t.Fatal("s:", after.screen)
	} else if back := do(after, "2"); back.projectOpen {
		t.Fatal("came back to Projects with a project open")
	}
	if after := do(m, "l"); after.screen != ScreenLinear {
		t.Fatal("l:", after.screen)
	}
	web := do(m, "esc", "down", "enter")
	view = Render(web)
	if !strings.Contains(view, "Viewing web (read-only); your profile works in core.") || !strings.Contains(view, "Linear       not linked") || !strings.Contains(view, "web-dev") {
		t.Fatal(view)
	}
	if strings.Contains(footerText(web), "its sessions") || do(web, "s").screen != ScreenProjects || do(web, "l").screen != ScreenProjects {
		t.Fatal("another project's sessions or board offered:", footerText(web))
	}
}

// A project whose overview can't be read still lists, and says why.
func TestProjectOverviewErrorIsShown(t *testing.T) {
	a := overviewAuthority()
	m := projectsView(t, a)
	m = do(m, "down", "down")
	if row := Render(m); !strings.Contains(row, "gone") {
		t.Fatal(row)
	}
	m = do(m, "enter")
	if view := Render(m); !strings.Contains(view, "Couldn't read this project: project overview unavailable offline") {
		t.Fatal(view)
	}
}

// U14–U16: the coordinator starred, the issue in its column, one state
// word; OWNER and HOST only when there's room.
func TestSessionsColumnsAndCoordinator(t *testing.T) {
	rows := sessionRows([]map[string]any{
		{"id": "ses_c", "name": "lead", "status": "active", "presence": "active", "coordinator": true, "owner_principal": "owner@example.test"},
		{"id": "ses_1", "name": "eng", "status": "active", "presence": "idle", "stale": true, "assignment": map[string]any{"state": "proposed", "issue_identifier": "SYN-9"}},
		{"id": "ses_r", "name": "old", "status": "retired", "presence": "active"},
	})
	m := Model{width: 100, sessions: rows, screen: ScreenSessions, cfg: Config{Now: time.Now}}
	view := renderSessions(m)
	for _, want := range []string{"★ lead", "SYN-9 (proposed)", "stale", "retired", "lead ★ coordinator"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if strings.Contains(view, "OWNER") || !strings.Contains(view, "Owner    owner@example.test") {
		t.Fatalf("OWNER column at 100, or no owner in the details:\n%s", view)
	}
	if m.width = 150; !strings.Contains(renderSessions(m), "OWNER") {
		t.Fatal("no OWNER at 150")
	}
	if got := sessionCounts(rows); got != "1 active · 1 stale · 1 retired" {
		t.Fatal(got)
	}
}

// U26, U27: n on a session opens a message to it; Inbox's n asks whom,
// Tab offering the live sessions but this one; Enter goes on, then sends.
func TestMessageEditorToThenMessage(t *testing.T) {
	a := &fakeAuthority{results: map[string]bus.Result{"list_sessions": {Source: "server", Data: map[string]any{"sessions": []any{
		map[string]any{"id": "ses_1", "name": "engineer", "status": "active"},
		map[string]any{"id": "ses_2", "name": "reviewer", "status": "active"},
		map[string]any{"id": "ses_3", "name": "tester", "status": "active"},
		map[string]any{"id": "ses_4", "name": "retiree", "status": "retired"},
	}}}}}
	m, err := New(testConfig(a))
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(100, 30)
	m, cmd := m.Show(ScreenSessions)
	m = quickSettle(m, cmd)
	if after := do(m, "n"); after.inputMode != inputNone || !strings.Contains(after.status.text, "That's this session") {
		t.Fatal("messaged itself:", after.status.text)
	}
	m = do(m, "down", "n")
	if m.inputMode != inputSend || m.sendTo != "reviewer" || !m.sendBody || !strings.Contains(Render(m), "To: reviewer") {
		t.Fatal("n didn't prefill:", m.sendTo, Render(m))
	}
	m = do(m, "esc")
	m, cmd = m.Show(ScreenInbox)
	m = quickSettle(m, cmd)
	m = do(m, "n")
	if m.sendBody || !strings.Contains(Render(m), "Tab: next session") {
		t.Fatal("Inbox's n didn't ask whom:", Render(m))
	}
	if m = do(m, "enter"); m.sendBody || !strings.Contains(m.status.text, "Choose whom") {
		t.Fatal("went on with no recipient")
	}
	m = do(m, "tab")
	if m.sendTo != "reviewer" {
		t.Fatal("tab:", m.sendTo)
	}
	if m = do(m, "tab"); m.sendTo != "tester" {
		t.Fatal("tab didn't move on:", m.sendTo)
	}
	if m = do(m, "tab"); m.sendTo != "reviewer" {
		t.Fatal("tab didn't cycle past retired and self:", m.sendTo)
	}
	m = do(m, "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "t", "tab")
	if m.sendTo != "tester" {
		t.Fatal("tab didn't complete what's typed:", m.sendTo)
	}
	m = do(m, "enter")
	if !m.sendBody {
		t.Fatal("enter didn't go on to the message")
	}
	if m = do(m, "enter"); !strings.Contains(m.status.text, "Write a message first") || a.callIndex("send") >= 0 {
		t.Fatal("sent an empty message")
	}
	m = do(m, "h", "i", "shift+tab")
	if m.sendBody || m.draft != "hi" {
		t.Fatal("shift+tab didn't go back to To:", m.draft)
	}
	m = do(m, "enter", "enter")
	i := a.callIndex("send")
	if i < 0 || a.calls[i].request.Args["to"] != "tester" || a.calls[i].request.Args["body"] != "hi" {
		t.Fatal("send:", a.calls)
	}
}

// Without a session there's nothing to send as: n says so.
func TestMessageNeedsASession(t *testing.T) {
	cfg := testConfig(&fakeAuthority{})
	cfg.Session, cfg.Instance = "", ""
	m, _ := New(cfg)
	m.screen = ScreenInbox
	if m = m.BeginSendTo("reviewer"); m.inputMode != inputNone || !strings.Contains(m.status.text, "Sending needs a session") {
		t.Fatal(m.status.text)
	}
}

var errOffline = errors.New("project overview unavailable offline: the server could not be reached")

// U22: Overview counts sessions in the Sessions tab's words and lists the
// ones its "current" filter shows: a long-retired one is counted, not listed.
func TestOverviewCountsSessionsLikeTheSessionsTab(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	m := Model{width: 100, cfg: Config{Now: func() time.Time { return now }}, attentionLoaded: true, sessions: []SessionRow{
		{Name: "lead", Status: "active", Coordinator: true},
		{Name: "fresh-retiree", Status: "retired", UpdatedAt: "2026-10-27T09:00:00Z"},
		{Name: "long-gone", Status: "retired", UpdatedAt: "2026-10-01T09:00:00Z"},
	}}
	view := renderOverview(m)
	if !strings.Contains(view, "Sessions · 1 active · 2 retired") || !strings.Contains(view, "★ lead") || !strings.Contains(view, "fresh-retiree") || strings.Contains(view, "long-gone") {
		t.Fatal(view)
	}
}
