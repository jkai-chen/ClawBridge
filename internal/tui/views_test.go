package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const viewsTeamTwo = "22222222-2222-4222-8222-222222222222"

// viewsSource answers the board, two teams, and the views read: the first
// team's views, the second's failure.
type viewsSource struct {
	testLinearSource
	err      error
	unlinked bool
}

func (s *viewsSource) Do(ctx context.Context, r LinearRequest) (LinearData, error) {
	if r.Operation != "views" {
		d, err := s.testLinearSource.Do(ctx, r)
		d.BindingV2 = &bus.TaskboardBindingV2{MappingVersion: 2, Configured: !s.unlinked, Revision: 4, LinearTeamIDs: []string{linearTestTeam, viewsTeamTwo}, LinearDefaultTeamID: linearTestTeam}
		return d, err
	}
	s.calls = append(s.calls, r)
	if s.err != nil {
		return LinearData{}, s.err
	}
	return LinearData{Views: []TeamViews{
		{TeamID: linearTestTeam, OtherKinds: 1, WorkspaceViews: 2, More: true, Views: []linearprovider.View{
			{ID: "view-1", Name: "Agent \x1b[2Jqueue", Kind: "issue", URL: "https://linear.app/example/view/one", TeamID: linearTestTeam},
			{ID: "view-2", Name: "Roadmap", Kind: "project", URL: "https://linear.app/example/view/two", TeamID: linearTestTeam},
		}},
		{TeamID: viewsTeamTwo, Err: "capability_unavailable: list_custom_views isn't offered"},
	}}, nil
}

func (s *viewsSource) viewCalls() int {
	n := 0
	for _, c := range s.calls {
		if c.Operation == "views" {
			n++
		}
	}
	return n
}

func viewsView(t *testing.T, source *viewsSource) Model {
	t.Helper()
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.SetSize(100, 40).Show(ScreenLinear)
	m = quickSettle(m, cmd)
	if !strings.Contains(footerText(m), "v shared views") {
		t.Fatal("no v on the board:", footerText(m))
	}
	return do(m, "v")
}

// U63: each bound team's views by name and kind, escaped, with the
// selected one's URL; what's left out is counted; a team that fails says
// why. Read-only: no Enter, nothing written.
func TestViewsListsEachTeamsViews(t *testing.T) {
	source := &viewsSource{}
	m := viewsView(t, source)
	view := Render(m)
	for _, want := range []string{"Shared views: the board's teams' Linear views", "Read-only. A project owner writes views", "create|update|delete; view show ID",
		`Agent \x1b[2Jqueue`, "issue", "Roadmap", "project", "https://linear.app/example/view/one",
		"Not listed: more views than shown, 1 view of another kind, 2 workspace views.", viewsTeamTwo, "not read", "list_custom_views isn't offered"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if !strings.Contains(view, "› Agent") || strings.Contains(view, "› Roadmap") {
		t.Fatal("the selection:", view)
	}
	if strings.Contains(view, "/view/two") || strings.Contains(view, "ID view-1") {
		t.Fatal("an unselected URL, or the ID without d:", view)
	}
	if source.viewCalls() != 1 {
		t.Fatal("views read", source.viewCalls())
	}
	if strings.Contains(footerText(m), "Enter") {
		t.Fatal("Enter on a read-only list:", footerText(m))
	}
	// Down selects the next view, across into its URL; d shows its ID.
	m = do(m, "down", "d")
	if view = Render(m); !strings.Contains(view, "/view/two") || strings.Contains(view, "/view/one") || !strings.Contains(view, "ID view-2") {
		t.Fatal(view)
	}
	if m = do(m, "r"); source.viewCalls() != 2 {
		t.Fatal("r didn't read again")
	}
	if back := do(m, "esc"); back.linear.step != "tasks" {
		t.Fatal("esc:", back.linear.step)
	}
}

// Without the viewer's Linear connection the read fails, and says so.
func TestViewsWithoutAConnection(t *testing.T) {
	m := viewsView(t, &viewsSource{err: errors.New("no Linear connection: run clawbridge linear connect")})
	if !strings.Contains(m.status.text, "no Linear connection") || m.views.loaded {
		t.Fatal(m.status.text)
	}
}

// No linked board, no v; a late answer after leaving the view is dropped.
func TestViewsNeedALinkedBoard(t *testing.T) {
	cfg := testConfig(&fakeAuthority{})
	source := &viewsSource{unlinked: true}
	cfg.Linear = source
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.SetSize(100, 40).Show(ScreenLinear)
	m = quickSettle(m, cmd)
	if strings.Contains(footerText(m), "shared views") {
		t.Fatal(footerText(m))
	}
	if m = do(m, "v"); m.linear.step == "views" || source.viewCalls() != 0 {
		t.Fatal("v opened the view")
	}
	m.linear.step = "tasks"
	if m = m.applyViews(viewsLoadedMsg{request: m.request, teams: []TeamViews{{TeamID: linearTestTeam}}}); m.views.loaded {
		t.Fatal("a late answer applied off the view")
	}
}
