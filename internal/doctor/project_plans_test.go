package doctor

import (
	"context"
	"strings"
	"testing"
)

// Project plans are counted; one Linear answered with other fields changed is
// a warning naming the operation and the fields, as a local (typed) or
// remote (decoded) overview gives it; an older server gets no check.
func TestCheckProjectPlans(t *testing.T) {
	run := func(o overviewExecutor) (Check, bool) {
		var got Check
		added := false
		checkProjectPlans(context.Background(), o, "w", "p", func(c Check) { got, added = c, true })
		return got, added
	}
	c, _ := run(overviewExecutor{data: map[string]any{"project_plans": map[string]any{"planned": 2, "with_changes": 0, "recent": []map[string]any{}}}})
	if c.ID != "linear.project_plans" || c.Status != Pass || !strings.Contains(c.Message, "2 project plan(s)") {
		t.Fatal(c)
	}
	for _, plans := range []map[string]any{
		{"planned": 2, "with_changes": 1, "recent": []map[string]any{{"operation_id": "q-1", "state": "accepted"}, {"operation_id": "q-2", "state": "accepted_with_changes", "changed": []string{"lead", "members"}}, {"operation_id": "q-0", "state": "accepted_with_changes", "changed": []string{"lead"}, "acknowledged_at": "2026-10-29T00:00:00Z"}}},
		{"planned": float64(2), "with_changes": float64(1), "recent": []any{map[string]any{"operation_id": "q-1", "state": "accepted"}, map[string]any{"operation_id": "q-2", "state": "accepted_with_changes", "changed": []any{"lead", "members"}}}},
	} {
		c, _ = run(overviewExecutor{data: map[string]any{"project_plans": plans}})
		if c.Status != Warn || !strings.Contains(c.Message, "q-2 (lead, members)") || strings.Contains(c.Message, "q-1") || strings.Contains(c.Message, "q-0") || !strings.Contains(c.Action, "writes --ack") {
			t.Fatal(c)
		}
	}
	if _, added := run(overviewExecutor{data: map[string]any{"project": map[string]any{}}}); added {
		t.Fatal("an older server's overview added a check")
	}
}
