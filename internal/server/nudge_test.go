package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
)

// passes records each marker pass, and can hold one until released.
type passes struct {
	mu      sync.Mutex
	pids    []string
	started chan string
	hold    chan struct{}
}

func (p *passes) Markers(_ context.Context, pid string) any {
	p.mu.Lock()
	p.pids = append(p.pids, pid)
	p.mu.Unlock()
	if p.started != nil {
		p.started <- pid
	}
	if p.hold != nil {
		<-p.hold
	}
	return nil
}

func (p *passes) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pids)
}

// testNudger resolves project "pN" to "id-pN" and "none" to nothing, on a
// clock the test moves.
func testNudger(markers markerPasser, flight *singleFlight) (*nudger, *time.Time) {
	clock := time.Unix(1000, 0)
	n := newNudger(flight, markers, func(_ context.Context, _, project string) (string, error) {
		if project == "none" {
			return "", nil
		}
		return "id-" + project, nil
	})
	n.now = func() time.Time { return clock }
	return n, &clock
}

// A confirm's nudge runs one marker pass; more signals within the minute
// collapse into one pass when the minute is up; another project is its
// own; an unbound project runs nothing.
func TestNudgeRunsOnePassPerProjectAMinute(t *testing.T) {
	p := &passes{}
	n, clock := testNudger(p, oneAtATime(&countTicks{ticks: make(chan struct{}, 8)}))
	n.Nudge("w", "p1")
	n.Nudge("w", "p1")
	n.serve(context.Background())
	if p.count() != 1 || p.pids[0] != "id-p1" {
		t.Fatal("two signals made", p.pids)
	}
	n.Nudge("w", "p1")
	n.Nudge("w", "p1")
	n.Nudge("w", "p2")
	wait := n.serve(context.Background())
	if p.count() != 2 || p.pids[1] != "id-p2" || wait != nudgeEvery {
		t.Fatal("within the minute", p.pids, wait)
	}
	*clock = clock.Add(30 * time.Second)
	if wait = n.serve(context.Background()); p.count() != 2 || wait != 30*time.Second {
		t.Fatal("ran before its minute", p.pids, wait)
	}
	*clock = clock.Add(30 * time.Second)
	n.serve(context.Background())
	if p.count() != 3 || p.pids[2] != "id-p1" {
		t.Fatal("the collapsed signals did not make one pass", p.pids)
	}
	if n.serve(context.Background()); p.count() != 3 {
		t.Fatal("a pass ran with nothing pending", p.pids)
	}
	n.Nudge("w", "none")
	if n.serve(context.Background()); p.count() != 3 {
		t.Fatal("an unbound project was passed", p.pids)
	}
}

// A nudge never runs beside a tick: it waits for the flight, then runs.
func TestNudgeWaitsForARunningTick(t *testing.T) {
	ticker := &blockTicks{started: make(chan struct{}, 1), release: make(chan struct{})}
	flight := oneAtATime(ticker)
	p := &passes{}
	n, _ := testNudger(p, flight)
	go flight.Tick(context.Background())
	<-ticker.started
	n.Nudge("w", "p1")
	if wait := n.serve(context.Background()); p.count() != 0 || wait != nudgeRetry {
		t.Fatal("a nudge ran beside a tick", p.pids, wait)
	}
	close(ticker.release)
	for flight.running.Load() {
		time.Sleep(time.Millisecond)
	}
	n.serve(context.Background())
	if p.count() != 1 {
		t.Fatal("the nudge did not run after the tick", p.pids)
	}
	// And a tick asked for during a nudge's pass is not started.
	p.started, p.hold = make(chan string, 1), make(chan struct{})
	n.last = map[string]time.Time{}
	n.Nudge("w", "p1")
	done := make(chan struct{})
	go func() { n.serve(context.Background()); close(done) }()
	<-p.started
	if got := flight.Tick(context.Background()); got == nil {
		t.Fatal("a tick started during a nudge's pass")
	}
	close(p.hold)
	<-done
}

// The loop serves a nudge within moments, and Nudge never blocks.
func TestNudgeLoopServesWithoutBlocking(t *testing.T) {
	p := &passes{started: make(chan string, 4)}
	n, _ := testNudger(p, oneAtATime(&countTicks{ticks: make(chan struct{}, 8)}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.run(ctx)
	for i := 0; i < 1000; i++ {
		n.Nudge("w", "p1")
	}
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not serve the nudge")
	}
}

// okStore answers every request, or fails it.
type okStore struct{ fail bool }

func (s okStore) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	if s.fail {
		return bus.Result{}, errors.New("refused")
	}
	return bus.Result{Data: map[string]any{}}, nil
}

// The handler nudges only after a confirm or release succeeded, and the
// request doesn't wait on it.
func TestHandlerNudgesAfterAConfirmOrRelease(t *testing.T) {
	verifier := VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{Principal: "ada@example.com", Subject: "s"}, nil
	})
	for _, c := range []struct {
		op     string
		fail   bool
		nudged bool
	}{
		{"assignment_confirm", false, true},
		{"assignment_release", false, true},
		{"assignment_propose", false, false},
		{"assignment_confirm", true, false},
	} {
		var got []string
		host := httptest.NewServer(NewHandlerWithOptions(okStore{c.fail}, verifier, HandlerOptions{Nudge: func(w, p string) { got = append(got, w+"/"+p) }}))
		req, _ := http.NewRequest(http.MethodPost, host.URL+"/v1/execute", bytes.NewBufferString(`{"operation":"`+c.op+`","workspace":"w","project":"p","session":"s","args":{}}`))
		req.Header.Set("Authorization", "Bearer token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		host.Close()
		if (len(got) == 1 && got[0] == "w/p") != c.nudged || len(got) > 1 {
			t.Fatal(c, got)
		}
	}
}

// No nudge without the in-process clock: the scheduler's endpoint alone,
// or no watcher, leaves Nudge unset.
func TestNoNudgeWithoutTheWatcherClock(t *testing.T) {
	if opts := startWatcher(context.Background(), nil, watchSettings{}, HandlerOptions{}); opts.Nudge != nil {
		t.Fatal("a nudge without a watcher")
	}
	if opts := startWatcher(context.Background(), nil, watchSettings{interval: time.Minute}, HandlerOptions{}); opts.Nudge != nil {
		t.Fatal("a nudge without the app")
	}
}

// markerTicks is a watcher that also serves nudges' marker passes.
type markerTicks struct {
	countTicks
	passes
}

// With the in-process clock and the app, startWatcher wires Nudge; with
// the scheduler's endpoint alone it doesn't.
func TestNudgeRidesBesideTheWatcherClock(t *testing.T) {
	saved := NewWatcher
	defer func() { NewWatcher = saved }()
	NewWatcher = func(context.Context, *bus.Store, *linearapp.App) (Ticker, error) {
		return &markerTicks{countTicks: countTicks{ticks: make(chan struct{}, 8)}}, nil
	}
	store, err := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &AppWriter{App: &linearapp.App{}}
	if opts := startWatcher(ctx, store, watchSettings{interval: time.Hour}, HandlerOptions{Writer: writer}); opts.Nudge == nil {
		t.Fatal("no nudge beside the clock")
	}
	if opts := startWatcher(ctx, store, watchSettings{tickAudience: "https://cb.example.com/v1/tick", tickAccount: "tick@example.iam.gserviceaccount.com"}, HandlerOptions{Writer: writer}); opts.Nudge != nil {
		t.Fatal("a nudge with the scheduler's endpoint alone")
	}
}

// A clock tick that lands while a nudge holds the flight runs once the
// pass ends, not an interval later.
func TestClockTickWaitsOutANudge(t *testing.T) {
	savedRetry := watchRetry
	watchRetry = 10 * time.Millisecond
	defer func() { watchRetry = savedRetry }()
	ticks := &countTicks{ticks: make(chan struct{}, 8)}
	flight := oneAtATime(ticks)
	if !flight.claim() {
		t.Fatal("flight busy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The clock fires at 300ms, finds the nudge and retries every 10ms;
	// the next clock fire would be at 600ms.
	go runWatchLoop(ctx, flight, 300*time.Millisecond)
	if n := ticks.count(350 * time.Millisecond); n != 0 {
		t.Fatal("a tick ran beside the nudge")
	}
	flight.running.Store(false)
	if n := ticks.count(150 * time.Millisecond); n != 1 {
		t.Fatal("the held tick did not run soon after the nudge:", n)
	}
}
