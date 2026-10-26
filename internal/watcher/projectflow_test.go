package watcher

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

const (
	wsBacklog  = "abab0001-0000-4000-8000-000000000001"
	wsStartedB = "abab0002-0000-4000-8000-000000000002"
	wsStartedA = "abab0003-0000-4000-8000-000000000003"
	wsArchived = "abab0004-0000-4000-8000-000000000004"
	wsDone     = "abab0005-0000-4000-8000-000000000005"
)

// flowSetup binds project p to one Linear project with health off, and
// turns project flow on in an enabled status policy on a proven server.
// The project is in backlog; its statuses list two started ones, the
// archived one first by position.
func flowSetup(t *testing.T, policy map[string]any) *env {
	t.Helper()
	e := setup(t)
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 1, "mode": "project_set", "linear_team_ids": []string{team}, "linear_default_team_id": team, "linear_project_ids": []string{linearProjectID(1)}}}); err != nil {
		t.Fatal(err)
	}
	e.s.SetProjectFlowProven(true)
	if policy == nil {
		policy = map[string]any{"enabled": true, "teams": map[string]any{}, "project_flow": true}
	}
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "status_policy_set", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "status_policy": policy}}); err != nil {
		t.Fatal(err)
	}
	e.f.projectStatus, e.f.saveLands = wsBacklog, true
	e.f.statuses = []map[string]any{
		{"id": wsBacklog, "name": "Backlog", "type": "backlog", "position": 0, "archivedAt": nil},
		{"id": wsArchived, "name": "Old start", "type": "started", "position": 1, "archivedAt": "2026-02-01T00:00:00.000Z"},
		{"id": wsStartedB, "name": "In Progress", "type": "started", "position": 3, "archivedAt": nil},
		{"id": wsStartedA, "name": "Kickoff", "type": "started", "position": 2, "archivedAt": nil},
		{"id": wsDone, "name": "Completed", "type": "completed", "position": 4, "archivedAt": nil},
	}
	e.f.healthIssues = []map[string]any{healthIssue(issueA, "FIX-1", "started", time.Now(), ""), healthIssue(issueB, "FIX-2", "unstarted", time.Now(), "")}
	return e
}

func (e *env) flowDue() {
	e.t.Helper()
	e.sql(`UPDATE project_flow_state SET next_flow_at='2000-01-01T00:00:00.000000000Z'`)
}

// With health off, a flow run reads the project and its issues only, moves
// the project by the lowest-position live started status's ID as the app,
// and reads it back: one save, no activity reads.
func TestProjectFlowMovesAStartedProject(t *testing.T) {
	e := flowSetup(t, nil)
	tick := e.tick()
	if len(tick.Health) != 1 || tick.Health[0].Flow != "move" || tick.Health[0].Moved != "accepted" || tick.Health[0].Health != "" {
		t.Fatal(tick.Health, tick.Error)
	}
	if len(e.f.saves) != 1 || e.f.saves[0] != linearProjectID(1)+" "+wsStartedA {
		t.Fatal("saved:", e.f.saves)
	}
	for _, q := range e.f.queries {
		if q == healthActivityQuery || q == healthProjectQuery {
			t.Fatal("a flow-only run read health's queries:", q)
		}
	}
	if !strings.Contains(strings.Join(e.f.queries, "\n"), flowReadBackQuery) {
		t.Fatal("no read-back")
	}
	// Moved: the next run sees started and moves nothing more.
	e.flowDue()
	if tick = e.tick(); len(tick.Health) != 1 || tick.Health[0].Flow != "none" || len(e.f.saves) != 1 {
		t.Fatal(tick.Health, e.f.saves)
	}
}

// The policy's override wins.
func TestProjectFlowOverrideWins(t *testing.T) {
	e := flowSetup(t, map[string]any{"enabled": true, "teams": map[string]any{}, "project_flow": true, "project_targets": map[string]any{"started": wsStartedB}})
	e.tick()
	if len(e.f.saves) != 1 || !strings.HasSuffix(e.f.saves[0], wsStartedB) {
		t.Fatal(e.f.saves)
	}
}

// A read-back that doesn't show the status is uncertain: never sent again,
// and the next read settles it.
func TestProjectFlowReadBackMismatchIsUncertain(t *testing.T) {
	e := flowSetup(t, nil)
	e.f.saveLands = false
	if tick := e.tick(); tick.Health[0].Moved != "pending" {
		t.Fatal(tick.Health)
	}
	e.flowDue()
	if tick := e.tick(); tick.Health[0].Flow != "pending" || len(e.f.saves) != 1 {
		t.Fatal("sent twice:", tick.Health, e.f.saves)
	}
	e.f.projectStatus = wsStartedA
	e.flowDue()
	e.tick()
	var state string
	e.queryRow(`SELECT state FROM project_flow_moves`, &state)
	if state != "accepted" || len(e.f.saves) != 1 {
		t.Fatal(state, e.f.saves)
	}
}

// Without the write scope nothing is sent, and the coordinator is told.
func TestProjectFlowWithoutWriteScopeRefusesBeforeSending(t *testing.T) {
	e := flowSetup(t, nil)
	e.f.noWrite = true
	if tick := e.tick(); tick.Health[0].Moved != "refused" || len(e.f.saves) != 0 {
		t.Fatal(tick.Health, e.f.saves)
	}
	notes := e.inbox("coord", "project-flow")
	if len(notes) != 1 || !strings.Contains(fmt.Sprint(notes[0]["body"]), "lacks Linear's write scope (app_scope)") {
		t.Fatal(notes)
	}
}

// With health on too, one run serves both from the flow reads.
func TestProjectFlowRidesTheHealthRun(t *testing.T) {
	e := flowSetup(t, nil)
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "health_set", Workspace: "w", Project: "p", Args: map[string]any{"linear_project": linearProjectID(1), "expected_revision": 0, "health_policy": map[string]any{"enabled": true}}}); err != nil {
		t.Fatal(err)
	}
	e.healthDue()
	e.f.activity = map[string]map[string]any{issueA: activity(trustedUser, "", time.Now().Add(-time.Hour))}
	tick := e.tick()
	if len(tick.Health) != 1 || tick.Health[0].Health == "" || tick.Health[0].Moved != "accepted" || len(e.f.saves) != 1 {
		t.Fatal(tick.Health, tick.Error)
	}
}
