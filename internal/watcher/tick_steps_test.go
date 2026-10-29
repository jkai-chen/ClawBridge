package watcher

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// stepStore fails intake's state read with an uncoded error and records the
// system operations the tick still makes after it.
type stepStore struct {
	Store
	mu  sync.Mutex
	ops []string
}

func (s *stepStore) IntakeSystemState(context.Context, string) (bus.IntakeState, error) {
	return bus.IntakeState{}, errors.New("synthetic intake state failure")
}
func (s *stepStore) ExecuteSystem(ctx context.Context, pid string, r bus.Request) (bus.Result, error) {
	s.mu.Lock()
	s.ops = append(s.ops, r.Operation)
	s.mu.Unlock()
	return s.Store.ExecuteSystem(ctx, pid, r)
}
func (s *stepStore) ExecuteDispatcher(ctx context.Context, pid, session string, r bus.Request) (bus.Result, error) {
	s.mu.Lock()
	s.ops = append(s.ops, r.Operation)
	s.mu.Unlock()
	return s.Store.ExecuteDispatcher(ctx, pid, session, r)
}

// One step that fails fails only itself: the tick records it, and the
// subscribers, health, docs and dispatcher steps after it still run.
func TestATickStepFailsOnlyItself(t *testing.T) {
	e := setup(t)
	store := &stepStore{Store: e.s}
	e.w.Store = store
	tick := e.tick()
	if tick.Error != "failed" {
		t.Fatalf("the failed step wasn't recorded: %+v", tick)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, op := range []string{"subscribe_due", "health_due", "doc_watch_due", "dispatcher_cases_due"} {
		if !slices.Contains(store.ops, op) {
			t.Fatalf("step %s didn't run after the failed one: %v", op, store.ops)
		}
	}
}

// A project whose share runs out is recorded as "deadline", however the
// step that was cut reported it, never a bare "failed".
func TestATickPastItsShareIsADeadline(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if tick := e.w.project(ctx, e.pid, HealthRunsPerTick); tick.Error != "deadline" {
		t.Fatalf("%+v", tick)
	}
	var tick ProjectTick
	tick.fail(context.Background(), errors.New("uncoded"))
	if tick.Error != "failed" {
		t.Fatal(tick.Error)
	}
	tick.fail(context.Background(), codedErr("second"))
	if tick.Error != "failed" {
		t.Fatal("the first failure didn't stand:", tick.Error)
	}
}

// codedErr is an error with a code, like the bus's and the provider's.
type codedErr string

func (e codedErr) Error() string { return string(e) }
func (e codedErr) Code() string  { return string(e) }

// A slow pass logs each step's time, in order; a quick one logs nothing.
func TestASlowPassLogsWhereItsShareWent(t *testing.T) {
	var logged []string
	old := logf
	logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { logf = old })
	logTick("p1", "tick", ProjectTick{ShareMS: 26000, TookMS: map[string]int64{"comments": 9000, "intake": 8000, "markers": 100}})
	if len(logged) != 1 || logged[0] != "watcher: tick: project p1 took 17100ms of its 26000ms share: comments 9000ms, intake 8000ms, markers 100ms" {
		t.Fatal(logged)
	}
	logged = nil
	logTick("p1", "tick", ProjectTick{TookMS: map[string]int64{"comments": 10, "intake": 20}})
	if len(logged) != 0 {
		t.Fatal(logged)
	}
}

// The watcher reuses a verified binding across ticks, not only within one.
func TestWatcherReusesTheBindingProofAcrossTicks(t *testing.T) {
	if watcherProofTTL < 15*time.Minute {
		t.Fatal("the proof expires before the next tick:", watcherProofTTL)
	}
}

// A binding's share grows with its teams and projects, from ProjectDeadline
// up to maxProjectShare.
func TestAProjectsShareGrowsWithItsBinding(t *testing.T) {
	for units, want := range map[int]time.Duration{0: ProjectDeadline, 4: ProjectDeadline, 5: ProjectDeadline + 2*time.Second, 8: ProjectDeadline + 8*time.Second, 14: maxProjectShare, 100: maxProjectShare} {
		if got := shareFor(units); got != want {
			t.Errorf("shareFor(%d) = %s, want %s", units, got, want)
		}
	}
	e := setup(t)
	if got := e.w.projectShare(context.Background(), e.pid); got != ProjectDeadline {
		t.Fatal("one team:", got)
	}
	ids := []string{}
	e.f.projects = map[string]bool{}
	for i := 1; i <= 7; i++ {
		ids = append(ids, linearProjectID(i))
		e.f.projects[linearProjectID(i)] = true
	}
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 1, "mode": "project_set", "linear_team_ids": []string{team}, "linear_default_team_id": team, "linear_project_ids": ids}}); err != nil {
		t.Fatal(err)
	}
	// One team and seven projects: four past the base.
	if got := e.w.projectShare(context.Background(), e.pid); got != ProjectDeadline+4*shareStep {
		t.Fatal("one team and seven projects:", got)
	}
	tick := e.tick()
	if tick.ShareMS != (ProjectDeadline + 4*shareStep).Milliseconds() {
		t.Fatal(tick.ShareMS)
	}
}

// A tick gives a project no more than it has left, and serves none it has
// less than minProjectShare left for: those aren't recorded as ticked, so
// they come first next time.
func TestATickServesOnlyWhatItHasTimeFor(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), minProjectShare-time.Second)
	defer cancel()
	var logged []string
	old := logf
	logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { logf = old })
	if got := e.w.Tick(ctx); len(got) != 0 {
		t.Fatal("a project was started without time for it:", got)
	}
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "watcher: tick: 1 project(s) wait for the next tick: ") {
		t.Fatal("the deferred project wasn't logged:", logged)
	}
	var ticked int
	e.queryRow(`SELECT count(*) FROM watch_ticks`, &ticked)
	if ticked != 0 {
		t.Fatal("an unserved project was recorded as ticked")
	}
	ctx, cancel = context.WithTimeout(context.Background(), minProjectShare+time.Second)
	defer cancel()
	got := e.w.Tick(ctx)
	if len(got) != 1 || got[0].ShareMS <= 0 || got[0].ShareMS > (minProjectShare+time.Second).Milliseconds() {
		t.Fatal("the share wasn't cut to what the tick had left:", got)
	}
}

// A share too short for intake defers its sources: nothing is read, every
// cursor stays, the project doesn't fail, and the markers still run.
func TestIntakeDefersPastItsRoom(t *testing.T) {
	e := setup(t)
	e.tick() // the first poll starts the cursors
	e.dueNow()
	cursors := func() map[string]string {
		out := map[string]string{}
		db, err := sql.Open("sqlite", e.path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.Query(`SELECT source||'|'||subject, cursor_time FROM watch_cursors WHERE source LIKE 'intake-%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			out[k] = v
		}
		return out
	}
	before := cursors()
	if len(before) == 0 {
		t.Fatal("no intake cursors started")
	}
	saved := intakeRoom
	intakeRoom = time.Hour
	t.Cleanup(func() { intakeRoom = saved })
	queries := len(e.f.queries)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tick := e.w.project(ctx, e.pid, HealthRunsPerTick)
	if tick.Error != "" || tick.Intake == nil || len(tick.Intake.Deferred) != len(before) || tick.Intake.Calls != 0 || len(e.f.queries) != queries {
		t.Fatalf("intake wasn't deferred whole: %+v %+v", tick.Error, tick.Intake)
	}
	if after := cursors(); !maps.Equal(after, before) {
		t.Fatal("a deferred source's cursor moved:", before, after)
	}
	if c := tick.Comments; c == nil || c.MarkersStopped == "error" {
		t.Fatalf("the markers didn't get their time: %+v", c)
	}
}

// The oldest cursor is read first: S3's, made older than S2's, before it.
func TestIntakeReadsTheOldestCursorFirst(t *testing.T) {
	e := setup(t)
	e.tick()
	e.sql(`UPDATE watch_cursors SET cursor_time='2026-01-01T00:00:00Z' WHERE source='intake-s3'`)
	e.sql(`UPDATE watch_cursors SET cursor_time='2026-01-02T00:00:00Z' WHERE source='intake-s2'`)
	e.dueNow()
	queries := len(e.f.queries)
	e.tick()
	got := e.f.queries[queries:]
	s2, s3 := slices.Index(got, delegatedQuery), slices.Index(got, mentionsQuery)
	if s2 < 0 || s3 < 0 || s3 > s2 {
		t.Fatal("the older S3 wasn't read first:", s2, s3)
	}
}

// A slow pass's intake names each signal's time.
func TestASlowPassSplitsIntakeBySignal(t *testing.T) {
	var logged []string
	old := logf
	logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { logf = old })
	logTick("p1", "tick", ProjectTick{ShareMS: 40000, TookMS: map[string]int64{"comments": 5000, "intake": 15000}, Intake: &IntakePoll{TookMS: map[string]int64{"S1": 12000, "S3": 2500}}})
	if len(logged) != 1 || logged[0] != "watcher: tick: project p1 took 20000ms of its 40000ms share: comments 5000ms, intake 15000ms (S1 12000ms, S3 2500ms)" {
		t.Fatal(logged)
	}
}
