package tui

import (
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

// U33: l on a linked board starts from it: its teams ticked, its default
// and its projects kept, so one change is one change.
func TestRelinkStartsFromTheBoard(t *testing.T) {
	source := &testLinearSource{binding: bus.TaskboardBinding{Configured: true, Revision: 4, LinearTeamIDs: []string{linearTestTeam}, LinearDefaultTeamID: linearTestTeam}}
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(100, 40)
	m, cmd := m.Show(ScreenLinear)
	m = quickSettle(m, cmd)
	m = do(m, "enter", "l")
	if m.linearStep() != "teams" || !linearHas(m.linear.chosen, linearTestTeam) || m.linear.defaultTeam != linearTestTeam || !m.linear.teamBoard {
		t.Fatal("relink dropped the board:", m.linear.chosen, m.linear.defaultTeam, m.linear.teamBoard)
	}
	if view := Render(m); !strings.Contains(view, "[x] Team") || !strings.Contains(view, "Starting from the linked board") {
		t.Fatal(view)
	}
}

// Not linked: l starts empty, as before.
func TestLinkStartsEmptyWhenUnlinked(t *testing.T) {
	m := Model{linear: linearState{chosen: []string{"stale"}}}
	m.linear.relinking = true
	if got := m.linearFromBoard(); len(got.linear.chosen) != 1 || got.linear.relinking {
		t.Fatal("an unlinked board changed the choice:", got.linear.chosen)
	}
}

// A board of Linear projects starts with those projects chosen.
func TestRelinkKeepsTheLinkedProjects(t *testing.T) {
	m := Model{linear: linearState{bindingV2: &bus.TaskboardBindingV2{Configured: true, Mode: "projects", LinearTeamIDs: []string{"team-a"}, LinearDefaultTeamID: "team-a", LinearProjectIDs: []string{"proj-1", "proj-2"}}}}
	got := m.linearFromBoard()
	if !got.linear.relinking || got.linear.teamBoard || got.linear.defaultTeam != "team-a" || len(got.linear.chosen) != 1 || len(got.linear.selectedProjects) != 2 {
		t.Fatalf("%+v", got.linear)
	}
	got.linear.selectedProjects[0] = "changed"
	if m.linear.bindingV2.LinearProjectIDs[0] != "proj-1" {
		t.Fatal("the choice aliases the binding")
	}
}
