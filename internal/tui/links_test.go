package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const heldIssue = "00000000-0000-4000-8000-0000000000c1"

// linksSource is a linked board with one issue, held by a session.
type linksSource struct{ testLinearSource }

func (s *linksSource) Do(ctx context.Context, r LinearRequest) (LinearData, error) {
	issue := linearprovider.Issue{ID: heldIssue, Identifier: "SYN-5", Title: "Parser", State: "In Progress"}
	switch r.Operation {
	case "issues":
		s.calls = append(s.calls, r)
		return LinearData{Binding: s.binding, Issues: linearprovider.Page[linearprovider.Issue]{Items: []linearprovider.Issue{issue}, Complete: true}}, nil
	case "issue":
		s.calls = append(s.calls, r)
		return LinearData{Binding: s.binding, Issue: &issue}, nil
	}
	return s.testLinearSource.Do(ctx, r)
}

func linksAuthority() *fakeAuthority {
	sessions := []any{
		map[string]any{"id": "ses_1", "name": "engineer", "status": "active"},
		map[string]any{"id": "ses_2", "name": "reviewer", "status": "active",
			"assignment": map[string]any{"state": "confirmed", "issue_uuid": heldIssue, "issue_identifier": "SYN-5"}},
	}
	return &fakeAuthority{results: map[string]bus.Result{
		"project_overview": {Source: "server", Data: map[string]any{"project": map[string]any{"role": "member"}, "sessions": sessions, "linear": map[string]any{"configured": true}}},
		"list_sessions":    {Source: "server", Data: map[string]any{"sessions": sessions}},
	}}
}

func linksModel(t *testing.T, a *fakeAuthority, source *linksSource) Model {
	t.Helper()
	source.binding = bus.TaskboardBinding{Configured: true, Revision: 4, LinearTeamIDs: []string{linearTestTeam}, LinearDefaultTeamID: linearTestTeam}
	cfg := testConfig(a)
	cfg.Linear = source
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m.SetSize(100, 40)
}

// U40: the issue list names the session holding each issue; its details
// go to that session with s.
func TestLinearIssueNamesItsSession(t *testing.T) {
	source := &linksSource{}
	m := linksModel(t, linksAuthority(), source)
	m, cmd := m.Show(ScreenLinear)
	m = quickSettle(m, cmd)
	m = do(m, "enter")
	if view := Render(m); !strings.Contains(view, "SYN-5 · Parser · In Progress ·  · held by reviewer") {
		t.Fatal(view)
	}
	m = do(m, "enter")
	if view := Render(m); !strings.Contains(view, "Held by reviewer") || !strings.Contains(footerText(m), "s go to its session") {
		t.Fatal(view, footerText(m))
	}
	m = do(m, "s")
	if s, ok := m.selectedSession(); m.screen != ScreenSessions || !ok || s.ID != "ses_2" {
		t.Fatal("s didn't land on the holder:", m.screen, s.ID)
	}
}

// U15: i opens the selected session's issue on Linear, with no connection
// chosen yet (the adapter takes the only one); a session with no issue
// offers no i.
func TestSessionOpensItsIssue(t *testing.T) {
	source := &linksSource{}
	m := linksModel(t, linksAuthority(), source)
	m, cmd := m.Show(ScreenSessions)
	m = quickSettle(m, cmd)
	if strings.Contains(footerText(m), "open its issue") || do(m, "i").screen != ScreenSessions {
		t.Fatal("i on a session with no issue")
	}
	m = do(m, "down")
	if !strings.Contains(footerText(m), "i open its issue") {
		t.Fatal(footerText(m))
	}
	m = do(m, "i")
	last := source.calls[len(source.calls)-1]
	if m.screen != ScreenLinear || m.linear.step != "details" || last.Operation != "issue" || last.IssueID != heldIssue || last.ProfileID != "" {
		t.Fatal("i:", m.screen, m.linear.step, last)
	}
	if view := Render(m); !strings.Contains(view, "Issue SYN-5 · Parser") || !strings.Contains(view, "Held by reviewer") {
		t.Fatal(view)
	}
	if back := do(m, "esc"); back.linear.step != "tasks" || strings.Contains(Render(back), "Held by") {
		t.Fatal("esc, or the holder line left on the list:", back.linear.step, Render(back))
	}
}

// L1: a Projects load reads at most maxOverviews overviews; opening one
// past them reads it then.
func TestProjectsReadOverviewsOnDemand(t *testing.T) {
	projects := []any{}
	overviews := map[string]map[string]any{}
	for i := 0; i < maxOverviews+3; i++ {
		slug := fmt.Sprintf("p%02d", i)
		projects = append(projects, map[string]any{"id": "prj_" + slug, "slug": slug, "role": "reader"})
		overviews[slug] = map[string]any{"coordinator": map[string]any{"name": "coord-" + slug, "status": "active"}}
	}
	a := &fakeAuthority{overviews: overviews, results: map[string]bus.Result{"list_projects": {Source: "server", Data: map[string]any{"projects": projects}}}}
	cfg := testConfig(a)
	cfg.Now = func() time.Time { return time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC) }
	m, _ := New(cfg)
	m = m.SetSize(100, 60)
	m, cmd := m.Show(ScreenProjects)
	m = quickSettle(m, cmd)
	count := func() int {
		n := 0
		for _, c := range a.calls {
			if c.request.Operation == "project_overview" {
				n++
			}
		}
		return n
	}
	if count() != maxOverviews {
		t.Fatal("reads:", count())
	}
	m = do(m.Select(maxOverviews+1), "enter")
	if count() != maxOverviews+1 || !strings.Contains(Render(m), "coord-p21") {
		t.Fatal("opening a later project:", count(), Render(m))
	}
	m = do(m, "esc", "enter")
	if count() != maxOverviews+1 {
		t.Fatal("read again:", count())
	}
}

func TestIssueHolderSaysAProposal(t *testing.T) {
	if got := (issueHolder{name: "reviewer", state: "proposed"}).String(); got != "reviewer (proposed)" {
		t.Fatal(got)
	}
	if got := (issueHolder{name: "reviewer", state: "confirmed"}).String(); got != "reviewer" {
		t.Fatal(got)
	}
}

// A project's overview that arrives after the screen moved on is dropped.
func TestStaleOverviewIsDropped(t *testing.T) {
	m := Model{screen: ScreenProjects, request: 3}
	if got := m.applyOverviewMsg(overviewMsg{request: 2, id: "prj_1"}); got.overviews != nil {
		t.Fatal("a stale overview landed")
	}
	if got := m.applyOverviewMsg(overviewMsg{request: 3, id: "prj_1"}); len(got.overviews) != 1 {
		t.Fatal("a current overview didn't land")
	}
}
