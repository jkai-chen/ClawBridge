package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

// FR8: doctor splits the hour's Linear calls between the app, people and
// agent sessions, from a local (typed) or remote (decoded) overview; under
// an opt-in cap it warns, never fails, when agent sessions spent more than
// half the hour; without one (the default) it says so;
// an older server's overview, or a failed read, adds no check.
func TestCheckLinearBudget(t *testing.T) {
	run := func(o overviewExecutor) (Check, bool) {
		var got Check
		added := false
		checkLinearBudget(context.Background(), o, "w", "p", func(c Check) { got, added = c, true })
		return got, added
	}
	typed := bus.LinearBudget{Used: 120, Limit: 600, Capped: true, By: bus.LinearBudgetSplit{App: 80, People: 10, Agents: 30}}
	decoded := map[string]any{"used": 120.0, "limit": 600.0, "capped": true, "source": "project", "by": map[string]any{"app": 80.0, "people": 10.0, "agents": 30.0}}
	for _, b := range []any{typed, decoded} {
		c, _ := run(overviewExecutor{data: map[string]any{"linear_budget": b}})
		if c.ID != "linear.budget" || c.Status != Pass || c.Message != "This hour 120 of 600 Linear calls are used: the app 80, people 10, agent sessions 30." {
			t.Fatal(c)
		}
	}
	c, _ := run(overviewExecutor{data: map[string]any{"linear_budget": bus.LinearBudget{Used: 400, Limit: 600, Capped: true, By: bus.LinearBudgetSplit{App: 99, People: 0, Agents: 301}}}})
	if c.Status != Warn || !strings.Contains(c.Action, "more than half") {
		t.Fatal(c)
	}
	// No cap, the default (FR8 r3): counted, never warned about.
	for _, b := range []any{bus.LinearBudget{Used: 900, By: bus.LinearBudgetSplit{App: 100, Agents: 800}}, map[string]any{"used": 900.0, "limit": 0.0, "capped": false, "source": "default", "by": map[string]any{"app": 100.0, "agents": 800.0}}} {
		c, _ = run(overviewExecutor{data: map[string]any{"linear_budget": b}})
		if c.Status != Pass || c.Message != "This hour 900 Linear calls were made, with no ClawBridge cap; Linear's own rate limit is respected through the queue: the app 100, people 0, agent sessions 800." {
			t.Fatal(c)
		}
	}
	if _, added := run(overviewExecutor{data: map[string]any{"project": map[string]any{}}}); added {
		t.Fatal("an older server's overview added a check")
	}
	if _, added := run(overviewExecutor{err: errors.New("down")}); added {
		t.Fatal("a failed read added a check")
	}
}
