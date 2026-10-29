package watcher

import (
	"context"
	"slices"
	"testing"

	"clawbridge/internal/bus"
)

// S1 reads nothing of a team listed without a project while no bound
// project carries it: the poll names it inert, not unreadable, and its
// issues are never queued.
func TestIntakeS1SkipsATeamWithoutProject(t *testing.T) {
	const inert = "abababab-abab-4bab-8bab-abababababab"
	e := setup(t)
	e.f.projects = map[string]bool{linearProjectID(1): true}
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 1, "mode": "project_set", "linear_team_ids": []string{team, inert}, "linear_default_team_id": team, "linear_project_ids": []string{linearProjectID(1)}, "teams_without_project": []string{inert}}}); err != nil {
		t.Fatal(err)
	}
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	e.tick()
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Unreadable) != 0 || !slices.Equal(tick.Intake.Inert, []string{inert}) {
		t.Fatalf("intake: %+v", tick.Intake)
	}
}
