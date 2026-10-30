package watcher

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/linearboard"
)

// queueMarkers leaves n marker rows due for session eng: each a claim
// released before it ran, as in the live miss this hotfix answers, so each issue has
// a claim row and a close row.
func (e *env) queueMarkers(n int) {
	e.t.Helper()
	for i := 0; i < n; i++ {
		issue := []string{issueA, issueB}[i%2]
		proposed := e.call("assignment_propose", "eng", map[string]any{"issue_uuid": issue, "issue_identifier": "FIX-1"})
		confirmed := e.call("assignment_confirm", "coord", map[string]any{"target_session": "eng", "revision": proposed["assignment"].(map[string]any)["revision"]})["assignment"].(map[string]any)
		e.call("assignment_release", "eng", map[string]any{"revision": confirmed["revision"], "outcome": "done"})
	}
	e.sql(`UPDATE assignment_markers SET next_attempt_at='2000-01-01T00:00:00.000000000Z'`)
}

func (e *env) budgetUsed() int {
	e.t.Helper()
	used, _ := e.budgetHour()
	return used
}

// budgetHour is the lease's budget and the hour it counts: the bus charges
// by its own wall clock, so a test can see the hour roll over mid-pass.
func (e *env) budgetHour() (int, string) {
	e.t.Helper()
	var used int
	var hour string
	e.queryRow(`SELECT budget_used, budget_hour FROM watch_lease`, &used, &hour)
	return used, hour
}

// markerLines drops the hourly budget line a new hour's first pass logs
// (logBudgetHour), which this test doesn't check: under CI load it can run
// across an hour however it starts.
func markerLines(logged []string) []string {
	var out []string
	for _, line := range logged {
		if !strings.HasPrefix(line, "watcher: budget: ") {
			out = append(out, line)
		}
	}
	return out
}

// The live miss's shape: twenty bound Linear
// projects, a queue of marker rows, a budget near its limit, and slow
// Linear. The pass applies what fits its time box, refunds the rest, and
// the tick completes every other step without failing.
func TestTickSurvivesTonightsMarkerQueue(t *testing.T) {
	e := setup(t)
	e.bindProjects("p", 1, 20)
	e.f.project = linearProjectID(1)
	e.tick() // takes the lease, and the day's health
	e.queueMarkers(3)
	var due int
	e.queryRow(`SELECT COUNT(*) FROM assignment_markers WHERE next_attempt_at<='2001'`, &due)
	if due < 4 {
		t.Fatal("queue:", due)
	}
	// Slow Linear: every read or write the fake answers takes 3 s. The
	// clock starts at this hour's first minute. The bus still charges by
	// its own wall clock, so the checks below allow for a new hour starting
	// mid-test (its budget line, its fresh budget).
	clock := time.Now().UTC().Truncate(time.Hour).Add(time.Minute)
	e.w.Now = func() time.Time { clock = clock.Add(time.Second); return clock }
	e.sql(`UPDATE watch_lease SET budget_used=budget_limit-60, budget_hour=?`, clock.Format("2006-01-02T15"))
	e.dueNow()
	before := e.budgetUsed()
	logged := []string{}
	old := logf
	logf = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	defer func() { logf = old }()
	tick := e.tick()
	if tick.Error != "" || tick.Comments == nil || tick.Intake == nil {
		t.Fatalf("the tick did not complete its other steps: %+v", tick)
	}
	c := tick.Comments
	if len(c.Markers) == 0 || len(c.Markers) > 3 || c.MarkersLeft == 0 || c.MarkersStopped == "" {
		t.Fatalf("the pass did not stop with rows left: %d applied, %d left, %q", len(c.Markers), c.MarkersLeft, c.MarkersStopped)
	}
	// The rows not started give their charge back: across a markers-only
	// pass, the budget moves by exactly the pass's net charge.
	e.queueMarkers(2)
	before, hourBefore := e.budgetHour()
	pass := e.w.Markers(context.Background(), e.pid)
	if pass.Error != "" || pass.Comments == nil || pass.Comments.MarkersLeft == 0 {
		t.Fatalf("the nudge pass left nothing to refund: %+v", pass)
	}
	// A new hour starts the budget again, so the difference only means
	// something within one hour.
	if after, hourAfter := e.budgetHour(); hourAfter == hourBefore && after-before != pass.Comments.MarkersCharged {
		t.Fatal("the budget kept charges for rows never applied:", after-before, "net", pass.Comments.MarkersCharged)
	}
	logged = markerLines(logged)
	if len(logged) != 2 || !strings.HasPrefix(logged[0], "watcher: markers: project "+e.pid+" (tick): applied ") || !strings.Contains(logged[0], "stopped "+c.MarkersStopped) || !strings.HasPrefix(logged[1], "watcher: markers: project "+e.pid+" (nudge): applied ") {
		t.Fatal("log:", logged)
	}
}

// A failed tick or nudge logs its code; a pass with nothing due logs no
// marker line.
func TestLogTickSaysCodesOnly(t *testing.T) {
	logged := []string{}
	old := logf
	logf = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	defer func() { logf = old }()
	logTick("prj_1", "tick", ProjectTick{Error: "deadline"})
	logTick("prj_1", "nudge", ProjectTick{Comments: &linearboard.WatchCycle{}})
	if strings.Join(logged, "|") != "watcher: tick: project prj_1 failed: deadline" {
		t.Fatal(logged)
	}
}

// A pass stopped by its limit or clock with rows left wants another pass;
// one stopped by the budget or an error, or with nothing left, doesn't.
func TestMarkersPending(t *testing.T) {
	for stopped, want := range map[string]bool{"limit": true, "time": true, "deadline": true, "budget": false, "error": false} {
		if got := (ProjectTick{Comments: &linearboard.WatchCycle{MarkersLeft: 2, MarkersStopped: stopped}}).MarkersPending(); got != want {
			t.Fatal(stopped, got)
		}
	}
	if (ProjectTick{Comments: &linearboard.WatchCycle{MarkersStopped: "time"}}).MarkersPending() || (ProjectTick{}).MarkersPending() {
		t.Fatal("pending with nothing left")
	}
}

// R116-L1: a nudge pass runs under its own deadline (ProjectDeadline, as
// a tick's project does), not the whole tick's deadline it is handed.
func TestANudgeRunsUnderTheProjectDeadline(t *testing.T) {
	e := setup(t)
	e.tick()
	e.queueMarkers(1)
	saved := nudgeDeadline
	nudgeDeadline = time.Nanosecond
	defer func() { nudgeDeadline = saved }()
	if saved != ProjectDeadline {
		t.Fatal("a nudge's deadline is not ProjectDeadline:", saved)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if pass := e.w.Markers(ctx, e.pid); pass.Error == "" {
		t.Fatalf("the nudge outlived its project deadline: %+v", pass)
	}
}

// CLAWBRIDGE_WATCH_BUDGET: a tick and a nudge each take the lease at the
// server's budget, not the bus default of 200.
func TestTickAndNudgeUseTheServerBudget(t *testing.T) {
	e := setup(t)
	e.w.Budget = 300
	limit := func() int {
		e.t.Helper()
		var n int
		e.queryRow(`SELECT budget_limit FROM watch_lease`, &n)
		return n
	}
	e.w.Tick(context.Background())
	if limit() != 300 {
		t.Fatal("tick's budget:", limit())
	}
	e.sql(`UPDATE watch_lease SET budget_limit=200, expires_at='2000-01-01T00:00:00.000000000Z'`)
	e.w.Markers(context.Background(), e.pid)
	if limit() != 300 {
		t.Fatal("nudge's budget:", limit())
	}
}
