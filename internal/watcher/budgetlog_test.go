package watcher

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/linearboard"
)

// Each project's hour is logged once, when a later pass sees the hour has
// ended: the last budget read, the passes the budget stopped, and what it
// deferred; codes and counts only.
func TestWatcherLogsEachProjectsBudgetHour(t *testing.T) {
	var logged []string
	old := logf
	logf = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	t.Cleanup(func() { logf = old })
	now := time.Date(2026, 10, 27, 9, 50, 0, 0, time.UTC)
	w := &Watcher{Now: func() time.Time { return now }}
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
		"watcher: budget: project prj_a hour 09: used 199 of 200, refused 2 passes, deferred 5",
		"watcher: budget: project prj_b hour 09: used 40 of 200, refused 0 passes, deferred 0",
		"watcher: budget: project prj_d hour 09: used 30 of none, refused 0 passes, deferred 0",
	}
	if !reflect.DeepEqual(logged, want) {
		t.Fatalf("%q", logged)
	}
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
	e.w.Now = func() time.Time { return start.Add(time.Hour) }
	e.w.Markers(context.Background(), e.pid)
	want := fmt.Sprintf("watcher: budget: project %s hour %s: used ", e.pid, start.Format("15"))
	found := false
	for _, line := range logged {
		found = found || strings.HasPrefix(line, want) && strings.Contains(line, " of 300, refused 0 passes, deferred 0")
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
