package doctor

import (
	"context"
	"strings"
	"testing"
)

// Milestone plans are counted; one Linear answered with other fields changed
// is a warning naming the operation and the fields, as a local (typed) or
// remote (decoded) overview gives it; an older server gets no check, and the
// project-plan check reads its own counts.
func TestCheckMilestonePlans(t *testing.T) {
	run := func(o overviewExecutor) (Check, bool) {
		var got Check
		added := false
		checkMilestonePlans(context.Background(), o, "w", "p", func(c Check) { got, added = c, true })
		return got, added
	}
	c, _ := run(overviewExecutor{data: map[string]any{"milestone_plans": map[string]any{"planned": 2, "with_changes": 0, "recent": []map[string]any{}}}})
	if c.ID != "linear.milestone_plans" || c.Status != Pass || !strings.Contains(c.Message, "2 milestone plan(s)") {
		t.Fatal(c)
	}
	for _, plans := range []map[string]any{
		{"planned": 2, "with_changes": 1, "recent": []map[string]any{{"operation_id": "q-1", "state": "accepted"}, {"operation_id": "q-2", "state": "accepted_with_changes", "changed": []string{"sortOrder", "targetDate"}}, {"operation_id": "q-0", "state": "accepted_with_changes", "changed": []string{"sortOrder"}, "acknowledged_at": "2026-11-02T00:00:00Z"}}},
		{"planned": float64(2), "with_changes": float64(1), "recent": []any{map[string]any{"operation_id": "q-1", "state": "accepted"}, map[string]any{"operation_id": "q-2", "state": "accepted_with_changes", "changed": []any{"sortOrder", "targetDate"}}}},
	} {
		c, _ = run(overviewExecutor{data: map[string]any{"milestone_plans": plans}})
		if c.Status != Warn || !strings.Contains(c.Message, "q-2 (sortOrder, targetDate)") || strings.Contains(c.Message, "q-1") || strings.Contains(c.Message, "q-0") || !strings.Contains(c.Action, "writes --ack") || !strings.Contains(c.Action, "milestone fields") {
			t.Fatal(c)
		}
	}
	if _, added := run(overviewExecutor{data: map[string]any{"project_plans": map[string]any{"planned": 1}}}); added {
		t.Fatal("the project plans' counts were read as milestones'")
	}
	if _, added := run(overviewExecutor{data: map[string]any{"project": map[string]any{}}}); added {
		t.Fatal("an older server's overview added a check")
	}
	// The proven gate is listed with the others.
	var gate Check
	checkProvenGates(context.Background(), overviewExecutor{data: map[string]any{"proven_gates": []any{map[string]any{"name": "project_plan", "on": true}, map[string]any{"name": "milestone_plan", "on": false}}}}, "w", "p", func(c Check) { gate = c })
	if gate.Message != "Proven gates: project_plan (off: milestone_plan)." {
		t.Fatal(gate)
	}
}
