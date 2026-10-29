//go:build chaos

package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/outbox"
	"clawbridge/internal/remote"
)

// The zero-loss deploy's chaos suite (ZERO-LOSS-DEPLOY §5), against real
// Postgres: go test -tags chaos ./internal/server -run ChaosZeroLoss.
// Each test has its own schema, since a maintenance is the whole
// database's. A serving process is a chaosAPI run as serve runs it, with
// its drain; a swap starts a second one on the same database (its start
// ends the maintenance, as the new binary's does) and points the clients'
// proxy, the load balancer, at it. The clients are hosted clients with a
// profile cache, so with an outbox (internal/outbox) beside it.

// zlFixture is chaosFixture in a schema of its own; op is the operator's
// store, which starts and ends a maintenance.
func zlFixture(t *testing.T, queue bus.QueueLimits) (f *stFixture, op *bus.Store) {
	op, dsn := drainSchemaStore(t)
	t.Setenv("CLAWBRIDGE_TEST_POSTGRES_DSN", dsn)
	return chaosFixture(t, queue), op
}

// servingAPI is a serving process with its drain, reported to the
// database before it answers.
func servingAPI(t *testing.T, f *stFixture, op *bus.Store, queue bus.QueueLimits) *chaosAPI {
	a := startChaosAPI(&chaosAPI{t: t, f: f, limits: StreamLimits{Reread: time.Second}, queue: queue, drained: true})
	within(t, 5*time.Second, "the server never reported itself", func() bool {
		list, err := op.MaintenanceInstances(context.Background())
		for _, i := range list {
			if i.Instance == a.app {
				return err == nil
			}
		}
		return false
	})
	return a
}

// zlMaintenance starts a maintenance as the operator does, and waits
// until every live server has drained.
func zlMaintenance(t *testing.T, op *bus.Store, ttl time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := op.StartMaintenance(ctx, "deploy", "operator", ttl); err != nil {
		t.Fatal(err)
	}
	if err := waitDrained(ctx, op, 10*time.Second, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// zlEnd ends the maintenance and waits until api has seen it.
func zlEnd(t *testing.T, op *bus.Store, api *chaosAPI) {
	t.Helper()
	if _, err := op.EndMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	within(t, 3*time.Second, "the server never saw the end", func() bool { return !api.drain.Quiet() })
}

// zlProxy is the load balancer: it forwards to its target, which a swap
// changes, and answers 502 with none. An answer it is told to lose is
// taken from the server, then dropped with the connection, as when a
// server dies between its commit and its answer.
type zlProxy struct {
	srv *httptest.Server

	mu     sync.Mutex
	target string
	// fate decides, for a POST, what becomes of its answer.
	fate func(operation string) zlFate
	// lost holds the answers the proxy dropped, as the server wrote them.
	lost [][]byte
	// held is signalled when an answer is held; release frees them.
	held    chan struct{}
	release chan struct{}
	// streams counts the stream connections asked for.
	streams int
}

type zlFate int

const (
	zlPass zlFate = iota
	// zlLose drops the answer, and the proxy has no target after it.
	zlLose
	// zlHold keeps the answer, and the client waiting, until release.
	zlHold
)

func newZLProxy(t *testing.T, target string) *zlProxy {
	p := &zlProxy{target: target, held: make(chan struct{}, 16), release: make(chan struct{})}
	p.srv = httptest.NewServer(p)
	t.Cleanup(func() {
		p.free()
		p.srv.CloseClientConnections()
		p.srv.Close()
	})
	return p
}

func (p *zlProxy) url() string { return p.srv.URL }

func (p *zlProxy) point(target string) {
	p.mu.Lock()
	p.target = target
	p.mu.Unlock()
}

func (p *zlProxy) decide(fate func(string) zlFate) {
	p.mu.Lock()
	p.fate = fate
	p.mu.Unlock()
}

func (p *zlProxy) free() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.release:
	default:
		close(p.release)
	}
}

func (p *zlProxy) streamsAsked() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streams
}

func (p *zlProxy) lostAnswers() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.lost...)
}

func (p *zlProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	target, fate := p.target, p.fate
	if r.Method == http.MethodGet && r.URL.Path == StreamPath {
		p.streams++
	}
	p.mu.Unlock()
	if target == "" {
		http.Error(w, "no upstream", http.StatusBadGateway)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Operation string `json:"operation"`
	}
	json.Unmarshal(body, &req)
	what := zlPass
	if fate != nil && r.Method == http.MethodPost {
		what = fate(req.Operation)
	}
	upstream, _ := url.Parse(target)
	if what == zlPass {
		r.Body = io.NopCloser(bytes.NewReader(body))
		(&httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(upstream) }, FlushInterval: -1}).ServeHTTP(w, r)
		return
	}
	out, _ := http.NewRequestWithContext(context.WithoutCancel(r.Context()), r.Method, target+r.URL.RequestURI(), bytes.NewReader(body))
	out.Header = r.Header.Clone()
	if resp, err := http.DefaultTransport.RoundTrip(out); err == nil {
		answer, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		p.mu.Lock()
		p.lost = append(p.lost, answer)
		if what == zlLose {
			p.target = ""
		}
		p.mu.Unlock()
	}
	if what == zlHold {
		p.held <- struct{}{}
		select {
		case <-p.release:
		case <-r.Context().Done():
		}
	}
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		conn.Close()
	}
}

// zlClient is a hosted client whose profile lives in dir: two clients on
// one dir share an outbox, as two processes on one profile do.
func zlClient(t *testing.T, server, principal, dir string) *remote.Client {
	t.Helper()
	c, err := remote.NewWithTokenSource(remote.Config{URL: server, Audience: "audience", Principal: principal, CachePath: filepath.Join(dir, "cache.db")}, remote.TokenSourceFunc(func(context.Context) (string, error) { return "tok-" + principal, nil }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ob := c.Outbox(); ob != nil {
			ob.Close()
		}
	})
	return c
}

func zlRequest(f *stFixture, session, op string, args map[string]any) bus.Request {
	args["instance"] = session + "-inst"
	return bus.Request{Operation: op, Workspace: f.workspace, Project: "p", Session: session, Args: args}
}

// zlSend sends as session, the call cut at within: the outbox takes over
// when it ends first.
func zlSend(c *remote.Client, f *stFixture, session, to, body, operationID string, within time.Duration) (bus.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	return c.Execute(ctx, zlRequest(f, session, "send", map[string]any{"to": to, "body": body, "operation_id": operationID}))
}

// queuedLocal checks that a write waits in the outbox.
func queuedLocal(t *testing.T, res bus.Result, err error) {
	t.Helper()
	data, _ := res.Data.(map[string]any)
	if err != nil || res.Source != "client_outbox" || data["outcome"] != "queued_local" {
		t.Fatalf("not queued locally: %v %v", res, err)
	}
}

// drainAll drains the clients' outboxes until nothing waits in them.
func drainAll(t *testing.T, within time.Duration, clients ...*remote.Client) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		waiting := 0
		for _, c := range clients {
			if _, err := c.DrainOutbox(context.Background(), 5*time.Second); err != nil {
				t.Fatal(err)
			}
			st, err := c.Outbox().Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			waiting += st.Waiting
		}
		if waiting == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d write(s) still wait after %s", waiting, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func zlDB(t *testing.T, f *stFixture) *sql.DB {
	t.Helper()
	db, err := sqlOpen(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// zlMessages is each message's IDs, by body, for the bodies starting tag.
func zlMessages(t *testing.T, f *stFixture, tag string) map[string][]string {
	t.Helper()
	rows, err := zlDB(t, f).Query(`SELECT body,id FROM messages WHERE body LIKE $1 ORDER BY sequence`, tag+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var body, id string
		if err = rows.Scan(&body, &id); err != nil {
			t.Fatal(err)
		}
		out[body] = append(out[body], id)
	}
	return out
}

// zlInbox is what a session was delivered, in recipient_seq order: each
// body starting tag, and every sequence number the session has.
func zlInbox(t *testing.T, f *stFixture, session, tag string) (bodies []string, seqs []int64) {
	t.Helper()
	rows, err := zlDB(t, f).Query(`SELECT m.body,d.recipient_seq FROM deliveries d JOIN messages m ON m.id=d.message_id AND m.project_id=d.project_id JOIN sessions s ON s.id=d.recipient_id AND s.project_id=d.project_id JOIN projects p ON p.id=s.project_id JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND s.name=$2 ORDER BY d.recipient_seq`, f.workspace, session)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		var seq int64
		if err = rows.Scan(&body, &seq); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
		if strings.HasPrefix(body, tag) {
			bodies = append(bodies, body)
		}
	}
	return bodies, seqs
}

// exactlyOnce checks that each body exists as one message, and nothing
// else under tag does.
func exactlyOnce(t *testing.T, f *stFixture, tag string, bodies []string) map[string][]string {
	t.Helper()
	got := zlMessages(t, f, tag)
	for _, b := range bodies {
		if len(got[b]) != 1 {
			t.Fatalf("%q exists %d times", b, len(got[b]))
		}
	}
	if len(got) != len(bodies) {
		t.Fatalf("%d messages under %s, want %d", len(got), tag, len(bodies))
	}
	return got
}

// inOrder checks that a recipient got each sender's messages in the
// order they were sent: a body is tag:sender:n.
func inOrder(t *testing.T, recipient string, bodies []string) {
	t.Helper()
	last := map[string]int{}
	for _, b := range bodies {
		parts := strings.Split(b, ":")
		n, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			t.Fatal(b)
		}
		sender := parts[len(parts)-2]
		if prev, ok := last[sender]; ok && n <= prev {
			t.Fatalf("%s got %s's %d after its %d", recipient, sender, n, prev)
		}
		last[sender] = n
	}
}

// gapFree checks a recipient's sequence numbers run without a gap or a
// repeat.
func gapFree(t *testing.T, session string, seqs []int64) {
	t.Helper()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("%s: recipient_seq %d follows %d", session, seqs[i], seqs[i-1])
		}
	}
}

// Case 1, a swap mid-send: the server commits the message and dies before
// answering. The client queues it and replays it to the new server, and
// one message exists, with the first one's ID.
func TestChaosZeroLossSwapMidSend(t *testing.T) {
	f, op := zlFixture(t, bus.QueueLimits{})
	blue := servingAPI(t, f, op, bus.QueueLimits{})
	p := newZLProxy(t, blue.url())
	c := zlClient(t, p.url(), f.owner, t.TempDir())
	tag := awUnique("midsend")
	p.decide(func(op string) zlFate {
		if op == "send" {
			return zlLose
		}
		return zlPass
	})
	res, err := zlSend(c, f, "a", "b", tag+":a:1", "op-"+tag, 2*time.Second)
	queuedLocal(t, res, err)
	blue.crash()
	lost := p.lostAnswers()
	if len(lost) != 1 {
		t.Fatalf("%d answers lost", len(lost))
	}
	var first struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(lost[0], &first) != nil || first.Data.ID == "" {
		t.Fatalf("the server didn't commit before it died: %s", lost[0])
	}
	p.decide(nil)
	green := servingAPI(t, f, op, bus.QueueLimits{})
	p.point(green.url())
	drainAll(t, 20*time.Second, c)
	noDeadLetters(t, c)
	got := exactlyOnce(t, f, tag, []string{tag + ":a:1"})
	if id := got[tag+":a:1"][0]; id != first.Data.ID {
		t.Fatalf("the replay made %s, not the first message %s", id, first.Data.ID)
	}
}

// Case 2, a swap before commit: the server dies mid-transaction. The
// client queues the send and replays it, and one message exists.
func TestChaosZeroLossSwapBeforeCommit(t *testing.T) {
	f, op := zlFixture(t, bus.QueueLimits{})
	blue := servingAPI(t, f, op, bus.QueueLimits{})
	p := newZLProxy(t, blue.url())
	c := zlClient(t, p.url(), f.owner, t.TempDir())
	tag := awUnique("precommit")
	db := zlDB(t, f)
	// The send's insert waits on this lock, inside its transaction.
	lock, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`LOCK TABLE messages IN EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	type sent struct {
		res bus.Result
		err error
	}
	done := make(chan sent, 1)
	go func() {
		res, err := zlSend(c, f, "a", "b", tag+":a:1", "op-"+tag, 4*time.Second)
		done <- sent{res, err}
	}()
	within(t, 3*time.Second, "the send never waited inside its transaction", func() bool {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock'`, blue.app).Scan(&n)
		return n > 0
	})
	p.point("")
	blue.crash()
	if err = lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	out := <-done
	queuedLocal(t, out.res, out.err)
	if got := zlMessages(t, f, tag); len(got) != 0 {
		t.Fatalf("the aborted transaction committed: %v", got)
	}
	green := servingAPI(t, f, op, bus.QueueLimits{})
	p.point(green.url())
	drainAll(t, 20*time.Second, c)
	noDeadLetters(t, c)
	exactlyOnce(t, f, tag, []string{tag + ":a:1"})
}

// Case 3, drain then swap: 100 sends from 3 sessions to 2 recipients,
// before, during and after the drain. Those during it wait in their
// outboxes and arrive after the swap; every recipient has each sender's
// messages in their order, each once.
func TestChaosZeroLossDrainThenSwap(t *testing.T) {
	f, op := zlFixture(t, bus.QueueLimits{})
	senders := []string{"s1", "s2", "s3"}
	for _, name := range append([]string{"r1", "r2"}, senders...) {
		f.session(name, bus.Request{Operation: "register_session", Args: map[string]any{"role": "worker"}})
	}
	blue := servingAPI(t, f, op, bus.QueueLimits{})
	p := newZLProxy(t, blue.url())
	tag := awUnique("swap")
	clients := map[string]*remote.Client{}
	var all []*remote.Client
	for _, s := range senders {
		clients[s] = zlClient(t, p.url(), f.owner, t.TempDir())
		all = append(all, clients[s])
	}
	var mu sync.Mutex
	var bodies []string
	next := map[string]int{}
	// phase sends n from each sender at once; queued says whether each
	// must wait in its outbox.
	phase := func(n map[string]int, queued bool) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan error, 100)
		for _, s := range senders {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range n[s] {
					mu.Lock()
					i := next[s]
					next[s]++
					mu.Unlock()
					body := fmt.Sprintf("%s:%s:%d", tag, s, i)
					res, err := zlSend(clients[s], f, s, []string{"r1", "r2"}[i%2], body, "op-"+body, 3*time.Second)
					if err != nil {
						errs <- err
						return
					}
					if local := res.Source == "client_outbox"; queued && !local {
						errs <- fmt.Errorf("%s reached a draining server: %v", body, res)
						return
					}
					mu.Lock()
					bodies = append(bodies, body)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
	}
	phase(map[string]int{"s1": 10, "s2": 10, "s3": 10}, false)
	zlMaintenance(t, op, 6*time.Second)
	phase(map[string]int{"s1": 12, "s2": 12, "s3": 12}, true)
	// A drain that meets the maintenance (its Retry-After past, on this
	// outbox's clock) sends nothing, loses nothing, and waits again.
	clients["s1"].Outbox().SetNow(func() time.Time { return time.Now().Add(31 * time.Second) })
	if r, err := clients["s1"].DrainOutbox(context.Background(), 5*time.Second); err != nil || r.Sent != 0 || r.Dead != 0 || r.Waiting != 12 {
		t.Fatalf("a drain in maintenance: %+v %v", r, err)
	}
	green := servingAPI(t, f, op, bus.QueueLimits{})
	p.point(green.url())
	blue.crash()
	phase(map[string]int{"s1": 12, "s2": 11, "s3": 11}, false)
	drainAll(t, 30*time.Second, all...)
	noDeadLetters(t, all...)
	if len(bodies) != 100 {
		t.Fatalf("%d sends", len(bodies))
	}
	exactlyOnce(t, f, tag, bodies)
	want := map[string]int{}
	for _, body := range bodies {
		n, _ := strconv.Atoi(body[strings.LastIndex(body, ":")+1:])
		want[[]string{"r1", "r2"}[n%2]]++
	}
	for _, r := range []string{"r1", "r2"} {
		got, seqs := zlInbox(t, f, r, tag)
		if len(got) != want[r] {
			t.Fatalf("%s got %d messages, want %d", r, len(got), want[r])
		}
		inOrder(t, r, got)
		gapFree(t, r, seqs)
	}
}

// queueWhileDown queues n sends from session a to b while the proxy has
// no server.
func queueWhileDown(t *testing.T, c *remote.Client, f *stFixture, tag string, n int) []string {
	t.Helper()
	var bodies []string
	for i := range n {
		body := fmt.Sprintf("%s:a:%d", tag, i)
		res, err := zlSend(c, f, "a", "b", body, "op-"+body, time.Second)
		queuedLocal(t, res, err)
		bodies = append(bodies, body)
	}
	return bodies
}

// Case 4, the client killed mid-drain: the process dies with a send the
// server committed and never answered, and its drain lease held. The next
// process on the profile takes the queue when the lease runs out, and
// nothing is sent twice or out of order. In the slow variant the first
// process isn't dead but stalled: its answer comes after the second one
// sent everything, and the write it then makes to the outbox changes
// nothing.
func TestChaosZeroLossClientKilledMidDrain(t *testing.T) {
	for _, variant := range []string{"killed", "slow"} {
		t.Run(variant, func(t *testing.T) {
			f, op := zlFixture(t, bus.QueueLimits{})
			blue := servingAPI(t, f, op, bus.QueueLimits{})
			p := newZLProxy(t, "")
			dir := t.TempDir()
			first := zlClient(t, p.url(), f.owner, dir)
			tag := awUnique("killed")
			bodies := queueWhileDown(t, first, f, tag, 20)
			var mu sync.Mutex
			answered := 0
			p.decide(func(op string) zlFate {
				mu.Lock()
				defer mu.Unlock()
				if op != "send" {
					return zlPass
				}
				if answered++; answered == 6 {
					return zlHold
				}
				return zlPass
			})
			p.point(blue.url())
			ctx, kill := context.WithCancel(context.Background())
			dead := make(chan struct{})
			go func() {
				defer close(dead)
				for ctx.Err() == nil {
					first.DrainOutbox(ctx, time.Minute)
					time.Sleep(100 * time.Millisecond)
				}
			}()
			select {
			case <-p.held:
			case <-time.After(20 * time.Second):
				t.Fatal("the first process never reached its sixth send")
			}
			if got := zlMessages(t, f, tag); len(got) != 6 {
				t.Fatalf("%d messages when the process stopped, want 6", len(got))
			}
			// The first process holds its lease and its sixth send.
			p.decide(nil)
			second := zlClient(t, p.url(), f.owner, dir)
			if r, _ := second.DrainOutbox(context.Background(), 5*time.Second); r.Sent != 0 {
				t.Fatalf("the second process sent %d under the first one's lease", r.Sent)
			}
			second.Outbox().SetNow(func() time.Time { return time.Now().Add(31 * time.Second) })
			drainAll(t, 20*time.Second, second)
			if variant == "slow" {
				// Its answer arrives now; it drains on, finding nothing.
				p.free()
				time.Sleep(1500 * time.Millisecond)
			}
			kill()
			p.free()
			<-dead
			for _, c := range []*remote.Client{first, second} {
				if st, err := c.Outbox().Status(context.Background()); err != nil || st.Waiting != 0 || st.Dead != 0 {
					t.Fatalf("the outbox after the drain: %+v %v", st, err)
				}
			}
			noDeadLetters(t, second)
			exactlyOnce(t, f, tag, bodies)
			got, seqs := zlInbox(t, f, "b", tag)
			inOrder(t, "b", got)
			gapFree(t, "b", seqs)
		})
	}
}

// Case 5, two drainers on one profile: the drain lease lets one send at a
// time, so nothing is sent twice and the order holds.
func TestChaosZeroLossTwoDrainers(t *testing.T) {
	f, op := zlFixture(t, bus.QueueLimits{})
	blue := servingAPI(t, f, op, bus.QueueLimits{})
	p := newZLProxy(t, "")
	dir := t.TempDir()
	one := zlClient(t, p.url(), f.owner, dir)
	two := zlClient(t, p.url(), f.owner, dir)
	tag := awUnique("twice")
	bodies := queueWhileDown(t, one, f, tag, 30)
	p.point(blue.url())
	var wg sync.WaitGroup
	for _, c := range []*remote.Client{one, two} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
				c.DrainOutbox(context.Background(), 5*time.Second)
				if st, err := c.Outbox().Status(context.Background()); err == nil && st.Waiting == 0 {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	noDeadLetters(t, one)
	exactlyOnce(t, f, tag, bodies)
	got, seqs := zlInbox(t, f, "b", tag)
	inOrder(t, "b", got)
	gapFree(t, "b", seqs)
}

// zlAgent is a consumer as the MCP server is one (§1.8): its lease loop
// asks the outbox, through PromoteAck, whether an offered message's ack
// waits there, and then sends that ack instead of handing the message to
// the agent again.
type zlAgent struct {
	t        *testing.T
	f        *stFixture
	c        *remote.Client
	session  string
	instance string
	tag      string
	// handled counts the agent's runs, by message.
	handled map[string]int
}

func newZLAgent(t *testing.T, f *stFixture, c *remote.Client, session, tag string) *zlAgent {
	return &zlAgent{t: t, f: f, c: c, session: session, instance: session + "-inst", tag: tag, handled: map[string]int{}}
}

func (a *zlAgent) request(op string, args map[string]any) bus.Request {
	args["instance"] = a.instance
	return bus.Request{Operation: op, Workspace: a.f.workspace, Project: "p", Session: a.session, Args: args}
}

type zlOffer struct {
	ID, Lease string
	// Attempt is the lease's attempt, as the wire has it.
	Attempt string
}

func (a *zlAgent) lease() []zlOffer {
	a.t.Helper()
	res, err := a.c.Execute(context.Background(), a.request("lease", map[string]any{}))
	if err != nil {
		a.t.Fatal(err)
	}
	var out []zlOffer
	list, _ := res.Data.(map[string]any)["messages"].([]any)
	for _, raw := range list {
		m := raw.(map[string]any)
		out = append(out, zlOffer{ID: m["id"].(string), Lease: m["lease_id"].(string), Attempt: fmt.Sprint(m["attempt"])})
	}
	return out
}

// alreadyHandled is §1.8's check on an offer.
func (a *zlAgent) alreadyHandled(id string) bool {
	a.t.Helper()
	queued, pending, err := a.c.PromoteAck(context.Background(), a.request("ack", map[string]any{}), id)
	if err != nil || pending {
		a.t.Fatalf("the waiting ack of %s wasn't sent: %v", id, err)
	}
	return queued
}

// handle is the agent's run: its effect is a send whose replay is the same
// one.
func (a *zlAgent) handle(id string) {
	a.t.Helper()
	a.handled[id]++
	if _, err := a.c.Execute(context.Background(), a.request("send", map[string]any{"to": "sink", "body": a.tag + ":" + id, "operation_id": "msg:" + id + ":effect"})); err != nil {
		a.t.Fatal(err)
	}
}

func (a *zlAgent) ack(o zlOffer, within time.Duration) (bus.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	return a.c.Execute(ctx, a.request("ack", map[string]any{"acks": []any{map[string]any{"message_id": o.ID, "lease_id": o.Lease}}, "operation_id": "msg:" + o.ID + ":ack"}))
}

// zlDelivery is a delivery's state on the server: acked, dead, and the
// operation the ack was recorded with.
func zlDelivery(t *testing.T, f *stFixture, id string) (acked, dead bool, ackOp string) {
	t.Helper()
	var op sql.NullString
	if err := zlDB(t, f).QueryRow(`SELECT acked_at IS NOT NULL,dead_at IS NOT NULL,ack_operation_id FROM deliveries WHERE message_id=$1`, id).Scan(&acked, &dead, &op); err != nil {
		t.Fatal(err)
	}
	return acked, dead, op.String
}

// noDeadLetters checks nothing was refused: a write the server turned
// down would leave the outbox as a dead letter, not as a message.
func noDeadLetters(t *testing.T, clients ...*remote.Client) {
	t.Helper()
	for _, c := range clients {
		if dead := deadLetters(t, c); len(dead) != 0 {
			t.Fatal("dead letters:", dead)
		}
	}
}

// deadLetters is the outbox's dead letters as "operation error".
func deadLetters(t *testing.T, c *remote.Client) []string {
	t.Helper()
	items, err := c.Outbox().List(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range items {
		out = append(out, i.Operation+" "+i.Error)
	}
	return out
}

// reregister is the agent's restart: it resumes the session, which
// answers its new instance.
func reregister(f *stFixture, session string) string {
	out := f.do(f.principalOf(session), bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: session, Args: map[string]any{"role": "worker", "instance": session + "-inst", "resume": true}})
	return out.(map[string]any)["instance"].(string)
}

// handledOnce checks the message's effect happened once.
func handledOnce(t *testing.T, f *stFixture, tag, id string) {
	t.Helper()
	if got := effects(t, f, tag); got[id] != 1 {
		t.Fatalf("message %s's effect happened %d times", id, got[id])
	}
}

// Case 6, an ack queued past its lease (§1.8): the agent handled the
// message, its ack waits through a drain, and the lease runs out. The
// message is offered again; the consumer doesn't hand it to the agent but
// sends the waiting ack first; the delivery ends acked once, by that ack.
// Variants: the delivery died meanwhile, and the session re-registered.
func TestChaosZeroLossAckPastItsLease(t *testing.T) {
	for _, variant := range []string{"acked", "dead", "instance_changed"} {
		t.Run(variant, func(t *testing.T) {
			queue := bus.QueueLimits{Lease: 2 * time.Second}
			if variant == "dead" {
				queue.Attempts = 1
			}
			f, op := zlFixture(t, queue)
			api := servingAPI(t, f, op, queue)
			tag := awUnique("ackpast")
			c := zlClient(t, api.url(), f.owner, t.TempDir())
			agent := newZLAgent(t, f, c, "b", tag)
			id := f.send("b", tag+" work")
			offers := agent.lease()
			if len(offers) != 1 || offers[0].ID != id {
				t.Fatal(offers)
			}
			agent.handle(id)
			// The drainer below makes the ack wait the maintenance's
			// Retry-After, its TTL: for the offer again, longer than the
			// test, so only §1.8's promotion sends it in time.
			ttl := 5 * time.Second
			if variant == "acked" {
				ttl = 20 * time.Second
			}
			zlMaintenance(t, op, ttl)
			res, err := agent.ack(offers[0], 2*time.Second)
			queuedLocal(t, res, err)
			// The MCP server's drainer meets the maintenance: the ack waits
			// its Retry-After.
			if r, err := c.DrainOutbox(context.Background(), 5*time.Second); err != nil || r.Sent != 0 || r.Waiting != 1 {
				t.Fatalf("a drain in maintenance: %+v %v", r, err)
			}
			time.Sleep(2500 * time.Millisecond) // the lease runs out
			zlEnd(t, op, api)
			switch variant {
			case "acked":
				again := agent.lease()
				if len(again) != 1 || again[0].ID != id || again[0].Attempt != "2" {
					t.Fatalf("not offered again: %v", again)
				}
				if !agent.alreadyHandled(id) {
					t.Fatal("the consumer didn't find the waiting ack")
				}
				acked, _, ackOp := zlDelivery(t, f, id)
				if !acked || ackOp != "msg:"+id+":ack" {
					t.Fatalf("acked %v by %q", acked, ackOp)
				}
				if again := agent.lease(); len(again) != 0 {
					t.Fatalf("offered after its ack: %v", again)
				}
				if dead := deadLetters(t, c); len(dead) != 0 {
					t.Fatal(dead)
				}
			case "dead":
				// Its one lease ran out: the delivery is dead, and the ack
				// that lands on it is done, listed once.
				if again := agent.lease(); len(again) != 0 {
					t.Fatalf("a dead delivery offered: %v", again)
				}
				drainAll(t, 15*time.Second, c)
				if _, dead, _ := zlDelivery(t, f, id); !dead {
					t.Fatal("the delivery isn't dead")
				}
				if got := deadLetters(t, c); len(got) != 1 || got[0] != "ack "+outbox.ClassAckedAfterDead {
					t.Fatal(got)
				}
				if got := deadLetters(t, c); len(got) != 0 {
					t.Fatalf("listed twice: %v", got)
				}
				st, _ := c.Outbox().Status(context.Background())
				if st.Dead != 0 {
					t.Fatalf("doctor would count it: %+v", st)
				}
			case "instance_changed":
				instance := reregister(f, "b")
				drainAll(t, 15*time.Second, c)
				if got := deadLetters(t, c); len(got) != 1 || got[0] != "ack "+outbox.ClassInstance {
					t.Fatal(got)
				}
				// The new instance's own lease reaches the agent, whose
				// replayed effect is the same one.
				fresh := newZLAgent(t, f, c, "b", tag)
				fresh.instance = instance
				again := fresh.lease()
				if len(again) != 1 || again[0].ID != id {
					t.Fatalf("the new instance wasn't offered it: %v", again)
				}
				if fresh.alreadyHandled(id) {
					t.Fatal("another instance's ack was taken for this one's")
				}
				fresh.handle(id)
				if _, err := fresh.ack(again[0], 5*time.Second); err != nil {
					t.Fatal(err)
				}
				if acked, _, _ := zlDelivery(t, f, id); !acked {
					t.Fatal("not acked")
				}
			}
			handledOnce(t, f, tag, id)
			if variant != "instance_changed" && agent.handled[id] != 1 {
				t.Fatalf("the agent ran %d times", agent.handled[id])
			}
		})
	}
}

// Case 6b, a reply queued with its piggybacked ack (§1.9, D2'): the ack's
// lease runs out before the server is back, and the reply lands with the
// ack applied. When the acked message expired meanwhile, the ack is
// refused (not_found): a send that carried it is resent bare and lands,
// and only the ack becomes a dead letter, ack_refused:not_found; a reply
// to it is refused itself, since what it answers is gone, and is the dead
// letter. Across a re-registration, the reply and its ack are one dead
// letter, instance_changed, and the new instance is offered the message.
func TestChaosZeroLossReplyWithAck(t *testing.T) {
	for _, variant := range []string{"acked", "expired_send", "expired_reply", "instance_changed"} {
		t.Run(variant, func(t *testing.T) {
			queue := bus.QueueLimits{Lease: 2 * time.Second}
			f, op := zlFixture(t, queue)
			api := servingAPI(t, f, op, queue)
			tag := awUnique("replyack")
			c := zlClient(t, api.url(), f.owner, t.TempDir())
			agent := newZLAgent(t, f, c, "b", tag)
			id := f.send("b", tag+" question")
			offers := agent.lease()
			if len(offers) != 1 {
				t.Fatal(offers)
			}
			zlMaintenance(t, op, 5*time.Second)
			answer := tag + ":answer"
			write := agent.request("reply", map[string]any{"message_id": id, "body": answer, "operation_id": "op-" + answer, "ack": map[string]any{"message_id": id, "lease_id": offers[0].Lease}})
			if variant == "expired_send" {
				write = agent.request("send", map[string]any{"to": "a", "body": answer, "operation_id": "op-" + answer, "ack": map[string]any{"message_id": id, "lease_id": offers[0].Lease}})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			res, err := c.Execute(ctx, write)
			cancel()
			queuedLocal(t, res, err)
			time.Sleep(2500 * time.Millisecond) // the lease runs out
			if strings.HasPrefix(variant, "expired") {
				if _, err = zlDB(t, f).Exec(`UPDATE messages SET expires_at='2000'||substr(expires_at,5) WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			zlEnd(t, op, api)
			instance := ""
			if variant == "instance_changed" {
				instance = reregister(f, "b")
			}
			drainAll(t, 15*time.Second, c)
			answers := len(zlMessages(t, f, answer)[answer])
			acked, _, _ := zlDelivery(t, f, id)
			dead := deadLetters(t, c)
			switch variant {
			case "acked":
				if answers != 1 || !acked || len(dead) != 0 {
					t.Fatalf("replies %d, acked %v, dead letters %v", answers, acked, dead)
				}
			case "expired_send":
				if answers != 1 || len(dead) != 1 || dead[0] != "ack "+outbox.ClassAckRefused+":not_found" {
					t.Fatalf("sends %d, dead letters %v", answers, dead)
				}
			case "expired_reply":
				if answers != 0 || len(dead) != 1 || dead[0] != "reply not_found" {
					t.Fatalf("replies %d, dead letters %v", answers, dead)
				}
			case "instance_changed":
				if answers != 0 || len(dead) != 1 || dead[0] != "reply "+outbox.ClassInstance {
					t.Fatalf("replies %d, dead letters %v", answers, dead)
				}
				fresh := newZLAgent(t, f, c, "b", tag)
				fresh.instance = instance
				if again := fresh.lease(); len(again) != 1 || again[0].ID != id {
					t.Fatalf("the new instance wasn't offered it: %v", again)
				}
			}
		})
	}
}

// Case 7, the stream: a maintenance tells a subscriber to come back after
// retry_after, which it does without an error, and nothing is missed. A
// send replayed after its answer was lost, and the sends queued during
// the maintenance, leave the recipient's sequence without a gap or a
// repeat.
func TestChaosZeroLossStreamMaintenance(t *testing.T) {
	f, op := zlFixture(t, bus.QueueLimits{})
	api := servingAPI(t, f, op, bus.QueueLimits{})
	tag := awUnique("stream")
	streams := newZLProxy(t, api.url())
	consumer := newChaosConsumer(t, f, streams.url(), "b", f.owner, tag)
	consumer.start()
	p := newZLProxy(t, api.url())
	sender := zlClient(t, p.url(), f.owner, t.TempDir())
	body := func(i int) string { return fmt.Sprintf("m%s:a:%d", tag, i) }
	var bodies []string
	for i := range 10 {
		if _, err := zlSend(sender, f, "a", "b", body(i), "op-"+body(i), 5*time.Second); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body(i))
	}
	// The answer to the next send is lost once; its in-process retry is
	// the replay. The proxy may decide for two requests at once, so the
	// flag is taken atomically.
	var lose atomic.Bool
	lose.Store(true)
	p.decide(func(op string) zlFate {
		if op == "send" && lose.CompareAndSwap(true, false) {
			return zlHold
		}
		return zlPass
	})
	go func() {
		<-p.held
		p.decide(nil)
	}()
	p.free()
	if _, err := zlSend(sender, f, "a", "b", body(10), "op-"+body(10), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	bodies = append(bodies, body(10))
	if len(p.lostAnswers()) != 1 {
		t.Fatal("no answer was lost")
	}
	zlMaintenance(t, op, 4*time.Second)
	asked := streams.streamsAsked()
	for i := 11; i < 21; i++ {
		res, err := zlSend(sender, f, "a", "b", body(i), "op-"+body(i), 2*time.Second)
		queuedLocal(t, res, err)
		bodies = append(bodies, body(i))
	}
	// The maintenance ends by its TTL; the drained server serves again.
	within(t, 10*time.Second, "the maintenance never ended", func() bool { _, on := api.drain.Check(context.Background()); return !on })
	// The subscriber waited retry_after (4 s at most) between tries, rather
	// than asking again at once.
	if n := streams.streamsAsked() - asked; n > 3 {
		t.Fatalf("the stream was asked for %d times during a 4 s maintenance", n)
	}
	drainAll(t, 30*time.Second, sender)
	noDeadLetters(t, sender)
	exactlyOnce(t, f, "m"+tag, bodies)
	for deadline := time.Now().Add(30 * time.Second); ; {
		unacked, dead := queueCounts(f, "b", f.owner)
		if got := effects(t, f, tag); len(got) == len(bodies) && unacked == 0 && dead == 0 {
			break
		}
		if time.Now().After(deadline) {
			consumer.diagnose()
			t.Fatalf("not settled: %d of %d effects", len(effects(t, f, tag)), len(bodies))
		}
		time.Sleep(100 * time.Millisecond)
	}
	for id, n := range effects(t, f, tag) {
		if n != 1 {
			t.Fatalf("message %s handled %d times", id, n)
		}
	}
	got, seqs := zlInbox(t, f, "b", "m"+tag)
	inOrder(t, "b", got)
	gapFree(t, "b", seqs)
	if len(seqs) != len(bodies) {
		t.Fatalf("%d deliveries for %d messages", len(seqs), len(bodies))
	}
	deliveredInOrder(t, f, "b")
	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if consumer.ends != 0 || len(consumer.errs) != 0 {
		t.Fatalf("the stream ended %d times, errors %v", consumer.ends, consumer.errs)
	}
	if consumer.connects < 2 {
		t.Fatalf("the stream connected %d times: never told to come back", consumer.connects)
	}
}

// Case 8, the Linear write outbox (#236) across a swap: a write the server
// queued before the drain isn't sent by the draining server, and the new
// server, once its resume grace has passed, sends it exactly once.
func TestChaosZeroLossLinearOutboxAcrossASwap(t *testing.T) {
	op, dsn := drainSchemaStore(t)
	t.Setenv("CLAWBRIDGE_TEST_POSTGRES_DSN", dsn)
	f := newOutboxFixture(t)
	f.mcp.refuse = 1
	res, err := f.queueWrite("comment", map[string]any{"id": awIssue, "body": "across the swap", "operation_id": "swap-1"})
	if err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
		t.Fatal("not queued:", res, err)
	}
	ctx := context.Background()
	if _, err = op.StartMaintenance(ctx, "deploy", "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	blue := NewDrain(f.store, "clawbridge-server-blue", time.Now)
	if _, on := blue.Check(ctx); !on {
		t.Fatal("the old server didn't see the maintenance")
	}
	// The old server's outbox loop, past the write's retry time: nothing.
	f.writer.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	bounded, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	f.writer.runOutbox(bounded, 50*time.Millisecond, blue)
	cancel()
	if n := f.mcp.count("save_comment"); n != 0 || f.queued("swap-1").State != bus.OutboxQueued {
		t.Fatal("the draining server sent the queued write:", n, f.queued("swap-1").State)
	}
	// The new server starts: it ends the maintenance and waits its grace.
	var mu sync.Mutex
	clock := time.Now()
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	green := NewDrain(f.store, "clawbridge-server-green", now)
	clearAtStart(ctx, f.store, green)
	if _, paused := green.Paused(); !paused {
		t.Fatal("the new server didn't wait its resume grace")
	}
	if m, err := op.MaintenanceState(ctx); err != nil || m.Active {
		t.Fatal("the new server's start didn't end the maintenance:", m, err)
	}
	mu.Lock()
	clock = clock.Add(resumeGrace + time.Second)
	mu.Unlock()
	loop, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.writer.runOutbox(loop, 50*time.Millisecond, green)
	}()
	within(t, 10*time.Second, "the new server never sent the queued write", func() bool {
		return f.queued("swap-1").State == bus.OutboxDone
	})
	// A few more cycles send nothing again.
	time.Sleep(300 * time.Millisecond)
	stop()
	<-done
	f.writer.Now = nil
	if n := f.mcp.count("save_comment"); n != 1 {
		t.Fatalf("the queued write was sent %d times", n)
	}
}
