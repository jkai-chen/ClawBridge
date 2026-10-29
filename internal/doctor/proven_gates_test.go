package doctor

import (
	"context"
	"errors"
	"testing"
)

// The owner's doctor names the PROVEN gates that are on and those that are
// off, from a local (typed) or remote (decoded) overview; a member's
// overview, an older server's or a failed read adds no check.
func TestCheckProvenGates(t *testing.T) {
	run := func(o overviewExecutor) (Check, bool) {
		var got Check
		added := false
		checkProvenGates(context.Background(), o, "w", "p", func(c Check) { got, added = c, true })
		return got, added
	}
	gate := func(name string, on bool) map[string]any { return map[string]any{"name": name, "on": on} }
	for _, gates := range []any{
		[]map[string]any{gate("project_flow", false), gate("issue_subscribers", true), gate("issue_move", true), gate("project_team_add", false)},
		[]any{gate("project_flow", false), gate("issue_subscribers", true), gate("issue_move", true), gate("project_team_add", false)},
	} {
		c, _ := run(overviewExecutor{data: map[string]any{"proven_gates": gates}})
		if c.ID != "linear.proven_gates" || c.Title != "Proven Linear gates" || c.Status != Pass || c.Message != "Proven gates: issue_subscribers, issue_move (off: project_flow, project_team_add)." {
			t.Fatal(c)
		}
	}
	c, _ := run(overviewExecutor{data: map[string]any{"proven_gates": []any{gate("project_flow", false), gate("issue_move", false)}}})
	if c.Message != "Proven gates: none on (off: project_flow, issue_move)." {
		t.Fatal(c)
	}
	c, _ = run(overviewExecutor{data: map[string]any{"proven_gates": []any{gate("project_flow", true), gate("issue_move", true)}}})
	if c.Message != "Proven gates: project_flow, issue_move." {
		t.Fatal(c)
	}
	if _, added := run(overviewExecutor{data: map[string]any{"project": map[string]any{}}}); added {
		t.Fatal("a member's or older server's overview added a check")
	}
	if _, added := run(overviewExecutor{err: errors.New("down")}); added {
		t.Fatal("a failed read added a check")
	}
}
