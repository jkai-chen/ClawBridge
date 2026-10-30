package linearapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"clawbridge/internal/linearpace"
)

const (
	poolTeam   = "c0000000-0000-4000-8000-0000000000c1"
	poolFailed = "c0000000-0000-4000-8000-0000000000c2"
)

// mcpFake is a real go-sdk MCP server that counts what reaches it: each
// JSON-RPC method, the DELETEs that close a session, and get_team calls.
// It can forget the sessions open so far (answering 404, as Linear does
// for a session it no longer knows) and answer the next calls 401.
type mcpFake struct {
	mu      sync.Mutex
	methods map[string]int
	deletes int
	known   map[string]bool
	gone    map[string]bool
	refuse  int
}

func (m *mcpFake) count(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.methods[method]
}

func (m *mcpFake) closed() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deletes
}

// forget makes every session opened so far unknown to the server.
func (m *mcpFake) forget() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.known {
		m.gone[id] = true
	}
}

type mcpLocal struct {
	to   *url.URL
	base http.RoundTripper
}

func (r mcpLocal) RoundTrip(q *http.Request) (*http.Response, error) {
	x := q.Clone(q.Context())
	x.URL.Scheme, x.URL.Host, x.Host = r.to.Scheme, r.to.Host, r.to.Host
	return r.base.RoundTrip(x)
}

func newMCPFake(t *testing.T) (*mcpFake, http.RoundTripper) {
	t.Helper()
	m := &mcpFake{methods: map[string]int{}, known: map[string]bool{}, gone: map[string]bool{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "synthetic-linear", Version: "1"}, nil)
	srv.AddTool(&mcp.Tool{Name: "get_team", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []any{"query"}}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct{ Query string }
		_ = json.Unmarshal(req.Params.Arguments, &a)
		raw, _ := json.Marshal(map[string]any{"id": a.Query, "name": "team"})
		return &mcp.CallToolResult{IsError: a.Query == poolFailed, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	h := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct{ Method string }
		_ = json.Unmarshal(body, &msg)
		id := r.Header.Get("Mcp-Session-Id")
		m.mu.Lock()
		if r.Method == http.MethodDelete {
			m.deletes++
		} else {
			m.methods[msg.Method]++
		}
		gone, refused := m.gone[id], m.refuse > 0 && msg.Method == "tools/call"
		if refused {
			m.refuse--
		}
		m.mu.Unlock()
		switch {
		case gone:
			http.Error(w, "session not found", http.StatusNotFound)
			return
		case refused:
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
		if sid := w.Header().Get("Mcp-Session-Id"); sid != "" {
			m.mu.Lock()
			m.known[sid] = true
			m.mu.Unlock()
		}
	}))
	t.Cleanup(h.Close)
	u, _ := url.Parse(h.URL)
	return m, mcpLocal{u, h.Client().Transport}
}

var poolClients atomic.Int64

// poolApp is an app against the fakes, with its own pacer (a client ID of
// its own), a clock the test moves, and the reaper's timer captured.
type poolApp struct {
	app    *App
	mcp    *mcpFake
	now    *time.Time
	reaper func()
}

func newPoolApp(t *testing.T) *poolApp {
	t.Helper()
	m, transport := newMCPFake(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	pa := &poolApp{mcp: m, now: &now}
	a, err := New(Config{ClientID: fmt.Sprintf("pool-client-%d", poolClients.Add(1)), Secret: secret, Key: key, Store: &memStore{live: map[string]int{}}, HTTP: &http.Client{Transport: &linear{app: true}}, MCPTransport: transport, Now: func() time.Time { return *pa.now }})
	if err != nil || a == nil {
		t.Fatal(a, err)
	}
	pa.app = a
	// The token is minted first, so a write's count is MCP calls alone.
	if _, _, err = a.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := a.WriteSessions()
	s.after = func(_ time.Duration, f func()) func() bool {
		pa.reaper = f
		return func() bool { pa.reaper = nil; return true }
	}
	t.Cleanup(s.Close)
	return pa
}

// write runs one app write's MCP use: a lease, one get_team per team, the
// board's close, and the release, with the calls counted in calls.
func (pa *poolApp) write(t *testing.T, calls *atomic.Int64, ok bool, teams ...string) error {
	t.Helper()
	ctx := linearpace.WithCounter(context.Background(), calls)
	l := pa.app.WriteSessions().Lease()
	p, err := l.Provider(ctx, false)
	if err != nil {
		l.Release(false)
		return err
	}
	for _, team := range teams {
		if _, err = p.GetTeam(ctx, team); err != nil {
			break
		}
	}
	p.Close()
	l.Release(ok && err == nil)
	return err
}

func (pa *poolApp) kept() int {
	s := pa.app.WriteSessions()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.free)
}

// Two writes within the idle time share one session: one handshake, one
// tools/list, and the second write's calls are counted under its own
// counter, four fewer than the first's.
func TestSessionsReuseOneSessionWithinTheIdleTime(t *testing.T) {
	pa := newPoolApp(t)
	var first, second atomic.Int64
	if err := pa.write(t, &first, true, poolTeam); err != nil {
		t.Fatal(err)
	}
	*pa.now = pa.now.Add(SessionIdle - time.Second)
	if err := pa.write(t, &second, true, poolTeam); err != nil {
		t.Fatal(err)
	}
	if pa.mcp.count("initialize") != 1 || pa.mcp.count("tools/list") != 1 || pa.mcp.count("tools/call") != 2 {
		t.Fatal("the second write opened a session", pa.mcp.methods)
	}
	if second.Load() != 1 || first.Load() != 5 {
		t.Fatal("calls counted", first.Load(), second.Load(), pa.mcp.methods)
	}
	if pa.mcp.closed() != 0 || pa.kept() != 1 {
		t.Fatal("the session was not kept", pa.mcp.closed(), pa.kept())
	}
}

// The reaper closes a session idle for the idle time; the next write opens
// a new one. Shutdown closes what is kept.
func TestSessionsReaperAndShutdownClose(t *testing.T) {
	pa := newPoolApp(t)
	var calls atomic.Int64
	if err := pa.write(t, &calls, true, poolTeam); err != nil {
		t.Fatal(err)
	}
	if pa.reaper == nil {
		t.Fatal("no reaper was scheduled for a kept session")
	}
	*pa.now = pa.now.Add(SessionIdle + time.Second)
	reap := pa.reaper
	pa.reaper = nil
	reap()
	if pa.mcp.closed() != 1 || pa.kept() != 0 {
		t.Fatal("the reaper left an idle session open", pa.mcp.closed(), pa.kept())
	}
	if pa.reaper != nil {
		t.Fatal("the reaper runs again with nothing kept")
	}
	if err := pa.write(t, &calls, true, poolTeam); err != nil || pa.mcp.count("initialize") != 2 {
		t.Fatal("the write after the reaper did not reopen", err, pa.mcp.count("initialize"))
	}
	pa.app.WriteSessions().Close()
	if pa.mcp.closed() != 2 || pa.kept() != 0 || pa.reaper != nil {
		t.Fatal("shutdown left a session open", pa.mcp.closed(), pa.kept())
	}
	// After shutdown a write still runs, and keeps nothing.
	if err := pa.write(t, &calls, true, poolTeam); err != nil || pa.kept() != 0 || pa.mcp.closed() != 3 {
		t.Fatal("a write after shutdown kept its session", err, pa.kept(), pa.mcp.closed())
	}
}

// A session idle past the idle time is never handed out, even when the
// reaper has not run yet.
func TestSessionsNeverHandOutAnExpiredSession(t *testing.T) {
	pa := newPoolApp(t)
	var calls atomic.Int64
	if err := pa.write(t, &calls, true, poolTeam); err != nil {
		t.Fatal(err)
	}
	*pa.now = pa.now.Add(SessionIdle)
	if err := pa.write(t, &calls, true, poolTeam); err != nil || pa.mcp.count("initialize") != 2 || pa.mcp.closed() != 1 {
		t.Fatal("an expired session was reused", err, pa.mcp.count("initialize"), pa.mcp.closed())
	}
}

// A failed call, a 401 and a failed write each close the session, so the
// next write opens a new one.
func TestSessionsDropAfterAFailure(t *testing.T) {
	cases := map[string]func(pa *poolApp, calls *atomic.Int64) error{
		"failed call": func(pa *poolApp, calls *atomic.Int64) error {
			return pa.write(t, calls, true, poolFailed)
		},
		"401": func(pa *poolApp, calls *atomic.Int64) error {
			pa.mcp.refuse = 1
			return pa.write(t, calls, true, poolTeam)
		},
		"failed write": func(pa *poolApp, calls *atomic.Int64) error {
			if err := pa.write(t, calls, false, poolTeam); err != nil {
				return err
			}
			return errFailedWrite
		},
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			pa := newPoolApp(t)
			var calls atomic.Int64
			// A minute on, a 401 may re-mint (mintPace).
			*pa.now = pa.now.Add(2 * time.Minute)
			if err := fail(pa, &calls); err == nil {
				t.Fatal("the failure did not fail")
			}
			if pa.kept() != 0 || pa.mcp.closed() != 1 {
				t.Fatal("a failed session was kept", pa.kept(), pa.mcp.closed())
			}
			if err := pa.write(t, &calls, true, poolTeam); err != nil || pa.mcp.count("initialize") != 2 {
				t.Fatal("the next write did not reopen", err, pa.mcp.count("initialize"))
			}
		})
	}
}

var errFailedWrite = &Error{Code: "failed_write"}

// A re-minted token never reuses the session the old one opened, even
// within the idle time.
func TestSessionsNeverReuseAcrossATokenGeneration(t *testing.T) {
	pa := newPoolApp(t)
	var calls atomic.Int64
	if err := pa.write(t, &calls, true, poolTeam); err != nil {
		t.Fatal(err)
	}
	old, _, _ := pa.app.Token(context.Background())
	*pa.now = pa.now.Add(61 * time.Second)
	if fresh, _, err := pa.app.Remint(context.Background(), old); err != nil || fresh == old {
		t.Fatal("no new token", err)
	}
	if err := pa.write(t, &calls, true, poolTeam); err != nil {
		t.Fatal(err)
	}
	if pa.mcp.count("initialize") != 2 || pa.mcp.closed() != 1 || pa.kept() != 1 {
		t.Fatal("the old token's session was reused", pa.mcp.count("initialize"), pa.mcp.closed(), pa.kept())
	}
}

// A kept session Linear no longer knows is reopened once inside the write,
// which succeeds; the reopen's calls are this write's.
func TestSessionsReopenAStaleSessionOnce(t *testing.T) {
	pa := newPoolApp(t)
	var first, second atomic.Int64
	if err := pa.write(t, &first, true, poolTeam); err != nil {
		t.Fatal(err)
	}
	pa.mcp.forget()
	if err := pa.write(t, &second, true, poolTeam); err != nil {
		t.Fatal("the stale session was not reopened", err)
	}
	// The refused call, the new session's handshake, and the call again.
	if pa.mcp.count("initialize") != 2 || second.Load() != first.Load()+1 {
		t.Fatal("reopen", pa.mcp.count("initialize"), first.Load(), second.Load(), pa.mcp.methods)
	}
	if pa.kept() != 1 {
		t.Fatal("the reopened session was not kept", pa.kept())
	}
	var third atomic.Int64
	if err := pa.write(t, &third, true, poolTeam); err != nil || third.Load() != 1 || pa.mcp.count("initialize") != 2 {
		t.Fatal("the reopened session was not reused", err, third.Load())
	}
}

// Concurrent writes never share a session: each holds its own until it is
// released, and both are kept for the writes after.
func TestSessionsConcurrentWritesTakeTheirOwn(t *testing.T) {
	pa := newPoolApp(t)
	ctx := context.Background()
	s := pa.app.WriteSessions()
	run := func() {
		l1, l2 := s.Lease(), s.Lease()
		p1, err1 := l1.Provider(ctx, false)
		p2, err2 := l2.Provider(ctx, false)
		if err1 != nil || err2 != nil || p1.Provider == p2.Provider {
			t.Fatal("two writes share one session", err1, err2)
		}
		var wg sync.WaitGroup
		for _, l := range []struct {
			lease *Lease
			p     *Leased
		}{{l1, p1}, {l2, p2}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := l.p.GetTeam(ctx, poolTeam)
				l.p.Close()
				l.lease.Release(err == nil)
			}()
		}
		wg.Wait()
	}
	run()
	if pa.kept() != 2 || pa.mcp.count("initialize") != 2 {
		t.Fatal("concurrent sessions", pa.kept(), pa.mcp.count("initialize"))
	}
	run()
	if pa.kept() != 2 || pa.mcp.count("initialize") != 2 {
		t.Fatal("the kept sessions were not reused", pa.kept(), pa.mcp.count("initialize"))
	}
}

// Within one write, a session the board closed is taken again by its next
// open, and only one write's sessions are ever in its lease.
func TestSessionsOneWriteReopensItsOwnSession(t *testing.T) {
	pa := newPoolApp(t)
	ctx := context.Background()
	l := pa.app.WriteSessions().Lease()
	p1, _ := l.Provider(ctx, false)
	p1.Close()
	p2, _ := l.Provider(ctx, false)
	if p1.Provider != p2.Provider || pa.mcp.count("initialize") != 1 {
		t.Fatal("the write opened a second session")
	}
	// Not closed by the board: it is not kept.
	l.Release(true)
	if pa.kept() != 0 || pa.mcp.closed() != 1 {
		t.Fatal("a session still in use was kept", pa.kept())
	}
}

// A token re-minted while a write holds its session (a 401 on another
// call, say) closes that session at the write's end instead of keeping it.
func TestSessionsCloseASessionWhoseTokenChangedDuringTheWrite(t *testing.T) {
	pa := newPoolApp(t)
	ctx := context.Background()
	l := pa.app.WriteSessions().Lease()
	p, err := l.Provider(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.GetTeam(ctx, poolTeam); err != nil {
		t.Fatal(err)
	}
	p.Close()
	old, _, _ := pa.app.Token(ctx)
	*pa.now = pa.now.Add(61 * time.Second)
	if _, _, err = pa.app.Remint(ctx, old); err != nil {
		t.Fatal(err)
	}
	l.Release(true)
	if pa.kept() != 0 || pa.mcp.closed() != 1 {
		t.Fatal("a session of the old token was kept", pa.kept(), pa.mcp.closed())
	}
}

// A write that ends well but saw one of its calls fail (a read that
// failed, a missing capability) closes its session instead of keeping it:
// the next write opens a new one.
func TestSessionsCloseASessionThatSawAFailedCall(t *testing.T) {
	pa := newPoolApp(t)
	ctx := context.Background()
	l := pa.app.WriteSessions().Lease()
	p, err := l.Provider(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.GetTeam(ctx, poolFailed); err == nil {
		t.Fatal("the failing call did not fail")
	}
	if _, err = p.GetTeam(ctx, poolTeam); err != nil {
		t.Fatal(err)
	}
	p.Close()
	l.Release(true)
	if pa.kept() != 0 || pa.mcp.closed() != 1 {
		t.Fatal("a session that saw a failed call was kept", pa.kept(), pa.mcp.closed())
	}
	var calls atomic.Int64
	if err = pa.write(t, &calls, true, poolTeam); err != nil || pa.mcp.count("initialize") != 2 {
		t.Fatal("the next write did not reopen", err, pa.mcp.count("initialize"))
	}
}

// At most maxIdleSessions are kept between writes: five writes held at
// once keep four when they end and close the fifth.
func TestSessionsKeepAtMostTheIdleCap(t *testing.T) {
	pa := newPoolApp(t)
	ctx := context.Background()
	s := pa.app.WriteSessions()
	var leases []*Lease
	for range maxIdleSessions + 1 {
		l := s.Lease()
		p, err := l.Provider(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.GetTeam(ctx, poolTeam); err != nil {
			t.Fatal(err)
		}
		p.Close()
		leases = append(leases, l)
	}
	for _, l := range leases {
		l.Release(true)
	}
	if pa.mcp.count("initialize") != maxIdleSessions+1 || pa.kept() != maxIdleSessions || pa.mcp.closed() != 1 {
		t.Fatal("idle cap", pa.mcp.count("initialize"), pa.kept(), pa.mcp.closed())
	}
}

// A 401 on a kept session's first call re-mints the token and reopens the
// session once; the write succeeds, and the reopened session, now under
// the new token, is kept and reused by the next write.
func TestSessionsKeepASessionReopenedUnderANewToken(t *testing.T) {
	pa := newPoolApp(t)
	var first, second, third atomic.Int64
	if err := pa.write(t, &first, true, poolTeam); err != nil {
		t.Fatal(err)
	}
	old, _, _ := pa.app.Token(context.Background())
	// A minute on, a 401 may re-mint (mintPace).
	*pa.now = pa.now.Add(61 * time.Second)
	pa.mcp.refuse = 1
	if err := pa.write(t, &second, true, poolTeam); err != nil {
		t.Fatal("the reused session was not reopened after a 401", err)
	}
	if fresh, _, _ := pa.app.Token(context.Background()); fresh == old {
		t.Fatal("the 401 did not re-mint the token")
	}
	if pa.mcp.count("initialize") != 2 || pa.kept() != 1 {
		t.Fatal("the reopened session was not kept", pa.mcp.count("initialize"), pa.kept(), pa.mcp.closed())
	}
	if err := pa.write(t, &third, true, poolTeam); err != nil || third.Load() != 1 || pa.mcp.count("initialize") != 2 {
		t.Fatal("the reopened session was not reused", err, third.Load(), pa.mcp.count("initialize"))
	}
}
