package linearboard

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

// SetLanes sends the narrow operation without a session, refuses a session
// before sending anything, and leaves the rest of the binding as it was.
func TestSetLanesIsThePersonsAndNarrow(t *testing.T) {
	store, e := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	const teamA, teamB = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	for _, r := range []bus.Request{
		{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}},
		{Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "p"}},
		{Operation: "register_session", Workspace: "w", Project: "p", Session: "own", Args: map[string]any{"instance": "own-i"}},
		{Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "mode": "team_board", "linear_team_ids": []string{teamA, teamB}, "linear_default_team_id": teamA, "linear_project_ids": []string{}}},
	} {
		if _, e := store.Execute(ctx, r); e != nil {
			t.Fatal(r.Operation, e)
		}
	}
	s := New(Config{Executor: store})
	lanes := []bus.Lane{{Name: "ai", WriteTeamIDs: []string{teamA}, DefaultTeamID: teamA}, {Name: "platform", WriteTeamIDs: []string{teamB, teamA}, DefaultTeamID: teamB}}
	person := bus.Request{Workspace: "w", Project: "p"}
	session := bus.Request{Workspace: "w", Project: "p", Session: "own", Args: map[string]any{"instance": "own-i"}}
	if _, e = s.SetLanes(ctx, session, LanesSetInput{Lanes: lanes, HaveLanes: true, Expected: 1}); e == nil || !strings.Contains(e.Error(), "not from an agent session") {
		t.Fatal("a session set the lanes:", e)
	}
	if _, e = s.SetLanes(ctx, person, LanesSetInput{Lanes: lanes, HaveLanes: true, Expected: -1}); e == nil {
		t.Fatal("no revision")
	}
	if _, e = s.SetLanes(ctx, person, LanesSetInput{Expected: 1}); e == nil {
		t.Fatal("nothing to set")
	}
	if _, e = s.SetLanes(ctx, person, LanesSetInput{Lanes: lanes, HaveLanes: true, Expected: 0}); e == nil || !strings.Contains(e.Error(), "read it again") {
		t.Fatal("a stale revision was accepted:", e)
	}
	data, e := s.SetLanes(ctx, person, LanesSetInput{Lanes: lanes, HaveLanes: true, Expected: 1})
	if e != nil {
		t.Fatal(e)
	}
	b, e := bus.ParseTaskboardBindingV2(data)
	// Unpinned is read-only when only lanes were named.
	if e != nil || b.Revision != 2 || len(b.Lanes) != 2 || b.Unpinned != bus.LaneReadOnly || len(b.LinearTeamIDs) != 2 || b.LinearDefaultTeamID != teamA || b.Mode != "team_board" {
		t.Fatal(b, e)
	}
	if data, e = s.SetLanes(ctx, person, LanesSetInput{Unpinned: "ai", Expected: 2}); e != nil {
		t.Fatal(e)
	}
	if b, e = bus.ParseTaskboardBindingV2(data); e != nil || b.Unpinned != "ai" || len(b.Lanes) != 2 {
		t.Fatal(b, e)
	}
}
