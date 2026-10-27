package bus

import (
	"context"
	"strings"
	"testing"
)

// The project's budget wins over the watcher's from its next call, on the
// lease already held and on a takeover; clearing it gives the watcher's
// back. Every change is audited.
func TestProjectWatchBudget(t *testing.T) {
	status := func(h *harness) map[string]any {
		h.t.Helper()
		return h.call("comment_watch_status", "", nil).(map[string]any)
	}
	if got := status(newHarness(t)); got["budget_source"] != nil || got["budget_limit"] != nil || got["watch_budget"] != nil {
		t.Fatal("no lease, no override:", got)
	}
	w := newWatchHarness(t)
	if got := status(w.harness); got["budget_source"] != "watcher" || got["budget_limit"] != 200 || got["watch_budget"] != nil {
		t.Fatal("the watcher's default:", got)
	}
	set := w.call("comment_watch_budget_set", "", map[string]any{"watch_budget": 5}).(map[string]any)
	if set["budget_source"] != "project" || set["budget_limit"] != 5 || set["watch_budget"] != 5 {
		t.Fatal("set:", set)
	}
	if due := w.call("comment_watch_due", "c", w.fenced(map[string]any{})).(map[string]any); due["budget_limit"] != 5 {
		t.Fatal("the held lease kept its own budget:", due)
	}
	if _, e := w.s.db.Exec(`UPDATE watch_lease SET expires_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		t.Fatal(e)
	}
	l := w.call("comment_watch_lease", "b", map[string]any{"action": "acquire", "budget": 300}).(map[string]any)
	if l["budget_limit"] != 5 {
		t.Fatal("a client's --budget beat the project's:", l)
	}
	fence := map[string]any{"lease_token": l["lease_token"], "generation": l["generation"]}
	if r := w.call("comment_watch_lease", "b", map[string]any{"action": "renew", "lease_token": l["lease_token"], "generation": l["generation"]}).(map[string]any); r["budget_limit"] != 5 {
		t.Fatal("renew:", r)
	}
	// A session may set it too; null clears it.
	cleared := w.call("comment_watch_budget_set", "a", map[string]any{"watch_budget": nil}).(map[string]any)
	if cleared["budget_source"] != "watcher" || cleared["budget_limit"] != 300 || cleared["watch_budget"] != nil {
		t.Fatal("clear:", cleared)
	}
	if due := w.call("comment_watch_due", "b", fence).(map[string]any); due["budget_limit"] != 300 {
		t.Fatal("cleared, the lease's own budget did not come back:", due)
	}
	for name, a := range map[string]map[string]any{
		"zero":     {"watch_budget": 0},
		"over":     {"watch_budget": watchMaxBudget + 1},
		"negative": {"watch_budget": -1},
		"text":     {"watch_budget": "100"},
		"missing":  {},
	} {
		if _, e := w.s.Execute(context.Background(), w.request("comment_watch_budget_set", "", a)); e == nil || !strings.Contains(e.Error(), "watch_budget") {
			t.Errorf("%s: %v", name, e)
		}
	}
	if got := w.call("comment_watch_budget_set", "", map[string]any{"watch_budget": watchMaxBudget}).(map[string]any); got["budget_limit"] != watchMaxBudget {
		t.Fatal("the maximum:", got)
	}
	if got := w.actions("watch_budget"); strings.Join(got, ",") != "comment_watch_budget_set,comment_watch_budget_set,comment_watch_budget_set" {
		t.Fatalf("history %v", got)
	}
}
