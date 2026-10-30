package linearboard

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// AGENT-CANCEL P1: the field survives every place a status policy passes
// through, on a real store: set with the op, read back, kept by a
// status-policy file that leaves the key out, cleared by one with [], and
// a file that adds a kind is refused before anything is sent.
func TestCancelKindsThreadThroughTheStatusPolicy(t *testing.T) {
	store, e := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	for _, r := range []bus.Request{
		{Operation: "workspace_init", Workspace: "ws", Args: map[string]any{"mode": "local"}},
		{Operation: "project_create", Workspace: "ws", Args: map[string]any{"slug": "prj"}},
		{Operation: "taskboard_set_v2", Workspace: "ws", Project: "prj", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{teamID}, "linear_default_team_id": teamID, "linear_project_ids": []string{projectA}}},
		{Operation: "project_kind_set", Workspace: "ws", Project: "prj", Args: map[string]any{"expected_revision": 0, "kinds": []string{"Engineering", "Research"}}},
	} {
		if _, e := store.Execute(ctx, r); e != nil {
			t.Fatal(r.Operation, e)
		}
	}
	s, _, p, _ := testV2(t)
	s.cfg.Executor = store
	p.statuses = append([]linearprovider.Status{}, sfStates...)
	scope := bus.Request{Workspace: "ws", Project: "prj", Args: map[string]any{}}
	if _, e = s.SetStatusPolicyV2(ctx, scope, []byte(`{"enabled": true, "claim": "In Progress"}`), 0, nil); e != nil {
		t.Fatal(e)
	}
	if _, e = s.SetCancelKindsV2(ctx, scope, []string{"engineering"}, 1, ClosesViaTTY); e != nil {
		t.Fatal(e)
	}
	view, e := s.CancelKindsV2(ctx, scope)
	if e != nil || view.Revision != 2 || !reflect.DeepEqual(view.CancelKinds, []string{"Engineering"}) || !reflect.DeepEqual(view.Kinds, []string{"Engineering", "Research"}) || view.Line != "Agents may cancel or mark Duplicate in projects of kind Engineering." {
		t.Fatal(view, e)
	}
	if policy, e := s.statusPolicy(ctx, scope); e != nil || !reflect.DeepEqual(policy.AgentMayCancelKinds, []string{"Engineering"}) {
		t.Fatal("decodeStatusPolicy dropped it:", policy.AgentMayCancelKinds, e)
	}
	out, e := s.SetStatusPolicyV2(ctx, scope, []byte(`{"enabled": true}`), 2, nil)
	if e != nil {
		t.Fatal(e)
	}
	if kept, _ := out.(map[string]any)["kept"].([]string); !strings.Contains(strings.Join(kept, ","), "agent_may_cancel_kinds") {
		t.Fatal("kept:", out)
	}
	if view, _ = s.CancelKindsV2(ctx, scope); !reflect.DeepEqual(view.CancelKinds, []string{"Engineering"}) {
		t.Fatal("a file without the key dropped it:", view)
	}
	// Spelled differently, the same list is no addition.
	if _, e = s.SetStatusPolicyV2(ctx, scope, []byte(`{"enabled": true, "agent_may_cancel_kinds": ["ENGINEERING"]}`), 3, nil); e != nil {
		t.Fatal(e)
	}
	if _, e = s.SetStatusPolicyV2(ctx, scope, []byte(`{"enabled": true, "agent_may_cancel_kinds": ["Engineering", "Research"]}`), 4, nil); e == nil || !strings.Contains(e.Error(), "adds Research to agent_may_cancel_kinds") || !strings.Contains(e.Error(), "cancel-kinds set") {
		t.Fatal("a file added a kind:", e)
	}
	if _, e = s.SetStatusPolicyV2(ctx, scope, []byte(`{"enabled": true, "agent_may_cancel_kinds": []}`), 4, nil); e != nil {
		t.Fatal(e)
	}
	if view, _ = s.CancelKindsV2(ctx, scope); view.Revision != 5 || len(view.CancelKinds) != 0 || view.Line != "Agents never cancel issues." {
		t.Fatal("[] did not clear it:", view)
	}
}

// A typed name is matched to the project's kinds, spelled as they are.
func TestCancelKindsMatch(t *testing.T) {
	got, e := CancelKindsMatch([]string{" engineering", "Engineering", ""}, []string{"Engineering", "Research"})
	if e != nil || !reflect.DeepEqual(got, []string{"Engineering"}) {
		t.Fatal(got, e)
	}
	if _, e = CancelKindsMatch([]string{"Ops"}, []string{"Engineering"}); e == nil || !strings.Contains(e.Error(), `"Ops" is not a kind`) {
		t.Fatal(e)
	}
	if _, e = CancelKindsMatch([]string{"Ops"}, nil); e == nil || !strings.Contains(e.Error(), "linear kind set") {
		t.Fatal(e)
	}
}
