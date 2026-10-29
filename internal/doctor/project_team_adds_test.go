package doctor

import (
	"context"
	"strings"
	"testing"
)

// Team adds are counted; one Linear answered with other fields changed is
// a warning naming the operation and the fields, as a local (typed) or
// remote (decoded) overview gives it; an older server adds no check.
func TestCheckProjectTeamAdds(t *testing.T) {
	run := func(o overviewExecutor) (Check, bool) {
		var got Check
		added := false
		checkProjectTeamAdds(context.Background(), o, "w", "p", func(c Check) { got, added = c, true })
		return got, added
	}
	c, _ := run(overviewExecutor{data: map[string]any{"project_team_adds": map[string]any{"added": 2, "with_changes": 0, "recent": []map[string]any{}}}})
	if c.ID != "linear.project_team_adds" || c.Status != Pass || !strings.Contains(c.Message, "2 team(s)") {
		t.Fatal(c)
	}
	for _, adds := range []map[string]any{
		{"added": 2, "with_changes": 1, "recent": []map[string]any{{"operation_id": "t-1", "state": "accepted"}, {"operation_id": "t-2", "state": "accepted_with_changes", "changed": []string{"lead", "members"}}, {"operation_id": "t-0", "state": "accepted_with_changes", "changed": []string{"lead"}, "acknowledged_at": "2026-10-29T00:00:00Z"}}},
		{"added": float64(2), "with_changes": float64(1), "recent": []any{map[string]any{"operation_id": "t-1", "state": "accepted"}, map[string]any{"operation_id": "t-2", "state": "accepted_with_changes", "changed": []any{"lead", "members"}}}},
	} {
		c, _ = run(overviewExecutor{data: map[string]any{"project_team_adds": adds}})
		if c.Status != Warn || !strings.Contains(c.Message, "t-2 (lead, members)") || strings.Contains(c.Message, "t-1") || strings.Contains(c.Message, "t-0") || !strings.Contains(c.Action, "writes --ack") {
			t.Fatal(c)
		}
	}
	if _, added := run(overviewExecutor{data: map[string]any{"project": map[string]any{}}}); added {
		t.Fatal("an older server's overview added a check")
	}
}
