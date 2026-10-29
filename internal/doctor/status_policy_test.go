package doctor

import (
	"context"
	"strings"
	"testing"
)

// A bound team the status policy lacks is a warning naming it (by name
// when the overview carries one), from a local (typed) or remote (decoded)
// overview; a policy that is off, complete, or an older server's overview
// adds no check.
func TestCheckStatusPolicyTeams(t *testing.T) {
	run := func(data any) (Check, bool) {
		var got Check
		added := false
		checkStatusPolicyTeams(context.Background(), overviewExecutor{data: data}, "w", "p", func(c Check) { got, added = c, true })
		return got, added
	}
	const idA, idB = "3333aaaa-3333-4333-8333-333333333331", "3333aaaa-3333-4333-8333-333333333332"
	typed := map[string]any{"linear": map[string]any{
		"teams":         []map[string]any{{"id": idA, "name": "Alpha"}, {"id": idB, "name": ""}},
		"status_policy": map[string]any{"enabled": true, "teams_missing": []string{idA, idB}},
	}}
	decoded := map[string]any{"linear": map[string]any{
		"teams":         []any{map[string]any{"id": idA, "name": "Alpha"}, map[string]any{"id": idB, "name": ""}},
		"status_policy": map[string]any{"enabled": true, "teams_missing": []any{idA, idB}},
	}}
	for _, data := range []any{typed, decoded} {
		c, added := run(data)
		if !added || c.ID != "linear.status_policy" || c.Title != "Status policy teams" || c.Status != Warn ||
			c.Message != "Bound team(s) Alpha, "+idB+" are not in the status policy: claims and done-releases there don't move the issue's state." ||
			!strings.Contains(c.Action, "clawbridge linear status-policy set --file FILE --expected-revision N") {
			t.Fatal(c)
		}
	}
	for name, data := range map[string]any{
		"none missing": map[string]any{"linear": map[string]any{"status_policy": map[string]any{"enabled": true}}},
		"older server": map[string]any{"linear": map[string]any{"status_policy": map[string]any{"enabled": true, "revision": 1}}},
		"no linear":    map[string]any{"project": map[string]any{}},
		"unconfigured": map[string]any{"linear": map[string]any{"configured": false}},
	} {
		if _, added := run(data); added {
			t.Fatal(name, "added a check")
		}
	}
	if _, added := (func() (Check, bool) {
		var got Check
		ok := false
		checkStatusPolicyTeams(context.Background(), overviewExecutor{err: context.Canceled}, "w", "p", func(c Check) { got, ok = c, true })
		return got, ok
	})(); added {
		t.Fatal("a failed read added a check")
	}
}
