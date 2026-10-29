package watcher

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	logTick("p1", "tick", ProjectTick{TookMS: map[string]int64{"comments": 9000, "intake": 8000, "markers": 100}})
	if len(logged) != 1 || logged[0] != "watcher: tick: project p1 took 17100ms: comments 9000ms, intake 8000ms, markers 100ms" {
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
