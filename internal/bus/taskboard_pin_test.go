package bus

import (
	"context"
	"strings"
	"testing"
)

// PR7-L1: the authority keeps a binding's Linear workspace pin. A set must
// name the same workspace; only unlinking resets it.
func TestTaskboardSetKeepsTheWorkspacePin(t *testing.T) {
	h := newHarness(t)
	const team = "11111111-1111-4111-8111-111111111111"
	const ws = "f0000000-0000-4000-8000-0000000000f1"
	set := func(rev int, workspace any) string {
		a := map[string]any{"expected_revision": rev, "mode": "team_board", "linear_team_ids": []any{team}, "linear_default_team_id": team, "linear_project_ids": []any{}}
		if workspace != nil {
			a["linear_workspace_id"] = workspace
		}
		if _, e := h.s.Execute(context.Background(), h.request("taskboard_set_v2", "a", a)); e != nil {
			return e.Error()
		}
		return ""
	}
	if e := set(0, ws); e != "" {
		t.Fatal(e)
	}
	for name, workspace := range map[string]any{"cleared": nil, "changed": "f0000000-0000-4000-8000-0000000000f2"} {
		if e := set(1, workspace); !strings.Contains(e, "pinned") {
			t.Fatal(name, "pin not kept:", e)
		}
	}
	if e := set(1, ws); e != "" {
		t.Fatal("same workspace refused", e)
	}
	h.call("taskboard_clear_v2", "a", map[string]any{"expected_revision": 2})
	if e := set(3, "f0000000-0000-4000-8000-0000000000f2"); e != "" {
		t.Fatal("unlink did not reset the pin", e)
	}
}
