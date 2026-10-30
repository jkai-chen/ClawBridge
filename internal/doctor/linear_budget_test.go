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

// When the app spent more than half the hour, the line names the top three
// kinds, so the job behind the app's calls is known; at half or less, or
// with no kinds (an older server), it doesn't.
func TestCheckLinearBudgetNamesTheTopKinds(t *testing.T) {
	run := func(b any) Check {
		var got Check
		checkLinearBudget(context.Background(), overviewExecutor{data: map[string]any{"linear_budget": b}}, "w", "p", func(c Check) { got = c })
		return got
	}
	kinds := []bus.LinearBudgetKind{{Kind: "app_write.update", Calls: 64, Events: 4}, {Kind: "watch.comments", Calls: 40, Events: 12}, {Kind: "watch.health", Calls: 10, Events: 1}, {Kind: "watch.docs", Calls: 2, Events: 2}}
	typed := bus.LinearBudget{Used: 120, By: bus.LinearBudgetSplit{App: 116, People: 4}, Kinds: kinds}
	decoded := map[string]any{"used": 120.0, "limit": 0.0, "capped": false, "by": map[string]any{"app": 116.0, "people": 4.0}, "kinds": []any{
		map[string]any{"kind": "app_write.update", "calls": 64.0, "events": 4.0}, map[string]any{"kind": "watch.comments", "calls": 40.0, "events": 12.0},
		map[string]any{"kind": "watch.health", "calls": 10.0, "events": 1.0}, map[string]any{"kind": "watch.docs", "calls": 2.0, "events": 2.0}}}
	for _, b := range []any{typed, decoded} {
		if c := run(b); c.Status != Pass || !strings.HasSuffix(c.Message, "agent sessions 0. Most calls: app_write.update 64, watch.comments 40, watch.health 10.") {
			t.Fatal(c.Message)
		}
	}
	capped := typed
	capped.Limit, capped.Capped = 600, true
	if c := run(capped); !strings.HasPrefix(c.Message, "This hour 120 of 600") || !strings.HasSuffix(c.Message, " Most calls: app_write.update 64, watch.comments 40, watch.health 10.") {
		t.Fatal(c.Message)
	}
	// Exactly half is not more than half.
	half := bus.LinearBudget{Used: 120, By: bus.LinearBudgetSplit{App: 60, Agents: 60}, Kinds: kinds}
	if c := run(half); strings.Contains(c.Message, "Most calls") {
		t.Fatal(c.Message)
	}
	if c := run(bus.LinearBudget{Used: 120, By: bus.LinearBudgetSplit{App: 120}}); strings.Contains(c.Message, "Most calls") {
		t.Fatal(c.Message)
	}
}
