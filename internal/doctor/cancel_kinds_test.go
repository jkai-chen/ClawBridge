package doctor

import (
	"context"
	"strings"
	"testing"
)

// AGENT-CANCEL R10: who may cancel is a pass line; a listed kind that is
// no longer a kind, or no kind setting, is a warning; an older server's
// overview adds no check. Local (typed) and remote (decoded) answers read
// the same.
func TestCheckCancelKinds(t *testing.T) {
	run := func(policy map[string]any) (Check, bool) {
		var got Check
		added := false
		checkCancelKinds(context.Background(), overviewExecutor{data: map[string]any{"linear": map[string]any{"status_policy": policy}}}, "w", "p", func(c Check) { got, added = c, true })
		return got, added
	}
	for name, c := range map[string]struct {
		policy  map[string]any
		status  string
		message string
		action  string
	}{
		"never":         {map[string]any{"agent_may_cancel_kinds": []string{}}, Pass, "Agents never cancel issues.", ""},
		"typed":         {map[string]any{"agent_may_cancel_kinds": []string{"Engineering"}}, Pass, "Agents may cancel or mark Duplicate in projects of kind Engineering.", ""},
		"decoded":       {map[string]any{"agent_may_cancel_kinds": []any{"Engineering", "Research"}}, Pass, "Agents may cancel or mark Duplicate in projects of kind Engineering, Research.", ""},
		"stale kind":    {map[string]any{"agent_may_cancel_kinds": []any{"Engineering", "Ops"}, "cancel_kinds_unknown": []any{"Ops"}}, Warn, "Ops is no longer a kind of this project.", "cancel-kinds show"},
		"no kind setup": {map[string]any{"agent_may_cancel_kinds": []string{"Engineering"}, "cancel_kinds_no_setting": true}, Warn, "This project has no kind setting", "clawbridge linear kind set"},
	} {
		got, added := run(c.policy)
		if !added || got.ID != "linear.cancel_kinds" || got.Status != c.status || !strings.Contains(got.Message, c.message) || !strings.Contains(got.Action, c.action) {
			t.Fatal(name, got)
		}
	}
	if _, added := run(map[string]any{"enabled": true, "revision": 1}); added {
		t.Fatal("an older server's overview added a check")
	}
	var added bool
	checkCancelKinds(context.Background(), overviewExecutor{err: context.Canceled}, "w", "p", func(Check) { added = true })
	if added {
		t.Fatal("a failed read added a check")
	}
}
