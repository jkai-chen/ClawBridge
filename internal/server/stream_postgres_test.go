package server

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// stFixture is a hosted workspace with sessions a and b of its owner and m
// of a project member, served with streams over a real Postgres.
type stFixture struct {
	t         *testing.T
	dsn       string
	store     *bus.Store
	hub       *StreamHub
	owner     string
	member    string
	workspace string
	host      *httptest.Server
	// expires is when the fixture's tokens run out; zero says nothing.
	expires time.Time
}

func newSTFixture(t *testing.T, times StreamLimits, limits bus.QueueLimits) *stFixture {
	t.Helper()
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	f := &stFixture{t: t, dsn: dsn, owner: awUnique("owner") + "@stream-test.example", member: awUnique("member") + "@stream-test.example", workspace: awUnique("stws")}
	store, err := bus.OpenPostgres(context.Background(), dsn, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.RequireVerifiedAccount()
	store.SetQueueLimits(limits)
	f.store = store
	f.do(f.owner, bus.Request{Operation: "workspace_init", Workspace: f.workspace, Args: map[string]any{"mode": "server"}})
	f.do(f.owner, bus.Request{Operation: "project_create", Workspace: f.workspace, Args: map[string]any{"slug": "p"}})
	f.do(f.owner, bus.Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": f.member, "role": "member"}})
	for _, name := range []string{"a", "b", "m"} {
		f.session(name, bus.Request{Operation: "register_session", Args: map[string]any{"role": "worker"}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.hub = NewStreamHub()
	go f.hub.Listen(ctx, dsn)
	verifier := VerifierFunc(func(_ context.Context, token string) (Identity, error) {
		return Identity{Principal: strings.TrimPrefix(token, "tok-"), Subject: "sub-" + strings.TrimPrefix(token, "tok-"), Expires: f.expires}, nil
	})
	f.host = httptest.NewServer(NewHandlerWithOptions(store, verifier, HandlerOptions{Streams: f.hub, StreamLimits: times}))
	t.Cleanup(f.host.Close)
	t.Cleanup(f.hub.Close)
	return f
}

func (f *stFixture) principalOf(session string) string {
	if session == "m" {
		return f.member
	}
	return f.owner
}

func (f *stFixture) exec(principal string, r bus.Request) (bus.Result, error) {
	return f.store.ExecuteAsIdentity(context.Background(), bus.Identity{Principal: principal, Issuer: bus.GoogleIssuer, Subject: "sub-" + principal}, r)
}

func (f *stFixture) do(principal string, r bus.Request) any {
	f.t.Helper()
	out, err := f.exec(principal, r)
	if err != nil {
		f.t.Fatalf("%s: %v", r.Operation, err)
	}
	return out.Data
}

// session runs r as session name at its instance.
func (f *stFixture) session(name string, r bus.Request) any {
	f.t.Helper()
	r.Workspace, r.Project, r.Session = f.workspace, "p", name
	if r.Args == nil {
		r.Args = map[string]any{}
	}
	r.Args["instance"] = name + "-inst"
	return f.do(f.principalOf(name), r)
}

func (f *stFixture) send(to, body string) string {
	f.t.Helper()
	return f.session("a", bus.Request{Operation: "send", Args: map[string]any{"to": to, "body": body, "operation_id": awUnique("op")}}).(map[string]any)["id"].(string)
}

func (f *stFixture) ticket(name, mode string) string {
	f.t.Helper()
	return f.session(name, bus.Request{Operation: "stream_ticket", Args: map[string]any{"mode": mode}}).(map[string]any)["ticket"].(string)
}

func (f *stFixture) url(name, mode string) string {
	return f.host.URL + StreamPath + "?" + url.Values{"workspace": {f.workspace}, "project": {"p"}, "session": {name}, "mode": {mode}}.Encode()
}

// get opens a stream request; ticket "" sends no ticket header.
func (f *stFixture) get(rawURL, principal, ticket string) *http.Response {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	req.Header.Set("Authorization", "Bearer tok-"+principal)
	if ticket != "" {
		req.Header.Set(StreamTicketHeader, ticket)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

// post runs r through the handler's /v1/execute.
func (f *stFixture) post(principal string, r bus.Request) *http.Response {
	f.t.Helper()
	body, _ := json.Marshal(r)
	req, _ := http.NewRequest(http.MethodPost, f.host.URL+"/v1/execute", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer tok-"+principal)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

type sseEvent struct {
	id, name string
	data     map[string]any
	at       time.Time
}

// sseReader reads a stream's events; pings come through as name "ping".
type sseReader struct {
	t      *testing.T
	events chan sseEvent
	body   interface{ Close() error }
}

func (f *stFixture) open(name, mode string) *sseReader {
	f.t.Helper()
	resp := f.get(f.url(name, mode), f.principalOf(name), f.ticket(name, mode))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		f.t.Fatal(resp.Status, resp.Header)
	}
	return readSSE(f.t, resp)
}

func readSSE(t *testing.T, resp *http.Response) *sseReader {
	r := &sseReader{t: t, events: make(chan sseEvent, 100), body: resp.Body}
	t.Cleanup(func() { resp.Body.Close() })
	go func() {
		defer close(r.events)
		scanner := bufio.NewScanner(resp.Body)
		var e sseEvent
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == ": ping":
				r.events <- sseEvent{name: "ping", at: time.Now()}
			case strings.HasPrefix(line, "id: "):
				e.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				e.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e.data)
			case line == "" && e.name != "":
				e.at = time.Now()
				r.events <- e
				e = sseEvent{}
			}
		}
	}()
	return r
}

// next is the next event other than a ping, within wait.
func (r *sseReader) next(wait time.Duration) (sseEvent, bool) {
	r.t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case e, ok := <-r.events:
			if !ok {
				return sseEvent{name: "closed"}, true
			}
			if e.name != "ping" {
				return e, true
			}
		case <-deadline:
			return sseEvent{}, false
		}
	}
}

func (r *sseReader) must(wait time.Duration, name string) sseEvent {
	r.t.Helper()
	e, ok := r.next(wait)
	if !ok || e.name != name {
		r.t.Fatalf("want %s, got %+v (%v)", name, e, ok)
	}
	return e
}

// quiet fails on any event other than a ping within wait.
func (r *sseReader) quiet(wait time.Duration) {
	r.t.Helper()
	if e, ok := r.next(wait); ok {
		r.t.Fatalf("unexpected event %+v", e)
	}
}

func messageID(e sseEvent) string {
	m, _ := e.data["message"].(map[string]any)
	id, _ := m["id"].(string)
	return id
}

func (f *stFixture) ack(name string, e sseEvent) {
	f.t.Helper()
	f.session(name, bus.Request{Operation: "ack", Args: map[string]any{"acks": []any{map[string]any{"message_id": messageID(e), "lease_id": e.data["lease_id"]}}}})
}

// Consume: what waited is offered at connect in order; a new message is
// pushed within a second of its commit; prefetch holds the rest until an
// ack; a second consume stream replaces the first and takes its leases
// over with their lease_until.
func TestPostgresStreamConsume(t *testing.T) {
	f := newSTFixture(t, StreamLimits{Reread: time.Hour}, bus.QueueLimits{Prefetch: 2})
	first, second := f.send("b", "one"), f.send("b", "two")
	f.send("b", "three")
	s := f.open("b", bus.StreamConsume)
	e1, e2 := s.must(5*time.Second, "message"), s.must(5*time.Second, "message")
	if messageID(e1) != first || messageID(e2) != second || e1.id != "1" || e2.data["recipient_seq"] != float64(2) || e1.data["attempt"] != float64(1) {
		t.Fatal(e1, e2)
	}
	if e1.data["message"].(map[string]any)["body"] != "one" || e1.data["message"].(map[string]any)["lease_id"] != nil {
		t.Fatal(e1.data)
	}
	// Prefetch 2: the third waits for an ack.
	s.quiet(300 * time.Millisecond)
	f.ack("b", e1)
	e3 := s.must(5*time.Second, "message")
	if e3.data["recipient_seq"] != float64(3) {
		t.Fatal(e3)
	}
	f.ack("b", e3)
	// Push latency: a message committed now arrives within a second, woken
	// by its own notification (the ack's wake has passed).
	s.quiet(300 * time.Millisecond)
	sent := time.Now()
	fourth := f.send("b", "four")
	e4 := s.must(5*time.Second, "message")
	if messageID(e4) != fourth || e4.at.Sub(sent) > time.Second {
		t.Fatal(messageID(e4), e4.at.Sub(sent))
	}
	// Takeover: e2 and e4 are held; the new stream gets them under new
	// lease IDs and the same lease_until, and the old one is replaced.
	s2 := f.open("b", bus.StreamConsume)
	s.must(5*time.Second, "replaced")
	t2, t4 := s2.must(5*time.Second, "message"), s2.must(5*time.Second, "message")
	if messageID(t2) != second || messageID(t4) != fourth || t2.data["lease_id"] == e2.data["lease_id"] || t2.data["lease_until"] != e2.data["lease_until"] || t2.data["attempt"] != float64(1) {
		t.Fatal(t2, e2)
	}
	if e, _ := s.next(time.Second); e.name != "closed" {
		t.Fatal("the replaced stream stayed open", e)
	}
	s2.quiet(300 * time.Millisecond)
}

// Observe: counts only, sent when they change.
func TestPostgresStreamObserve(t *testing.T) {
	f := newSTFixture(t, StreamLimits{Reread: time.Hour}, bus.QueueLimits{})
	s := f.open("b", bus.StreamObserve)
	if e := s.must(5*time.Second, "status"); e.data["unacked"] != float64(0) || e.data["latest_seq"] != float64(0) || e.data["dead"] != float64(0) {
		t.Fatal(e)
	}
	id := f.send("b", "hello")
	e := s.must(5*time.Second, "status")
	if e.data["unacked"] != float64(1) || e.data["latest_seq"] != float64(1) || len(e.data) != 3 {
		t.Fatal(e)
	}
	f.session("b", bus.Request{Operation: "mark_read", Args: map[string]any{"ids": []any{id}}})
	if e = s.must(5*time.Second, "status"); e.data["unacked"] != float64(0) || e.data["latest_seq"] != float64(1) {
		t.Fatal(e)
	}
	// Nothing changed for b: no event.
	f.send("a", "to a")
	s.quiet(300 * time.Millisecond)
}

// M3: the URL never carries a secret, a ticket works once, and only for the
// account, scope and mode it was issued for.
func TestPostgresStreamTickets(t *testing.T) {
	f := newSTFixture(t, StreamLimits{}, bus.QueueLimits{})
	for _, extra := range []string{"&instance=b-inst", "&ticket=x", "&other=1"} {
		resp := f.get(f.url("b", bus.StreamConsume)+extra, f.owner, f.ticket("b", bus.StreamConsume))
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatal(extra, resp.Status)
		}
	}
	if resp := f.get(f.url("b", bus.StreamConsume), f.owner, ""); resp.StatusCode != 400 {
		t.Fatal("no ticket", resp.Status)
	}
	if resp := f.get(f.url("b", bus.StreamConsume), f.owner, "stk_unknown"); resp.StatusCode != 403 {
		t.Fatal("unknown ticket", resp.Status)
	}
	ticket := f.ticket("b", bus.StreamObserve)
	cases := []struct {
		name, url, principal string
	}{
		{"another mode", f.url("b", bus.StreamConsume), f.owner},
		{"another session", f.url("a", bus.StreamObserve), f.owner},
		{"another account", f.url("b", bus.StreamObserve), f.member},
	}
	for _, c := range cases {
		// A refused redemption uses the ticket up too.
		ticket := f.ticket("b", bus.StreamObserve)
		if resp := f.get(c.url, c.principal, ticket); resp.StatusCode != 403 {
			t.Fatal(c.name, resp.Status)
		}
		if resp := f.get(f.url("b", bus.StreamObserve), f.owner, ticket); resp.StatusCode != 403 {
			t.Fatal(c.name, "reused", resp.Status)
		}
	}
	resp := f.get(f.url("b", bus.StreamObserve), f.owner, ticket)
	readSSE(t, resp).must(5*time.Second, "status")
	if again := f.get(f.url("b", bus.StreamObserve), f.owner, ticket); again.StatusCode != 403 {
		t.Fatal("used ticket", again.Status)
	}
	// A ticket needs the session's instance.
	if _, err := f.exec(f.owner, bus.Request{Operation: "stream_ticket", Workspace: f.workspace, Project: "p", Session: "b", Args: map[string]any{"instance": "wrong", "mode": "observe"}}); err == nil {
		t.Fatal("a ticket without the instance")
	}
	// An expired ticket.
	expired := f.ticket("b", bus.StreamObserve)
	expireTickets(t, f)
	if resp := f.get(f.url("b", bus.StreamObserve), f.owner, expired); resp.StatusCode != 403 {
		t.Fatal("expired ticket", resp.Status)
	}
}

func sqlOpen(dsn string) (*sql.DB, error) { return sql.Open("pgx", dsn) }

// expireTickets ages every stream ticket past its minute.
func expireTickets(t *testing.T, f *stFixture) {
	t.Helper()
	db, err := sqlOpen(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE stream_tickets SET expires_at='2000-01-01T00:00:00.000000000Z'`); err != nil {
		t.Fatal(err)
	}
}

// M4: a member removed mid-stream is refused at the stream's next read and
// the stream closes with revoked, before anything more is leased; a
// re-register, which moves the instance, closes the old instance's stream
// the same way.
func TestPostgresStreamRevoked(t *testing.T) {
	f := newSTFixture(t, StreamLimits{Reread: time.Hour}, bus.QueueLimits{})
	s := f.open("m", bus.StreamConsume)
	f.send("m", "before")
	s.must(5*time.Second, "message")
	// Through the handler, which wakes every stream on a revoke.
	resp := f.post(f.owner, bus.Request{Operation: "project_role_revoke", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": f.member}})
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	s.must(5*time.Second, "revoked")
	if e, _ := s.next(time.Second); e.name != "closed" {
		t.Fatal(e)
	}
	var leased int
	db, err := sqlOpen(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.QueryRow(`SELECT COUNT(*) FROM deliveries d JOIN sessions s ON s.id=d.recipient_id WHERE s.owner_principal=$1 AND d.lease_id IS NOT NULL`, f.member).Scan(&leased); err != nil || leased != 1 {
		t.Fatal(leased, err)
	}
	b := f.open("b", bus.StreamObserve)
	b.must(5*time.Second, "status")
	f.do(f.owner, bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "b", Args: map[string]any{"instance": "b-inst", "resume": true, "role": "worker"}})
	f.send("b", "after the move")
	b.must(5*time.Second, "revoked")
}

// G9 S5-03: a stream opened by a device-key-signed client (a signed GET plus
// a stream ticket, no Bearer token) closes with revoked when that device is
// revoked through the handler, which wakes every stream; and a new signed
// request from the device is then refused.
func TestPostgresDeviceStreamRevoked(t *testing.T) {
	joinFloor = time.Millisecond
	t.Cleanup(func() { joinFloor = 250 * time.Millisecond })
	f := newSTFixture(t, StreamLimits{Reread: time.Hour}, bus.QueueLimits{})
	ada := awUnique("ada") + "@stream-test.example"
	invite := f.do(f.owner, bus.Request{Operation: "invite_create", Workspace: f.workspace, Args: map[string]any{"email": ada, "grants": map[string]any{"p": "member"}}})
	d := newPGDeviceClient(t, f.store)
	d.handler = NewHandlerWithOptions(f.store, nil, HandlerOptions{Streams: f.hub, Auth: AuthOptions{Methods: []string{AuthDevice}, PublicOrigin: testOrigin, Devices: f.store}, Now: time.Now})
	host := httptest.NewServer(d.handler)
	t.Cleanup(host.Close)
	if w := d.postJoin(d.joinBody(ada, invite.(map[string]any)["code"].(string), d.now.Unix(), d.priv), "198.51.100.21"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Every request is signed over the origin the device was told, and sent
	// to the test host.
	call := func(method, target, body string, header ...string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, host.URL+target, strings.NewReader(body))
		req.Header.Set("Authorization", d.header(signed{method: method, target: target, body: body}))
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	execute := func(r bus.Request) *http.Response {
		t.Helper()
		body, _ := json.Marshal(r)
		return call(http.MethodPost, "/v1/execute", string(body))
	}
	reg := execute(bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "d", Args: map[string]any{"instance": "d-inst", "role": "worker"}})
	reg.Body.Close()
	if reg.StatusCode != 200 {
		t.Fatal(reg.Status)
	}
	tk := execute(bus.Request{Operation: "stream_ticket", Workspace: f.workspace, Project: "p", Session: "d", Args: map[string]any{"instance": "d-inst", "mode": "observe"}})
	var ticket struct{ Data struct{ Ticket string } }
	if err := json.NewDecoder(tk.Body).Decode(&ticket); err != nil || tk.StatusCode != 200 || ticket.Data.Ticket == "" {
		t.Fatal(tk.Status, err)
	}
	tk.Body.Close()
	target := StreamPath + "?" + url.Values{"workspace": {f.workspace}, "project": {"p"}, "session": {"d"}, "mode": {"observe"}}.Encode()
	resp := call(http.MethodGet, target, "", StreamTicketHeader, ticket.Data.Ticket)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.Status)
	}
	s := readSSE(t, resp)
	s.must(5*time.Second, "status")
	// The device revokes itself through the handler.
	rev := execute(bus.Request{Operation: "device_revoke", Workspace: f.workspace, Args: map[string]any{"device_id": d.id}})
	rev.Body.Close()
	if rev.StatusCode != 200 {
		t.Fatal(rev.Status)
	}
	s.must(5*time.Second, "revoked")
	if e, _ := s.next(2 * time.Second); e.name != "closed" {
		t.Fatal(e)
	}
	again := execute(bus.Request{Operation: "whoami", Workspace: f.workspace, Project: "p"})
	again.Body.Close()
	if again.StatusCode != 401 {
		t.Fatal("a revoked device signed in:", again.Status)
	}
}

// The stream's intervals: pings keep it open past the server's read and
// write timeouts, and it says bye at its lifetime.
func TestPostgresStreamLifetime(t *testing.T) {
	f := newSTFixture(t, StreamLimits{Ping: 100 * time.Millisecond, Lifetime: 1500 * time.Millisecond}, bus.QueueLimits{})
	f.host.Close()
	verifier := VerifierFunc(func(_ context.Context, token string) (Identity, error) {
		return Identity{Principal: strings.TrimPrefix(token, "tok-"), Subject: "sub-" + strings.TrimPrefix(token, "tok-")}, nil
	})
	f.host = httptest.NewUnstartedServer(NewHandlerWithOptions(f.store, verifier, HandlerOptions{Streams: f.hub, StreamLimits: StreamLimits{Ping: 100 * time.Millisecond, Lifetime: 1500 * time.Millisecond}}))
	f.host.Config.ReadTimeout, f.host.Config.WriteTimeout = 300*time.Millisecond, 300*time.Millisecond
	f.host.Start()
	t.Cleanup(f.host.Close)
	start := time.Now()
	s := f.open("b", bus.StreamObserve)
	s.must(5*time.Second, "status")
	pings := 0
	for {
		e, ok := <-s.events
		if !ok {
			t.Fatal("closed without bye")
		}
		if e.name == "ping" {
			pings++
			continue
		}
		if e.name != "bye" || time.Since(start) < 1400*time.Millisecond || pings < 5 {
			t.Fatal(e, time.Since(start), pings)
		}
		break
	}
}

// The sweeper offers a run-out lease again and wakes the stream, with the
// attempt counted.
func TestPostgresStreamSweeper(t *testing.T) {
	f := newSTFixture(t, StreamLimits{Reread: time.Hour}, bus.QueueLimits{Lease: 300 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runSweeper(ctx, f.store, 100*time.Millisecond, nil, nil)
	id := f.send("b", "slow")
	s := f.open("b", bus.StreamConsume)
	if e := s.must(5*time.Second, "message"); messageID(e) != id || e.data["attempt"] != float64(1) {
		t.Fatal(e)
	}
	if e := s.must(5*time.Second, "message"); messageID(e) != id || e.data["attempt"] != float64(2) {
		t.Fatal(e)
	}
}

// A dropped LISTEN connection is reconnected, and the reconnect wakes every
// stream, so what was sent while it was down still arrives without the
// periodic re-read.
func TestPostgresStreamListenerReconnects(t *testing.T) {
	f := newSTFixture(t, StreamLimits{Reread: time.Hour}, bus.QueueLimits{})
	s := f.open("b", bus.StreamConsume)
	f.send("b", "before")
	s.must(5*time.Second, "message")
	db, err := sqlOpen(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'LISTEN%' AND pid<>pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	id := f.send("b", "while down")
	if e := s.must(10*time.Second, "message"); messageID(e) != id {
		t.Fatal(e)
	}
}

// Through the real Caddy reverse proxy (deploy/compose.yaml's `caddy
// reverse-proxy`): events arrive unbuffered, and with the production
// intervals and server timeouts the pings keep a stream open past a minute
// and past the server's write timeout. It needs a Caddy that forwards
// CLAWBRIDGE_TEST_CADDY_URL to CLAWBRIDGE_TEST_CADDY_UPSTREAM, e.g.
//
//	docker run --rm -p 127.0.0.1:18088:8088 caddy:2.10-alpine caddy reverse-proxy --from :8088 --to host.docker.internal:18089
func TestPostgresStreamThroughCaddy(t *testing.T) {
	upstream, front := os.Getenv("CLAWBRIDGE_TEST_CADDY_UPSTREAM"), os.Getenv("CLAWBRIDGE_TEST_CADDY_URL")
	if upstream == "" || front == "" {
		t.Skip("needs a Caddy reverse proxy (CLAWBRIDGE_TEST_CADDY_UPSTREAM, CLAWBRIDGE_TEST_CADDY_URL)")
	}
	f := newSTFixture(t, StreamLimits{}, bus.QueueLimits{})
	verifier := VerifierFunc(func(_ context.Context, token string) (Identity, error) {
		return Identity{Principal: strings.TrimPrefix(token, "tok-"), Subject: "sub-" + strings.TrimPrefix(token, "tok-")}, nil
	})
	// Run's server, timeouts included.
	srv := &http.Server{Addr: upstream, Handler: NewHandlerWithOptions(f.store, verifier, HandlerOptions{Streams: f.hub}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 80 * time.Second, IdleTimeout: 60 * time.Second}
	go srv.ListenAndServe()
	t.Cleanup(func() { srv.Close() })
	time.Sleep(200 * time.Millisecond)
	f.host.URL = strings.TrimSuffix(front, "/")
	s := f.open("b", bus.StreamConsume)
	f.send("b", "first")
	s.must(5*time.Second, "message")
	start, pings := time.Now(), 0
	for time.Since(start) < 90*time.Second {
		e, ok := <-s.events
		if !ok {
			t.Fatal("closed after", time.Since(start))
		}
		if e.name != "ping" {
			t.Fatal(e)
		}
		pings++
	}
	sent := time.Now()
	f.send("b", "late")
	if e := s.must(5*time.Second, "message"); e.at.Sub(sent) > time.Second || pings < 3 {
		t.Fatal(e.at.Sub(sent), pings)
	}
}

// A Google token proves the caller only until it expires: the stream says
// bye then, and the client reconnects with a fresh token.
func TestPostgresStreamEndsWithToken(t *testing.T) {
	f := newSTFixture(t, StreamLimits{}, bus.QueueLimits{})
	f.expires = time.Now().Add(time.Second)
	s := f.open("b", bus.StreamObserve)
	s.must(5*time.Second, "status")
	if e := s.must(5*time.Second, "bye"); time.Until(f.expires) > 100*time.Millisecond {
		t.Fatal("bye before the token expired", e)
	}
}

// A client that connects and never reads can't hold its stream: once the
// socket fills, a write outlasts WriteWait and the stream ends, its count
// and subscription released, and its leases run out and are offered again.
func TestPostgresStreamStalledClient(t *testing.T) {
	f := newSTFixture(t, StreamLimits{WriteWait: 300 * time.Millisecond, Reread: time.Hour}, bus.QueueLimits{Prefetch: 400, Lease: 5 * time.Second})
	// 24 MB, more than the sockets buffer; four senders stay under the
	// per-session send rate.
	body := strings.Repeat("x", 60000)
	for i := range 4 {
		sender := fmt.Sprintf("s%d", i)
		f.session(sender, bus.Request{Operation: "register_session", Args: map[string]any{"role": "worker"}})
		for range 100 {
			f.session(sender, bus.Request{Operation: "send", Args: map[string]any{"to": "b", "body": body, "operation_id": awUnique("op")}})
		}
	}
	u, _ := url.Parse(f.url("b", bus.StreamConsume))
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer tok-%s\r\n%s: %s\r\n\r\n", u.RequestURI(), u.Host, f.owner, StreamTicketHeader, f.ticket("b", bus.StreamConsume))
	// Polled, not timed, so a loaded machine only makes it slower. The
	// stream opened (it leased all 400) and was closed for its stalled
	// writes: the hub holds nothing.
	db, err := sqlOpen(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count := func(where string) int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM deliveries d JOIN sessions s ON s.id=d.recipient_id AND s.project_id=d.project_id JOIN projects p ON p.id=s.project_id JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND s.name='b' AND `+where, f.workspace).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		f.hub.mu.Lock()
		subs, open := len(f.hub.subs), f.hub.open[f.owner]
		f.hub.mu.Unlock()
		if count(`(d.lease_id IS NOT NULL OR d.attempts>=1)`) == 400 && subs == 0 && open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stalled stream is still open", subs, open)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Its leases run out and are offered again with the attempt counted.
	// The sweep is global, so another package's sweeper may get there first:
	// count the deliveries, not this sweep's result.
	for deadline = time.Now().Add(60 * time.Second); count(`d.lease_id IS NULL AND d.attempts=1`) < 400; {
		if _, err := f.store.SweepDeliveries(context.Background()); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the stalled stream's leases weren't offered again", count(`d.lease_id IS NULL AND d.attempts=1`))
		}
		time.Sleep(200 * time.Millisecond)
	}
	s := f.open("b", bus.StreamConsume)
	if e := s.must(5*time.Second, "message"); e.data["attempt"] != float64(2) {
		t.Fatal(e.data["attempt"])
	}
}

// §8: one principal holds at most PerPrincipal streams on a server; the next
// is refused with 429 and its ticket stays usable once one closes.
func TestPostgresStreamPerPrincipal(t *testing.T) {
	f := newSTFixture(t, StreamLimits{PerPrincipal: 2, Ping: 50 * time.Millisecond}, bus.QueueLimits{})
	first := f.open("a", bus.StreamObserve)
	f.open("b", bus.StreamObserve)
	ticket := f.ticket("b", bus.StreamObserve)
	if resp := f.get(f.url("b", bus.StreamObserve), f.owner, ticket); resp.StatusCode != 429 {
		t.Fatal(resp.Status)
	}
	// Another principal has its own count.
	f.open("m", bus.StreamObserve)
	first.body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp := f.get(f.url("b", bus.StreamObserve), f.owner, ticket)
		if resp.StatusCode == 200 {
			readSSE(t, resp).must(5*time.Second, "status")
			break
		}
		resp.Body.Close()
		if resp.StatusCode != 429 || time.Now().After(deadline) {
			t.Fatal(resp.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
