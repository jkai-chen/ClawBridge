package watcher

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// kindsStore answers each project's hour of kinds from a table; a
// project it doesn't know fails, as a store that can't say.
type kindsStore struct {
	Store
	kinds map[string][]bus.LinearBudgetKind
	asked []string
}

func (k *kindsStore) LinearBudgetKindsAt(_ context.Context, pid, hour string) ([]bus.LinearBudgetKind, error) {
	k.asked = append(k.asked, pid+" "+hour)
	got, ok := k.kinds[pid]
	if !ok {
		return nil, fmt.Errorf("unavailable")
	}
	return got, nil
}

// Each project's hour is logged once, when a later pass sees the hour has
// ended: the last budget read, the passes the budget stopped, and what it
// deferred; codes and counts only.
func TestWatcherLogsEachProjectsBudgetHour(t *testing.T) {
	var logged []string
	old := logf
	logf = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	t.Cleanup(func() { logf = old })
	now := time.Date(2026, 10, 27, 9, 50, 0, 0, time.UTC)
	store := &kindsStore{kinds: map[string][]bus.LinearBudgetKind{
		"prj_a": {{Kind: "watch.comments", Calls: 120, Events: 30}, {Kind: "app_write.update", Calls: 64, Events: 4}, {Kind: "watch.health", Calls: 10, Events: 1}, {Kind: "watch.docs", Calls: 5, Events: 5}},
		"prj_b": {{Kind: "watch.comments", Calls: 40, Events: 10}, {Kind: "watch.markers", Calls: 0, Events: 1}},
		"prj_d": {},
	}}
	w := &Watcher{Store: store, Now: func() time.Time { return now }}
	pass := func(pid string, c *linearboard.WatchCycle) {
		w.noteBudget(pid, ProjectTick{ProjectID: pid, Comments: c})
	}
	pass("prj_b", &linearboard.WatchCycle{BudgetUsed: 40, BudgetLimit: 200})
	pass("prj_a", &linearboard.WatchCycle{BudgetUsed: 190, BudgetLimit: 200, Deferred: 3})
	pass("prj_a", &linearboard.WatchCycle{BudgetUsed: 199, BudgetLimit: 200, BudgetExhausted: true, Deferred: 5})
	// A markers-only nudge: no budget read; its budget stop defers its rows.
	pass("prj_a", &linearboard.WatchCycle{MarkersStopped: "budget", MarkersLeft: 2})
	pass("prj_a", &linearboard.WatchCycle{MarkersStopped: "limit", MarkersLeft: 4})
	pass("prj_c", nil)
	// No cap, the default: the most read holds, and the line says none.
	pass("prj_d", &linearboard.WatchCycle{BudgetUsed: 12})
	pass("prj_d", &linearboard.WatchCycle{BudgetUsed: 30})
	pass("prj_d", &linearboard.WatchCycle{MarkersStopped: "limit"})
	if len(logged) != 0 {
		t.Fatalf("logged within the hour: %v", logged)
	}
	now = now.Add(15 * time.Minute)
	pass("prj_b", &linearboard.WatchCycle{BudgetUsed: 2, BudgetLimit: 200})
	want := []string{
		"watcher: budget: project prj_a hour 09: used 199 of 200, refused 2 passes, deferred 5, top: watch.comments 120, app_write.update 64, watch.health 10",
		"watcher: budget: project prj_b hour 09: used 40 of 200, refused 0 passes, deferred 0, top: watch.comments 40",
		"watcher: budget: project prj_d hour 09: used 30 of none, refused 0 passes, deferred 0",
	}
	if !reflect.DeepEqual(logged, want) {
		t.Fatalf("%q", logged)
	}
	if !reflect.DeepEqual(store.asked, []string{"prj_a 2026-10-27T09", "prj_b 2026-10-27T09", "prj_d 2026-10-27T09"}) {
		t.Fatal(store.asked)
	}
	// The store can't say for prj_b now: the line has no top.
	delete(store.kinds, "prj_b")
	now = now.Add(time.Hour)
	pass("prj_z", nil)
	if logged[len(logged)-1] != "watcher: budget: project prj_b hour 10: used 2 of 200, refused 0 passes, deferred 0" || len(logged) != 4 {
		t.Fatalf("%q", logged)
	}
	for _, line := range logged {
		if strings.Contains(line, "FIX-") || strings.Contains(line, "issue") {
			t.Fatal("issue data in", line)
		}
	}
}

// A real tick records its project's hour, and the next hour's nudge logs
// it, with the server's budget as the limit.
func TestTickAndNudgeLogTheBudgetHour(t *testing.T) {
	e := setup(t)
	e.w.Budget = 300
	var logged []string
	old := logf
	logf = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	t.Cleanup(func() { logf = old })
	start := time.Now().UTC()
	e.w.Now = func() time.Time { return start }
	e.w.Tick(context.Background())
	// An agent session's personal write in the same hour: its kind is
	// the hour's top one.
	e.call("linear_write_charge", "eng", map[string]any{"operation_id": "w-1", "kind": "update", "calls": 4})
	e.w.Now = func() time.Time { return start.Add(time.Hour) }
	e.w.Markers(context.Background(), e.pid)
	want := fmt.Sprintf("watcher: budget: project %s hour %s: used ", e.pid, start.Format("15"))
	found := false
	for _, line := range logged {
		// The bus's own kinds for the hour.
		found = found || strings.HasPrefix(line, want) && strings.HasSuffix(line, " of 300, refused 0 passes, deferred 0, top: profile.update 4")
	}
	if !found {
		t.Fatalf("%q", logged)
	}
}

// A webhook pass the budget stops counts as refused, as a tick's does.
func TestWebhookPassNotesTheBudgetHour(t *testing.T) {
	e := setup(t)
	var logged []string
	old := logf
	logf = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	t.Cleanup(func() { logf = old })
	start := time.Now().UTC()
	e.w.Now = func() time.Time { return start }
	e.w.Budget = 1
	e.w.webhook(context.Background(), e.pid)
	e.w.Now = func() time.Time { return start.Add(time.Hour) }
	e.w.noteBudget("prj_next", ProjectTick{})
	want := fmt.Sprintf("watcher: budget: project %s hour %s: used ", e.pid, start.Format("15"))
	for _, line := range logged {
		if strings.HasPrefix(line, want) && strings.Contains(line, " of 1, refused 1 passes") {
			return
		}
	}
	t.Fatalf("%q", logged)
}
