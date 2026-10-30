package server

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
)

// Q10: a hundred webhook nudges for one project within a minute run
// exactly one webhook pass; the minute after, one more.
func TestNudgeWebhookCoalesces(t *testing.T) {
	p := &passes{}
	n, clock := testNudger(p, oneAtATime(&countTicks{ticks: make(chan struct{}, 8)}))
	for i := 0; i < 100; i++ {
		n.NudgeWebhook("id-p1")
		if i%10 == 0 {
			n.serve(context.Background())
			*clock = clock.Add(time.Second)
		}
	}
	n.serve(context.Background())
	if !reflect.DeepEqual(p.pids, []string{"webhook:id-p1"}) {
		t.Fatal("within the minute:", p.pids)
	}
	*clock = clock.Add(nudgeEvery)
	n.serve(context.Background())
	if !reflect.DeepEqual(p.pids, []string{"webhook:id-p1", "webhook:id-p1"}) {
		t.Fatal("the minute after:", p.pids)
	}
	*clock = clock.Add(nudgeEvery)
	if n.serve(context.Background()); p.count() != 2 {
		t.Fatal("a pass with nothing pending:", p.pids)
	}
}

// A webhook pass never runs markers, and a marker or fallback nudge in
// the same minute runs in the same pass, after it; neither is lost.
func TestNudgeWebhookKeepsOtherKinds(t *testing.T) {
	p := &passes{}
	n, clock := testNudger(p, oneAtATime(&countTicks{ticks: make(chan struct{}, 8)}))
	n.NudgeWebhook("id-p1")
	n.Nudge("w", "p1")
	n.serve(context.Background())
	if !reflect.DeepEqual(p.pids, []string{"webhook:id-p1", "id-p1"}) {
		t.Fatal(p.pids)
	}
	*clock = clock.Add(nudgeEvery)
	n.NudgeProject("id-p1")
	n.NudgeWebhook("id-p1")
	n.serve(context.Background())
	if !reflect.DeepEqual(p.pids[2:], []string{"webhook:id-p1", "fallback:id-p1"}) {
		t.Fatal(p.pids)
	}
	*clock = clock.Add(nudgeEvery)
	if n.serve(context.Background()); p.count() != 4 {
		t.Fatal("a pass with nothing pending:", p.pids)
	}
	// A marker nudge during a webhook-only pass is kept for the next.
	p.started, p.hold = make(chan string, 1), make(chan struct{})
	n.NudgeWebhook("id-p1")
	done := make(chan struct{})
	go func() { n.serve(context.Background()); close(done) }()
	<-p.started
	n.mu.Lock()
	n.markersPending("id-p1")
	n.mu.Unlock()
	close(p.hold)
	<-done
	p.started, p.hold = nil, nil
	*clock = clock.Add(nudgeEvery)
	n.serve(context.Background())
	if !reflect.DeepEqual(p.pids[4:], []string{"webhook:id-p1", "id-p1"}) {
		t.Fatal("the marker nudge was lost:", p.pids)
	}
}

// catchUpStore records the drain end's calls in order.
type catchUpStore struct {
	mu       sync.Mutex
	calls    []string
	projects []string
	fail     bool
}

func (s *catchUpStore) WebhookDrainEnded(context.Context, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "drain_ended")
	return nil
}

func (s *catchUpStore) WebhookCatchUp(context.Context, time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "catch_up")
	if s.fail {
		return nil, errors.New("down")
	}
	return s.projects, nil
}

// §6 P6: a drain's end wakes the streams, records the end, marks every
// live team's cursors due, then nudges each project the catch-up moved.
func TestDrainEndCatchesUpLiveTeams(t *testing.T) {
	store := &catchUpStore{projects: []string{"pid-1", "pid-2"}}
	var mu sync.Mutex
	order := []string{}
	add := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	nudge := func(pid string) { add("nudge:" + pid) }
	end := webhookDrainEnd(func() { add("hub") }, store, func() func(string) { return nudge }, func() time.Time { return whStart })
	end()
	store.mu.Lock()
	calls := store.calls
	store.mu.Unlock()
	if !reflect.DeepEqual(order, []string{"hub", "nudge:pid-1", "nudge:pid-2"}) || !reflect.DeepEqual(calls, []string{"drain_ended", "catch_up"}) {
		t.Fatal(order, calls)
	}
	// Without a nudge (no in-process clock) the cursors are due anyway; a
	// failed catch-up still woke the streams.
	order = nil
	webhookDrainEnd(func() { add("hub") }, store, func() func(string) { return nil }, time.Now)()
	store.fail = true
	webhookDrainEnd(func() { add("hub") }, store, func() func(string) { return nudge }, time.Now)()
	if !reflect.DeepEqual(order, []string{"hub", "hub"}) {
		t.Fatal(order)
	}
}

// T2's counters reach the database from the sweeper too: a lone refusal
// no later request flushes is written within about a minute.
func TestSweeperFlushesWebhookCounts(t *testing.T) {
	store := newFakeWebhook()
	e := newWhEnv(t, store)
	e.post(whAddr, e.good(1), "wrong-secret-wrong-secret")
	e.h.FlushCounts(context.Background())
	if store.count("", "signature") != 0 {
		t.Fatal("flushed within the minute")
	}
	e.clock.add(webhookFlushEvery)
	s := &purgingSweeps{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runSweeper(ctx, s, 2*time.Millisecond, nil, e.h.FlushCounts); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for store.count("", "signature") == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	store.mu.Lock()
	calls := store.calls
	store.mu.Unlock()
	if store.count("", "signature") != 1 || calls != 1 {
		t.Fatal("the sweeper did not flush once:", store.counts, calls)
	}
}

// The route registers its flush with the options, for the sweeper.
func TestWebhookOptionsHandTheFlushToTheSweeper(t *testing.T) {
	store := newFakeWebhook()
	wo := &WebhookOptions{Secrets: map[string][]string{whTeam: {whSecret}}, OrgID: func(context.Context) (string, error) { return whOrg, nil }, flush: &webhookFlush{}}
	wo.flush.FlushCounts(context.Background()) // no handler yet: nothing
	h := newWebhookHandler(store, wo, func() time.Time { return whStart }, nil)
	h.tally("", "signature")
	h.flushedAt = whStart.Add(-webhookFlushEvery)
	wo.flush.FlushCounts(context.Background())
	if store.count("", "signature") != 1 {
		t.Fatal(store.counts)
	}
	if got, err := webhookStartup(context.Background(), &webhookStart{}, map[string][]string{whTeam: {whSecret}}, false, func(context.Context) (string, error) { return whOrg, nil }, whStart); err != nil || got.flush == nil {
		t.Fatal("startup gave no flush:", got, err)
	}
}

// Beside the in-process clock, a verified delivery's projects get a
// webhook pass; without the clock the route sends no nudge.
func TestWebhookNudgeRidesBesideTheWatcherClock(t *testing.T) {
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
	wo := &WebhookOptions{}
	if startWatcher(ctx, store, watchSettings{interval: time.Hour}, HandlerOptions{Writer: writer, Webhook: wo}); wo.Nudge == nil {
		t.Fatal("no webhook nudge beside the clock")
	}
	wo = &WebhookOptions{}
	if startWatcher(ctx, store, watchSettings{tickAudience: "https://cb.example.test/v1/tick", tickAccount: "tick@example.test"}, HandlerOptions{Writer: writer, Webhook: wo}); wo.Nudge != nil {
		t.Fatal("a webhook nudge with the scheduler's endpoint alone")
	}
	startWatcher(ctx, store, watchSettings{interval: time.Hour}, HandlerOptions{Writer: writer}) // no route: no panic
}
