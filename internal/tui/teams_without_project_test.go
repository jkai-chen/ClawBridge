package tui

import (
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// The board line shows each team listed without a project as Linear shows
// it now: inert, or a stale entry once a bound project carries it.
func TestLinearBoardShowsTeamsWithoutProject(t *testing.T) {
	var m Model
	b := bus.TaskboardBindingV2{Configured: true, Mode: "project_set", LinearTeamIDs: []string{linearTestTeam, "team-two"}, LinearProjectIDs: []string{"p"}, TeamsWithoutProject: []string{"team-two"}}
	m.applyLinearBinding(LinearData{BindingV2: &b, WithoutProject: []linearboard.TeamWithoutProject{{TeamID: "team-two", Name: "AI", Inert: true}}})
	if got := linearBoardLine(m); !strings.HasSuffix(got, "\nAI: no project yet (inert)") {
		t.Fatal(got)
	}
	m.applyLinearBinding(LinearData{BindingV2: &b, WithoutProject: []linearboard.TeamWithoutProject{{TeamID: "team-two", Name: "AI"}}})
	if got := linearBoardLine(m); !strings.HasSuffix(got, "\nAI: a bound project carries it (stale entry; the next link drops it)") {
		t.Fatal(got)
	}
	m.applyLinearBinding(LinearData{BindingV2: &b})
	if got := linearBoardLine(m); strings.Contains(got, "\n") {
		t.Fatal("a line for a team no read reported:", got)
	}
}
