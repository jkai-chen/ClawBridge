//go:build chaos

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
)

// The message queue's chaos suite (MESSAGE-QUEUE-DESIGN §9), against real
// Postgres: go test -tags chaos ./internal/server -run Chaos. -short skips
// the soak. The API runs in this process on a fixed local address: a
// crash closes its listener and connections at once (no bye, no graceful
// shutdown) and terminates every Postgres backend its pool holds, so an
// in-flight transaction aborts as under kill -9. What isn't covered is the
// OS tearing the process's sockets down. A consumer is a hosted client
// following the consume stream; its crash cancels it mid-handle, unacked.

// chaosAPI is an API server that can crash and come back on its address.
type chaosAPI struct {
	t      *testing.T
	f      *stFixture
	app    string
	addr   string
	limits StreamLimits
	queue  bus.QueueLimits
	srv    *http.Server
	cancel context.CancelFunc
	store  *bus.Store
	// drained runs the API as serve does, with its drain (maintenance.go):
	// a start clears an older maintenance, and drain is this run's.
	drained bool
	drain   *Drain
}

func newChaosAPI(t *testing.T, f *stFixture, limits StreamLimits, queue bus.QueueLimits) *chaosAPI {
	return startChaosAPI(&chaosAPI{t: t, f: f, limits: limits, queue: queue})
}

func startChaosAPI(a *chaosAPI) *chaosAPI {
	t := a.t
	a.app = strings.ReplaceAll(awUnique("cbchaos"), "-", "_")
	a.start()
	t.Cleanup(func() {
		if a.srv != nil {
			a.cancel()
			a.srv.Close()
			a.store.Close()
		}
	})
	return a
}

// dsn is the API's own connection string, its backends named for crash.
func (a *chaosAPI) dsn() string {
	sep := "?"
	if strings.Contains(a.f.dsn, "?") {
		sep = "&"
	}
	return a.f.dsn + sep + "application_name=" + a.app
}

func (a *chaosAPI) start() {
	a.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	store, err := bus.OpenPostgres(ctx, a.dsn(), a.f.owner)
	if err != nil {
		a.t.Fatal(err)
	}
	store.RequireVerifiedAccount()
	store.SetQueueLimits(a.queue)
	hub := NewStreamHub()
	options := HandlerOptions{Streams: hub, StreamLimits: a.limits}
	var drain *Drain
	if a.drained {
		drain = NewDrain(store, a.app, time.Now)
		clearAtStart(ctx, store, drain)
		store.SetQuiet(drain.Quiet)
		hub.maintenance = drain.Invalidate
		drain.onStart, drain.onEnd = hub.StartDrain, hub.EndDrain
		options.Drain = drain
		go drain.Run(ctx)
	}
	a.drain = drain
	go hub.Listen(ctx, a.dsn())
	go runSweeper(ctx, store, 200*time.Millisecond, drain, nil)
	addr := a.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var ln net.Listener
	for deadline := time.Now().Add(5 * time.Second); ; {
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		if time.Now().After(deadline) {
			a.t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.addr = ln.Addr().String()
	verifier := VerifierFunc(func(_ context.Context, token string) (Identity, error) {
		return Identity{Principal: strings.TrimPrefix(token, "tok-"), Subject: "sub-" + strings.TrimPrefix(token, "tok-")}, nil
	})
	a.srv = &http.Server{Handler: NewHandlerWithOptions(store, verifier, options)}
	a.cancel, a.store = cancel, store
	go a.srv.Serve(ln)
}

// crash kills the API as kill -9 would: its database work aborts, its
// connections drop, and nothing says bye.
func (a *chaosAPI) crash() {
	a.t.Helper()
	a.terminate("")
	a.cancel()
	a.srv.Close()
	a.store.Close()
	a.srv = nil
}

// terminate ends the API's Postgres backends whose query starts with
// prefix ("" for all).
func (a *chaosAPI) terminate(prefix string) {
	a.t.Helper()
	db, err := sqlOpen(a.f.dsn)
	if err != nil {
		a.t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name=$1 AND query LIKE $2 AND pid<>pg_backend_pid()`, a.app, prefix+"%"); err != nil {
		a.t.Fatal(err)
	}
}

func (a *chaosAPI) url() string { return "http://" + a.addr }

// chaosConsumer handles a session's messages from its consume stream: the
// effect is a message to the sink with operation_id msg:<id>:effect, so a
// redelivery replays it rather than repeating it; then the ack.
type chaosConsumer struct {
	t         *testing.T
	f         *stFixture
	url       string
	session   string
	principal string
	tag       string
	// sink is the session each effect is sent to.
	sink     string
	liveness time.Duration
	// hold makes the consumer take messages without handling them.
	hold atomic.Bool
	// killAfterEffect kills the consumer after its next effect, unacked.
	killAfterEffect atomic.Bool

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	attempts map[string][]float64
	connects int
	// ends counts the streams that ended with an error.
	ends int
	// errs counts the handler's failed calls by message; last is when it
	// last finished a message: the soak's diagnosis.
	errs map[string]int
	last time.Time
}

func newChaosConsumer(t *testing.T, f *stFixture, url, session, principal, tag string) *chaosConsumer {
	c := &chaosConsumer{t: t, f: f, url: url, session: session, principal: principal, tag: tag, sink: "sink", attempts: map[string][]float64{}, errs: map[string]int{}}
	t.Cleanup(c.kill)
	return c
}

func (c *chaosConsumer) client() *remote.Client {
	client, err := remote.NewWithTokenSource(remote.Config{URL: c.url, Audience: "audience", Principal: c.principal, CachePath: filepath.Join(c.t.TempDir(), "cache.db")}, remote.TokenSourceFunc(func(context.Context) (string, error) { return "tok-" + c.principal, nil }))
	if err != nil {
		c.t.Fatal(err)
	}
	return client
}

func (c *chaosConsumer) request(op string, args map[string]any) bus.Request {
	args["instance"] = c.session + "-inst"
	return bus.Request{Operation: op, Workspace: c.f.workspace, Project: "p", Session: c.session, Args: args}
}

func (c *chaosConsumer) start() {
	client := c.client()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.mu.Lock()
	c.cancel, c.done = cancel, done
	c.mu.Unlock()
	scope := remote.StreamScope{Workspace: c.f.workspace, Project: "p", Session: c.session, Instance: c.session + "-inst", Mode: bus.StreamConsume}
	go func() {
		defer close(done)
		// A stream the server ended for good is started again, as an
		// agent's supervisor would, and said so.
		for ctx.Err() == nil {
			err := client.Stream(ctx, scope, remote.StreamOptions{MaxBackoff: 500 * time.Millisecond, Liveness: c.liveness, OnConnect: func() {
				c.mu.Lock()
				c.connects++
				c.mu.Unlock()
			}}, func(e remote.StreamEvent) error {
				if e.Name != "message" {
					return nil
				}
				var m struct {
					LeaseID      string  `json:"lease_id"`
					Attempt      float64 `json:"attempt"`
					RecipientSeq int64   `json:"recipient_seq"`
					Message      struct {
						ID string `json:"id"`
					} `json:"message"`
				}
				if json.Unmarshal(e.Data, &m) != nil {
					return nil
				}
				c.mu.Lock()
				c.attempts[m.Message.ID] = append(c.attempts[m.Message.ID], m.Attempt)
				c.mu.Unlock()
				if c.hold.Load() {
					return nil
				}
				// A rate-limited effect waits and retries, as an agent would.
				for {
					_, err := client.Execute(ctx, c.request("send", map[string]any{"to": c.sink, "body": c.tag + ":" + m.Message.ID, "operation_id": "msg:" + m.Message.ID + ":effect"}))
					if err == nil {
						break
					}
					c.failed("effect", err)
					if ctx.Err() != nil || !strings.Contains(err.Error(), "rate") {
						return ctx.Err()
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(time.Second):
					}
				}
				if c.killAfterEffect.CompareAndSwap(true, false) {
					cancel()
					return ctx.Err()
				}
				if _, err := client.Execute(ctx, c.request("ack", map[string]any{"acks": []any{map[string]any{"message_id": m.Message.ID, "lease_id": m.LeaseID}}, "operation_id": "msg:" + m.Message.ID + ":ack"})); err != nil {
					return ctx.Err()
				}
				return nil
			})
			if ctx.Err() == nil {
				c.mu.Lock()
				c.ends++
				c.mu.Unlock()
				c.t.Logf("%s: the stream ended (%v); starting it again", c.session, err)
				time.Sleep(200 * time.Millisecond)
			}
		}
	}()
}

// failed counts a handler call that failed, by kind and message.
func (c *chaosConsumer) failed(kind string, err error) {
	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:120]
	}
	c.mu.Lock()
	c.errs[kind+": "+msg]++
	c.mu.Unlock()
}

// diagnose logs what the consumer saw and what the server holds for it.
func (c *chaosConsumer) diagnose() {
	c.mu.Lock()
	c.t.Logf("%s: %d messages offered, %d connects, last handled %v ago, errors %v", c.session, len(c.attempts), c.connects, time.Since(c.last).Round(time.Second), c.errs)
	c.mu.Unlock()
	db, err := sqlOpen(c.f.dsn)
	if err != nil {
		return
	}
	defer db.Close()
	rows, err := db.Query(`SELECT d.attempts, d.lease_id IS NOT NULL, COALESCE(d.lease_consumer,''), d.acked_at IS NOT NULL, d.dead_at IS NOT NULL, COUNT(*) FROM deliveries d JOIN sessions s ON s.id=d.recipient_id AND s.project_id=d.project_id JOIN projects p ON p.id=s.project_id JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND s.name=$2 GROUP BY 1,2,3,4,5 ORDER BY 1`, c.f.workspace, c.session)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var attempts, n int
		var leased, acked, dead bool
		var consumer string
		if rows.Scan(&attempts, &leased, &consumer, &acked, &dead, &n) == nil {
			c.t.Logf("%s: %d deliveries attempts=%d leased=%v consumer=%q acked=%v dead=%v", c.session, n, attempts, leased, consumer, acked, dead)
		}
	}
	var policy string
	_ = db.QueryRow(`SELECT s.notification_policy FROM sessions s JOIN projects p ON p.id=s.project_id JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND s.name=$2`, c.f.workspace, c.session).Scan(&policy)
	c.t.Logf("%s: policy %s", c.session, policy)
	if path := os.Getenv("CHAOS_STACKS"); path != "" {
		if f, err := os.Create(path); err == nil {
			_ = pprof.Lookup("goroutine").WriteTo(f, 1)
			f.Close()
			c.t.Logf("goroutine stacks in %s", path)
		}
	}
}

// kill stops the consumer at once, whatever it was doing.
func (c *chaosConsumer) kill() {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// deliveredInOrder checks, on the server's record, that a session's
// deliveries were first leased in recipient_seq order: delivered_at is set
// at a delivery's first lease and never moves. What a consumer processes
// after a crash can't show it: a push may die in a dead client's buffer
// and come again after later messages, as at-least-once delivery allows.
func deliveredInOrder(t *testing.T, f *stFixture, session string) {
	t.Helper()
	db, err := sqlOpen(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT d.recipient_seq,d.delivered_at FROM deliveries d JOIN sessions s ON s.id=d.recipient_id AND s.project_id=d.project_id JOIN projects p ON p.id=s.project_id JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND s.name=$2 ORDER BY d.recipient_seq`, f.workspace, session)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var last time.Time
	n := 0
	for rows.Next() {
		var seq int64
		var at *string
		if err = rows.Scan(&seq, &at); err != nil {
			t.Fatal(err)
		}
		n++
		if at == nil {
			t.Fatalf("%s: delivery %d was never leased", session, seq)
		}
		when, err := time.Parse(time.RFC3339Nano, *at)
		if err != nil {
			t.Fatal(err)
		}
		if when.Before(last) {
			t.Fatalf("%s: delivery %d was first leased before an earlier one", session, seq)
		}
		last = when
	}
	if n == 0 {
		t.Fatalf("%s: no deliveries found", session)
	}
}

// effects is each handled message's effect count, by message ID.
func effects(t *testing.T, f *stFixture, tag string) map[string]int {
	t.Helper()
	db, err := sqlOpen(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT body,COUNT(*) FROM messages WHERE body LIKE $1 GROUP BY body`, tag+":%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var body string
		var n int
		if err = rows.Scan(&body, &n); err != nil {
			t.Fatal(err)
		}
		out[strings.TrimPrefix(body, tag+":")] = n
	}
	return out
}

// queueCounts is a session's unacked and dead counts, read as its owner.
func queueCounts(f *stFixture, session, principal string) (int64, int64) {
	f.t.Helper()
	data := f.do(principal, bus.Request{Operation: "notification_status", Workspace: f.workspace, Project: "p", Session: session, Args: map[string]any{"instance": session + "-inst", "queue": true}}).(map[string]any)
	return data["unacked_count"].(int64), data["dead_count"].(int64)
}

// settled waits until every sent message's effect happened once and the
// session has nothing unacked, then checks no effect happened twice.
func settled(t *testing.T, f *stFixture, tag, session string, sent []string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := effects(t, f, tag)
		unacked, dead := queueCounts(f, session, f.owner)
		done := unacked == 0 && dead == 0
		for _, id := range sent {
			done = done && got[id] == 1
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not settled: %d of %d effects, %d unacked, %d dead", len(got), len(sent), unacked, dead)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for id, n := range effects(t, f, tag) {
		if n != 1 {
			t.Fatalf("message %s handled %d times", id, n)
		}
	}
}

func chaosFixture(t *testing.T, queue bus.QueueLimits) *stFixture {
	f := newSTFixture(t, StreamLimits{Reread: time.Hour}, queue)
	f.session("sink", bus.Request{Operation: "register_session", Args: map[string]any{"role": "worker"}})
	return f
}

// The API dies (kill -9) with messages in flight and comes back: every
// message's effect happens exactly once and every delivery is acked.
func TestChaosAPICrash(t *testing.T) {
	queue := bus.QueueLimits{Lease: 3 * time.Second}
	f := chaosFixture(t, queue)
	api := newChaosAPI(t, f, StreamLimits{Reread: 2 * time.Second}, queue)
	tag := awUnique("crash")
	c := newChaosConsumer(t, f, api.url(), "b", f.owner, tag)
	c.start()
	var sent []string
	for i := range 30 {
		sent = append(sent, f.send("b", fmt.Sprint("first ", i)))
	}
	for deadline := time.Now().Add(20 * time.Second); len(effects(t, f, tag)) < 10; {
		if time.Now().After(deadline) {
			t.Fatal("nothing handled before the crash")
		}
		time.Sleep(20 * time.Millisecond)
	}
	api.crash()
	for i := range 30 {
		sent = append(sent, f.send("b", fmt.Sprint("while down ", i)))
	}
	time.Sleep(300 * time.Millisecond)
	api.start()
	settled(t, f, tag, "b", sent, 60*time.Second)
	deliveredInOrder(t, f, "b")
}

// The hub's LISTEN connection is killed mid-burst: nothing is missed, and
// with the periodic re-read at 30 s, nothing waits that long.
func TestChaosListenerKilledMidBurst(t *testing.T) {
	f := chaosFixture(t, bus.QueueLimits{})
	api := newChaosAPI(t, f, StreamLimits{}, bus.QueueLimits{})
	tag := awUnique("listen")
	c := newChaosConsumer(t, f, api.url(), "b", f.owner, tag)
	c.start()
	var sent []string
	for i := range 40 {
		sent = append(sent, f.send("b", fmt.Sprint("burst ", i)))
		if i == 15 {
			api.terminate("LISTEN")
		}
	}
	start := time.Now()
	settled(t, f, tag, "b", sent, 30*time.Second)
	t.Logf("settled %v after the burst", time.Since(start))
}

// A consumer dies mid-handle, after its effect and before its ack: the
// message is offered again to the next consumer, whose replayed effect is
// the same one, so it is handled once.
func TestChaosConsumerKilledMidHandle(t *testing.T) {
	queue := bus.QueueLimits{Lease: 2 * time.Second}
	f := chaosFixture(t, queue)
	api := newChaosAPI(t, f, StreamLimits{Reread: time.Second}, queue)
	tag := awUnique("consumer")
	first := newChaosConsumer(t, f, api.url(), "b", f.owner, tag)
	first.killAfterEffect.Store(true)
	first.start()
	sent := []string{f.send("b", "doomed handler")}
	<-first.done
	if got := effects(t, f, tag); got[sent[0]] != 1 {
		t.Fatal("no effect before the kill", got)
	}
	if unacked, _ := queueCounts(f, "b", f.owner); unacked != 1 {
		t.Fatal("acked before the kill", unacked)
	}
	second := newChaosConsumer(t, f, api.url(), "b", f.owner, tag)
	second.start()
	sent = append(sent, f.send("b", "after"))
	settled(t, f, tag, "b", sent, 30*time.Second)
}

// The network stalls under the stream (a proxy stops forwarding without
// closing): the client gives the connection up after its liveness window,
// reconnects, and nothing is lost.
func TestChaosNetworkStall(t *testing.T) {
	f := chaosFixture(t, bus.QueueLimits{})
	api := newChaosAPI(t, f, StreamLimits{Ping: 300 * time.Millisecond, Reread: time.Second}, bus.QueueLimits{})
	proxy := newStallProxy(t, api.addr)
	tag := awUnique("stall")
	c := newChaosConsumer(t, f, "http://"+proxy.addr, "b", f.owner, tag)
	c.liveness = 2 * time.Second
	c.start()
	var sent []string
	for i := range 5 {
		sent = append(sent, f.send("b", fmt.Sprint("before ", i)))
	}
	settled(t, f, tag, "b", sent, 20*time.Second)
	proxy.stallStreams()
	for i := range 5 {
		sent = append(sent, f.send("b", fmt.Sprint("stalled ", i)))
	}
	settled(t, f, tag, "b", sent, 30*time.Second)
	c.mu.Lock()
	connects := c.connects
	c.mu.Unlock()
	if connects < 2 {
		t.Fatal("the stalled stream was never given up", connects)
	}
}

// stallProxy forwards TCP to the API; stallStreams freezes every stream
// connection open so far, both ways, without closing it.
type stallProxy struct {
	addr string
	mu   sync.Mutex
	gate []chan struct{}
}

func newStallProxy(t *testing.T, target string) *stallProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	p := &stallProxy{addr: ln.Addr().String()}
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(in, target)
		}
	}()
	return p
}

func (p *stallProxy) serve(in net.Conn, target string) {
	defer in.Close()
	out, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer out.Close()
	// A connection is a stream's once a request on it opens the stream:
	// the client may reuse one that carried other requests first.
	stalled := make(chan struct{})
	var marked atomic.Bool
	pump := func(dst io.Writer, src io.Reader, upstream bool) {
		buf := make([]byte, 4096)
		for {
			n, err := src.Read(buf)
			if upstream && !marked.Load() && strings.Contains(string(buf[:n]), "GET "+StreamPath) {
				marked.Store(true)
				p.mu.Lock()
				p.gate = append(p.gate, stalled)
				p.mu.Unlock()
			}
			select {
			case <-stalled:
				// Frozen: hold the connection open, forward nothing.
				select {}
			default:
			}
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go pump(out, in, true)
	pump(in, out, false)
}

func (p *stallProxy) stallStreams() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, g := range p.gate {
		close(g)
	}
	p.gate = nil
}

// A compaction cycle: the session's notifications are held and its leases
// given back (context compacting's steps), then accepted again (context
// bind's): no attempt is counted, and everything is offered again.
func TestChaosCompactionCycle(t *testing.T) {
	f := chaosFixture(t, bus.QueueLimits{})
	api := newChaosAPI(t, f, StreamLimits{Reread: time.Hour}, bus.QueueLimits{})
	tag := awUnique("compact")
	c := newChaosConsumer(t, f, api.url(), "b", f.owner, tag)
	c.hold.Store(true)
	c.start()
	var sent []string
	for i := range 3 {
		sent = append(sent, f.send("b", fmt.Sprint("held ", i)))
	}
	leases := map[string]string{}
	for deadline := time.Now().Add(10 * time.Second); len(leases) < 3; {
		if time.Now().After(deadline) {
			t.Fatal("never offered", leases)
		}
		db, err := sqlOpen(f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range sent {
			var lease *string
			_ = db.QueryRow(`SELECT lease_id FROM deliveries WHERE message_id=$1`, id).Scan(&lease)
			if lease != nil {
				leases[id] = *lease
			}
		}
		db.Close()
		time.Sleep(20 * time.Millisecond)
	}
	f.session("b", bus.Request{Operation: "set_notification_policy", Args: map[string]any{"policy": "hold"}})
	for id, lease := range leases {
		out := f.session("b", bus.Request{Operation: "nack", Args: map[string]any{"message_id": id, "lease_id": lease, "delay_seconds": 0}}).(map[string]any)
		if out["outcome"] != "released" {
			t.Fatal(out)
		}
	}
	time.Sleep(300 * time.Millisecond)
	c.hold.Store(false)
	f.session("b", bus.Request{Operation: "set_notification_policy", Args: map[string]any{"policy": "accept"}})
	settled(t, f, tag, "b", sent, 20*time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range sent {
		offers := c.attempts[id]
		if len(offers) != 2 || offers[0] != 1 || offers[1] != 1 {
			t.Fatalf("%s offered %v: the compaction counted an attempt", id, offers)
		}
	}
}

// The soak: 10k messages over 50 sessions while consumers are killed at
// random and come back. Every delivery is acked or dead, each session's
// deliveries were first leased in order, and no message is handled twice. The kills follow a
// seeded random sequence: CHAOS_SEED replays a failed run.
func TestChaosSoak(t *testing.T) {
	if testing.Short() {
		t.Skip("the soak runs nightly and on demand")
	}
	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv("CHAOS_SEED"); s != "" {
		var err error
		if seed, err = strconv.ParseUint(s, 10, 64); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("replay with CHAOS_SEED=%d", seed)
		}
	})
	rng := rand.New(rand.NewPCG(seed, seed))
	sessions, perSession := 50, 200
	if s := os.Getenv("CHAOS_MESSAGES"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < sessions {
			t.Fatal("CHAOS_MESSAGES must be at least", sessions)
		}
		perSession = n / sessions
	}
	// A session sends at most 120 a minute, a principal 600: each
	// session gets its own feeder, and every session and feeder its own
	// principal. A rate-limited effect waits, so the leases are long.
	// Every write in a workspace takes the workspace's lock, so one
	// workspace's writes run one at a time: the sessions are spread over
	// ten workspaces, all on one API.
	// CHAOS_WORKSPACES=1 puts every session in one workspace, to measure
	// that lock.
	queue := bus.QueueLimits{Lease: time.Minute}
	groups := 10
	if s := os.Getenv("CHAOS_WORKSPACES"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > sessions {
			t.Fatal("CHAOS_WORKSPACES must be 1 to", sessions)
		}
		groups = n
	}
	workspaces := make([]*stFixture, groups)
	for g := range groups {
		workspaces[g] = newSTFixture(t, StreamLimits{Reread: time.Hour}, queue)
	}
	api := newChaosAPI(t, workspaces[0], StreamLimits{Reread: 2 * time.Second, PerPrincipal: 4}, queue)
	fx := make([]*stFixture, sessions)
	tag := awUnique("soak")
	consumers := make([]*chaosConsumer, sessions)
	names := make([]string, sessions)
	principals := make([]string, sessions)
	feeders := make([]string, sessions)
	register := func(f *stFixture, name string) string {
		principal := awUnique(name) + "@stream-test.example"
		f.do(f.owner, bus.Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": principal, "role": "member"}})
		f.do(principal, bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: name, Args: map[string]any{"role": "worker", "instance": name + "-inst"}})
		return principal
	}
	for i := range sessions {
		fx[i] = workspaces[i*groups/sessions]
		names[i] = fmt.Sprintf("s%02d", i)
		principals[i] = register(fx[i], names[i])
		feeders[i] = register(fx[i], fmt.Sprintf("f%02d", i))
		consumers[i] = newChaosConsumer(t, fx[i], api.url(), names[i], principals[i], tag)
		// Effects go back to the session's own feeder: one shared sink
		// would serialize every effect on its delivery counter.
		consumers[i].sink = fmt.Sprintf("f%02d", i)
		consumers[i].start()
	}
	stop := make(chan struct{})
	var killer sync.WaitGroup
	killer.Add(1)
	go func() {
		defer killer.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(200+rng.IntN(800)) * time.Millisecond):
			}
			c := consumers[rng.IntN(sessions)]
			c.kill()
			c.start()
		}
	}()
	sent := make([][]string, sessions)
	var feeding sync.WaitGroup
	for i := range sessions {
		feeding.Add(1)
		go func() {
			defer feeding.Done()
			feeder := fmt.Sprintf("f%02d", i)
			for round := range perSession {
				r := bus.Request{Operation: "send", Workspace: fx[i].workspace, Project: "p", Session: feeder, Args: map[string]any{"instance": feeder + "-inst", "to": names[i], "body": fmt.Sprint("soak ", round), "operation_id": fmt.Sprint("feed-", round)}}
				for {
					out, err := fx[i].exec(feeders[i], r)
					if err == nil {
						sent[i] = append(sent[i], out.Data.(map[string]any)["id"].(string))
						break
					}
					if !strings.Contains(err.Error(), "rate limit") {
						t.Error(err)
						return
					}
					time.Sleep(time.Second)
				}
			}
		}()
	}
	began := time.Now()
	feeding.Wait()
	if t.Failed() {
		return
	}
	t.Logf("fed %d messages in %v", sessions*perSession, time.Since(began).Round(time.Second))
	t.Cleanup(func() { t.Logf("done %v after the feed began", time.Since(began).Round(time.Second)) })
	close(stop)
	killer.Wait()
	settle := 20 * time.Minute
	if s := os.Getenv("CHAOS_SETTLE"); s != "" {
		var err error
		if settle, err = time.ParseDuration(s); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(settle)
	for i, name := range names {
		for {
			got := effects(t, fx[i], tag)
			unacked, dead := queueCounts(fx[i], name, principals[i])
			handled := 0
			for _, id := range sent[i] {
				if got[id] > 1 {
					t.Fatalf("%s: message %s handled %d times", name, id, got[id])
				}
				handled += got[id]
			}
			// Acked or dead: a dead letter's effect may or may not have run.
			if unacked == 0 && int64(handled)+dead >= int64(len(sent[i])) {
				break
			}
			if time.Now().After(deadline) {
				consumers[i].diagnose()
				t.Fatalf("%s not settled: %d handled, %d unacked, %d dead of %d", name, handled, unacked, dead, len(sent[i]))
			}
			time.Sleep(200 * time.Millisecond)
		}
		deliveredInOrder(t, fx[i], name)
	}
}
