package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
)

// fakeDrainStore is the maintenance row and the instance reports.
type fakeDrainStore struct {
	mu      sync.Mutex
	state   bus.Maintenance
	fail    bool
	reports []time.Time
	reads   int
}

func (f *fakeDrainStore) MaintenanceState(context.Context) (bus.Maintenance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.fail {
		return bus.Maintenance{}, errors.New("down")
	}
	return f.state, nil
}

func (f *fakeDrainStore) ReportServerInstance(_ context.Context, _ string, drainedFor time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, drainedFor)
	return nil
}

func (f *fakeDrainStore) set(m bus.Maintenance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = m
}

func (f *fakeDrainStore) lastReport() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reports[len(f.reports)-1]
}

// drainClock is a settable process clock.
type drainClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *drainClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *drainClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var drainStart = time.Date(2026, 10, 28, 22, 0, 0, 0, time.UTC)

// on is a maintenance started at drainStart with left to run, read at now.
func on(now time.Time, left time.Duration) bus.Maintenance {
	return bus.Maintenance{Active: true, StartedAt: drainStart, EndsAt: now.Add(left), Reason: "deploy", StartedBy: "operator", Now: now}
}

func newTestDrain(state bus.Maintenance) (*Drain, *fakeDrainStore, *drainClock) {
	clock := &drainClock{now: drainStart}
	store := &fakeDrainStore{state: state}
	return NewDrain(store, "clawbridge-server-test", clock.Now), store, clock
}

// recordingStore answers every request and records its operations.
type recordingStore struct {
	mu  sync.Mutex
	ops []string
}

func (s *recordingStore) ExecuteAs(_ context.Context, _ string, r bus.Request) (bus.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, r.Operation)
	return bus.Result{Data: map[string]any{}}, nil
}

func (s *recordingStore) ran() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

var drainVerifier = VerifierFunc(func(context.Context, string) (Identity, error) {
	return Identity{Principal: "ada@example.com", Subject: "s"}, nil
})

func drainPost(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// In maintenance every request but a pure read answers 503 maintenance
// with Retry-After, and reaches no store; a pure read is served. Join and
// intake are refused too; healthz and config stay up.
func TestDrainRefusesAllButPureReads(t *testing.T) {
	d, _, _ := newTestDrain(on(drainStart, 10*time.Minute))
	store := &recordingStore{}
	host := httptest.NewServer(NewHandlerWithOptions(store, drainVerifier, HandlerOptions{Drain: d}))
	defer host.Close()
	cases := []struct {
		body   string
		served bool
	}{
		{`{"operation":"snapshot","workspace":"w","project":"p","args":{}}`, true},
		{`{"operation":"list_sessions","workspace":"w","project":"p","session":"s","args":{"instance":"i"}}`, true},
		{`{"operation":"heartbeat","workspace":"w","project":"p","session":"s","args":{"instance":"i"}}`, true},
		{`{"operation":"send","workspace":"w","project":"p","session":"s","args":{"instance":"i"}}`, false},
		{`{"operation":"lease","workspace":"w","project":"p","session":"s","args":{"instance":"i"}}`, false},
		{`{"operation":"ack","workspace":"w","project":"p","session":"s","args":{"instance":"i"}}`, false},
		{`{"operation":"stream_ticket","workspace":"w","project":"p","session":"s","args":{"instance":"i"}}`, false},
		{`{"operation":"register_session","workspace":"w","project":"p","session":"s","args":{}}`, false},
		// A session without its instance isn't a pure read.
		{`{"operation":"list_sessions","workspace":"w","project":"p","session":"s","args":{}}`, false},
	}
	for _, c := range cases {
		res := drainPost(t, host.URL+"/v1/execute", c.body)
		var got errorBody
		_ = json.NewDecoder(res.Body).Decode(&got)
		res.Body.Close()
		if c.served != (res.StatusCode == 200) {
			t.Fatalf("%s: status %d", c.body, res.StatusCode)
		}
		// A served read says when the drain ends, for doctor.
		if until := res.Header.Get(MaintenanceUntilHeader); c.served != (until == "2026-10-28T22:10:00Z") {
			t.Fatalf("%s: until %q", c.body, until)
		}
		if !c.served && (res.StatusCode != 503 || got.Error.Code != "maintenance" || res.Header.Get("Retry-After") != "30" || got.Error.Message != "the server is in maintenance; retry after 30 s") {
			t.Fatalf("%s: %d %+v retry %q", c.body, res.StatusCode, got, res.Header.Get("Retry-After"))
		}
	}
	if ops := store.ran(); strings.Join(ops, ",") != "snapshot,list_sessions,heartbeat" {
		t.Fatal("refused requests reached the store:", ops)
	}
	join := drainPost(t, host.URL+"/v1/join", `{}`)
	join.Body.Close()
	if join.StatusCode != 404 {
		// No device sign-in here: join isn't served at all.
		t.Fatal("join without device sign-in:", join.StatusCode)
	}
	for _, path := range []string{"/healthz", "/v1/config"} {
		res, err := http.Get(host.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(path, res.StatusCode)
		}
	}
}

// Without maintenance nothing is refused, and with no drain at all the
// handler is today's.
func TestDrainOffServesWrites(t *testing.T) {
	d, _, _ := newTestDrain(bus.Maintenance{Now: drainStart})
	for _, drain := range []*Drain{d, nil} {
		store := &recordingStore{}
		host := httptest.NewServer(NewHandlerWithOptions(store, drainVerifier, HandlerOptions{Drain: drain}))
		res := drainPost(t, host.URL+"/v1/execute", `{"operation":"send","workspace":"w","project":"p","session":"s","args":{"instance":"i"}}`)
		res.Body.Close()
		host.Close()
		if res.StatusCode != 200 || len(store.ran()) != 1 || res.Header.Get(MaintenanceUntilHeader) != "" {
			t.Fatal(res.StatusCode, store.ran(), res.Header)
		}
	}
}

// Retry-After is min(30, what is left), at least 1, and counts down from
// the read by the process clock.
func TestDrainRetryAfter(t *testing.T) {
	for _, c := range []struct {
		left time.Duration
		want int
	}{{10 * time.Minute, 30}, {30 * time.Second, 30}, {12*time.Second + time.Millisecond, 13}, {500 * time.Millisecond, 1}, {0, 1}} {
		d, _, _ := newTestDrain(on(drainStart, c.left))
		if got, ok := d.Check(context.Background()); !ok || got != c.want {
			t.Fatal(c.left, got, ok)
		}
	}
	d, _, clock := newTestDrain(on(drainStart, 20*time.Second))
	d.Check(context.Background())
	clock.add(500 * time.Millisecond)
	if got, _ := d.Check(context.Background()); got != 20 {
		t.Fatal("within the cache:", got)
	}
	clock.add(5 * time.Second)
	// The store still says 20 s left as of its own read, so a fresh read
	// answers 20; within the cache the process clock counts down.
	if got, _ := d.Check(context.Background()); got != 20 {
		t.Fatal(got)
	}
}

// A request reads the row at most once a second; a notify reads it at
// once. An unreadable row keeps the last view, so it never shuts the
// server, and never opens a drained one either.
func TestDrainCacheAndNotify(t *testing.T) {
	d, store, clock := newTestDrain(bus.Maintenance{Now: drainStart})
	ctx := context.Background()
	d.Check(ctx)
	store.set(on(drainStart, time.Minute))
	if _, ok := d.Check(ctx); ok {
		t.Fatal("read within the cache")
	}
	d.Invalidate()
	if _, ok := d.Check(ctx); !ok {
		t.Fatal("a notify didn't make the next request read")
	}
	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()
	clock.add(2 * time.Second)
	if _, ok := d.Check(ctx); !ok {
		t.Fatal("an unreadable row ended maintenance")
	}
	reads := store.reads
	for range 5 {
		d.Check(ctx)
	}
	if store.reads != reads {
		t.Fatal("read more than once a second:", store.reads-reads)
	}
}

// D5: a request counted before maintenance started holds the drain until
// it ends; only then is the process drained, for this maintenance.
func TestDrainInFlightRace(t *testing.T) {
	d, store, clock := newTestDrain(bus.Maintenance{Now: drainStart})
	ctx := context.Background()
	done := d.Begin()
	if _, ok := d.Check(ctx); ok {
		t.Fatal("maintenance before it started")
	}
	store.set(on(drainStart, time.Minute))
	d.Invalidate()
	d.refresh(ctx)
	clock.add(3 * time.Second)
	d.report(ctx)
	if !store.lastReport().IsZero() {
		t.Fatal("drained with a request in flight")
	}
	select {
	case <-d.kick:
	default:
	}
	done()
	done() // a second done counts nothing
	select {
	case <-d.kick:
	default:
		t.Fatal("the last request's end didn't wake the report")
	}
	d.report(ctx)
	if !store.lastReport().Equal(drainStart) {
		t.Fatal("not drained for this maintenance:", store.lastReport())
	}
	if d.inflight.Load() != 0 {
		t.Fatal(d.inflight.Load())
	}
	// Without maintenance a process with nothing in flight isn't drained.
	store.set(bus.Maintenance{Now: drainStart})
	d.refresh(ctx)
	d.report(ctx)
	if !store.lastReport().IsZero() {
		t.Fatal("drained outside maintenance")
	}
}

// A request is counted from its start, before authentication: one that
// waits in authentication while maintenance starts still holds the drain.
func TestDrainCountsFromRequestStart(t *testing.T) {
	d, store, _ := newTestDrain(bus.Maintenance{Now: drainStart})
	entered, release := make(chan struct{}), make(chan struct{})
	verifier := VerifierFunc(func(context.Context, string) (Identity, error) {
		close(entered)
		<-release
		return Identity{Principal: "ada@example.com", Subject: "s"}, nil
	})
	host := httptest.NewServer(NewHandlerWithOptions(&recordingStore{}, verifier, HandlerOptions{Drain: d}))
	defer host.Close()
	got := make(chan int, 1)
	go func() {
		res := drainPost(t, host.URL+"/v1/execute", `{"operation":"send","workspace":"w","project":"p","session":"s","args":{"instance":"i"}}`)
		res.Body.Close()
		got <- res.StatusCode
	}()
	<-entered
	if d.inflight.Load() != 1 {
		t.Fatal("not counted before authentication")
	}
	store.set(on(drainStart, time.Minute))
	d.Invalidate()
	d.refresh(context.Background())
	d.report(context.Background())
	if !store.lastReport().IsZero() {
		t.Fatal("drained while a request authenticates")
	}
	close(release)
	// It passed authentication after the start, so it is refused.
	if code := <-got; code != 503 {
		t.Fatal(code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for d.inflight.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	d.report(context.Background())
	if !store.lastReport().Equal(drainStart) {
		t.Fatal("never drained")
	}
}

// countingTicker counts its ticks.
type countingTicker struct {
	mu sync.Mutex
	n  int
}

func (c *countingTicker) Tick(context.Context) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return map[string]any{}
}

func (c *countingTicker) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

// The workers start nothing while the server drains: the watcher's flight
// (clock, tick endpoint and nudges) answers paused, and the sweeper skips.
// A running tick counts in flight until it ends.
func TestDrainPausesWorkers(t *testing.T) {
	d, _, _ := newTestDrain(bus.Maintenance{Now: drainStart})
	ctx := context.Background()
	d.Check(ctx)
	ticker := &countingTicker{}
	flight := oneAtATime(ticker)
	flight.drain = d
	if !flight.claim() || d.inflight.Load() != 1 {
		t.Fatal("a claimed tick isn't in flight")
	}
	flight.release()
	if d.inflight.Load() != 0 || flight.running.Load() {
		t.Fatal("release left it in flight")
	}
	flight.Tick(ctx)
	if ticker.count() != 1 {
		t.Fatal("no tick outside maintenance")
	}
	d.state, d.readAt = on(drainStart, time.Minute), drainStart
	d.active.Store(true)
	if got := flight.Tick(ctx); got.(map[string]any)["paused"] != true || ticker.count() != 1 {
		t.Fatal("a tick ran in maintenance:", got)
	}
	if flight.claim() || d.inflight.Load() != 0 {
		t.Fatal("a nudge claimed the flight in maintenance")
	}
	if heldFlight(tickPaused) {
		t.Fatal("the clock would retry a paused tick")
	}
	sweeps := &countingSweeper{}
	ctxSweep, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	runSweeper(ctxSweep, sweeps, 5*time.Millisecond, d, nil)
	if sweeps.count() != 0 {
		t.Fatal("swept in maintenance")
	}
	ctxSweep, cancel = context.WithCancel(ctx)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	runSweeper(ctxSweep, sweeps, 5*time.Millisecond, nil, nil)
	if sweeps.count() == 0 {
		t.Fatal("no drain must sweep")
	}
}

type countingSweeper struct {
	mu sync.Mutex
	n  int
}

func (c *countingSweeper) SweepDeliveries(context.Context) (bus.SweepResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return bus.SweepResult{}, nil
}

func (c *countingSweeper) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

// The tick endpoint answers paused while the server drains.
func TestDrainTickEndpointPaused(t *testing.T) {
	d, _, _ := newTestDrain(on(drainStart, time.Minute))
	d.Check(context.Background())
	ticker := &countingTicker{}
	flight := oneAtATime(ticker)
	flight.drain = d
	h := newTickHandler(tickVerifierFunc(func(context.Context, string) error { return nil }), flight, newPreauthLimiter(60, time.Now))
	req := httptest.NewRequest(http.MethodPost, TickPath, nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"paused":true`) || ticker.count() != 0 {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

type tickVerifierFunc func(context.Context, string) error

func (f tickVerifierFunc) VerifyTick(ctx context.Context, token string) error { return f(ctx, token) }

// E2: after a maintenance ends, however long it ran, the workers and the
// model API wait the resume grace, so every live session beats before
// anything judges its presence; then they start again.
func TestDrainResumeGraceAfterLongMaintenance(t *testing.T) {
	d, store, clock := newTestDrain(on(drainStart, 25*time.Minute))
	ctx := context.Background()
	ended := 0
	d.onEnd = func() { ended++ }
	d.refresh(ctx)
	clock.add(20 * time.Minute)
	if _, paused := d.Paused(); !paused {
		t.Fatal("not paused in maintenance")
	}
	store.set(bus.Maintenance{Now: drainStart.Add(20 * time.Minute)})
	d.refresh(ctx)
	if ended != 1 || d.Quiet() {
		t.Fatal("the end wasn't applied:", ended)
	}
	ticker := &countingTicker{}
	flight := oneAtATime(ticker)
	flight.drain = d
	if retry, paused := d.Paused(); !paused || retry != 30 {
		t.Fatal("no grace after the end:", retry, paused)
	}
	if got := flight.Tick(ctx); got.(map[string]any)["paused"] != true {
		t.Fatal("a tick ran in the grace")
	}
	clock.add(resumeGrace - time.Second)
	if retry, paused := d.Paused(); !paused || retry != 1 {
		t.Fatal("the grace ended early:", retry)
	}
	clock.add(time.Second)
	if _, paused := d.Paused(); paused {
		t.Fatal("the grace never ended")
	}
	flight.Tick(ctx)
	if ticker.count() != 1 {
		t.Fatal("no tick after the grace")
	}
}

// The TTL ends a maintenance like an end: the row reads inactive.
func TestDrainTTLEnds(t *testing.T) {
	d, store, clock := newTestDrain(on(drainStart, time.Minute))
	ctx := context.Background()
	started := 0
	d.onStart = func(retry int) {
		started++
		if retry != 30 {
			t.Fatal(retry)
		}
	}
	d.refresh(ctx)
	d.refresh(ctx)
	if started != 1 {
		t.Fatal("start applied", started, "times")
	}
	clock.add(time.Minute)
	m := on(drainStart, 0)
	m.Active = false
	store.set(m)
	d.refresh(ctx)
	if _, ok := d.Check(ctx); ok || d.Quiet() {
		t.Fatal("the TTL didn't end maintenance")
	}
}

// fakeClearStore is a start's clear.
type fakeClearStore struct {
	cleared bool
	err     error
}

func (f fakeClearStore) ClearMaintenanceAtStart(context.Context) (bool, error) {
	return f.cleared, f.err
}

// A start that ended a maintenance waits the grace; a plain restart (a
// crash, no row) or one whose clear failed never waits.
func TestDrainStartGraceOnlyWhenCleared(t *testing.T) {
	for _, c := range []struct {
		store fakeClearStore
		pause bool
	}{{fakeClearStore{cleared: true}, true}, {fakeClearStore{}, false}, {fakeClearStore{err: errors.New("down")}, false}} {
		d, _, _ := newTestDrain(bus.Maintenance{Now: drainStart})
		clearAtStart(context.Background(), c.store, d)
		if _, paused := d.Paused(); paused != c.pause {
			t.Fatal(c.store, paused)
		}
	}
}

// A drain closes every open stream with maintenance, then bye, and wakes a
// waiting cases/next poll, which answers no case without asking the store
// for one. Neither is in flight, so the drain completes with both open.
func TestDrainClosesStreamsAndPolls(t *testing.T) {
	hub := NewStreamHub()
	sig := hub.drainSignal()
	d, _, _ := newTestDrain(bus.Maintenance{Now: drainStart})
	d.onStart = hub.StartDrain
	store := &nextCases{}
	polls := &casePolls{held: map[string]bool{}, limit: 4}
	got := make(chan any, 1)
	go func() {
		out, _ := serveCasesNext(context.Background(), store, hub, "tok", map[string]json.RawMessage{"wait_seconds": json.RawMessage(`20`)}, 3, polls, nil, d)
		got <- out
	}()
	deadline := time.Now().Add(2 * time.Second)
	for store.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	before := store.count()
	d.state, d.readAt = on(drainStart, time.Minute), drainStart
	d.active.Store(true)
	hub.StartDrain(17)
	select {
	case out := <-got:
		if out.(map[string]any)["case"] != nil || store.count() != before {
			t.Fatal(out, store.count(), before)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll kept waiting")
	}
	select {
	case <-sig:
	default:
		t.Fatal("open streams weren't told")
	}
	if hub.retryAfter() != 17 {
		t.Fatal(hub.retryAfter())
	}
	hub.StartDrain(3)
	if hub.retryAfter() != 17 {
		t.Fatal("a second start changed retry_after")
	}
	hub.EndDrain()
	select {
	case <-hub.drainSignal():
		t.Fatal("a new stream after the end would close at once")
	default:
	}
	if d.inflight.Load() != 0 {
		t.Fatal(d.inflight.Load())
	}
}

// The model API is busy, in its own shape, while paused.
func TestDrainModelAPIBusy(t *testing.T) {
	d, _, _ := newTestDrain(on(drainStart, 12*time.Second))
	rec := httptest.NewRecorder()
	failModelBusy(rec, 12)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "12" || strings.TrimSpace(rec.Body.String()) != `{"error":"server_busy","retry_after":12}` {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if retry, ok := d.Check(context.Background()); !ok || retry != 12 {
		t.Fatal(retry, ok)
	}
	rec = httptest.NewRecorder()
	failIntakeMaintenance(rec, 9)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "9" || !strings.Contains(rec.Body.String(), `"jsonrpc":"2.0"`) || !strings.Contains(rec.Body.String(), "retry after 9 s") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

// The model API answers server_busy, reaching no store, in maintenance and
// in the grace after it.
func TestDrainModelHandlerBusy(t *testing.T) {
	d, store, clock := newTestDrain(on(drainStart, 12*time.Second))
	// A nil ModelStore inside: any store call panics the test.
	h := newModelHandler(&nextCases{}, nil, 2, clock.Now, nil, d)
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, ModelHelloPath, nil)
		req.RemoteAddr = "127.0.0.1:5000"
		req.Header.Set("ClawBridge-Model-API", modelAPIVersion)
		req.Header.Set("Authorization", "Bearer cbm_"+strings.Repeat("a", 40))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(); rec.Code != 503 || !strings.Contains(rec.Body.String(), `"server_busy"`) || rec.Header().Get("Retry-After") != "12" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	store.set(bus.Maintenance{Now: drainStart})
	d.Invalidate()
	if rec := get(); rec.Code != 503 || rec.Header().Get("Retry-After") != "30" {
		t.Fatal("the grace let the model API in:", rec.Code, rec.Header().Get("Retry-After"))
	}
	clock.add(resumeGrace)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("after the grace the request should reach the store")
			}
		}()
		get()
	}()
}

// intakeRecordingStore is a store with intake, whose intake must not be
// reached; its device store must not be either.
type intakeRecordingStore struct {
	recordingStore
	IntakeStore
	DeviceStore
}

// /v1/join and /mcp/intake are refused before their handlers run: join
// with 503 maintenance, intake with a JSON-RPC error at 503.
func TestDrainRefusesJoinAndIntake(t *testing.T) {
	d, _, _ := newTestDrain(on(drainStart, 10*time.Second))
	store := &intakeRecordingStore{}
	host := httptest.NewServer(NewHandlerWithOptions(store, drainVerifier, HandlerOptions{Drain: d, Auth: AuthOptions{Methods: []string{AuthDevice}, PublicOrigin: "https://cb.example.com", Devices: store}}))
	defer host.Close()
	join := drainPost(t, host.URL+"/v1/join", `{}`)
	var got errorBody
	_ = json.NewDecoder(join.Body).Decode(&got)
	join.Body.Close()
	if join.StatusCode != 503 || got.Error.Code != "maintenance" || join.Header.Get("Retry-After") != "10" {
		t.Fatal("join:", join.StatusCode, got)
	}
	intake := drainPost(t, host.URL+IntakePath, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var rpc map[string]any
	_ = json.NewDecoder(intake.Body).Decode(&rpc)
	intake.Body.Close()
	if intake.StatusCode != 503 || rpc["jsonrpc"] != "2.0" || rpc["error"] == nil || intake.Header.Get("Retry-After") != "10" {
		t.Fatal("intake:", intake.StatusCode, rpc)
	}
}

// serve's shutdown outlasts an app write to Linear, and compose's
// stop_grace_period for the API outlasts serve's shutdown (§2.3).
func TestShutdownGraceOutlastsAppWrites(t *testing.T) {
	if shutdownGrace <= appWriteTimeout {
		t.Fatal("serve's shutdown can cut an app write:", shutdownGrace, appWriteTimeout)
	}
	compose, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	api := string(compose)[strings.Index(string(compose), "\n  api:\n"):]
	api = api[:strings.Index(api[1:], "\n  caddy:\n")+1]
	m := regexp.MustCompile(`\n    stop_grace_period: (\S+)\n`).FindStringSubmatch(api)
	if m == nil {
		t.Fatal("the api service has no stop_grace_period")
	}
	grace, err := time.ParseDuration(m[1])
	if err != nil || grace <= shutdownGrace {
		t.Fatal("compose stops the API before serve's shutdown ends:", m[1], err)
	}
}

// The client reads the header the server sets.
func TestMaintenanceUntilHeaderMatchesTheClients(t *testing.T) {
	if MaintenanceUntilHeader != remote.MaintenanceUntilHeader {
		t.Fatal(MaintenanceUntilHeader, remote.MaintenanceUntilHeader)
	}
}
