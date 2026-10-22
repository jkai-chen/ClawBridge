package linearboard

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// pooledV2 is one opened native connection: it counts binding reads and
// reports whether it was closed or its session went away.
type pooledV2 struct {
	*providerV2
	teams, projects int
	closed          bool
	dead            bool
}

func (p *pooledV2) GetTeam(ctx context.Context, id string) (linearprovider.Team, error) {
	p.teams++
	return p.providerV2.GetTeam(ctx, id)
}
func (p *pooledV2) GetProject(ctx context.Context, id string) (linearprovider.Project, error) {
	p.projects++
	return p.providerV2.GetProject(ctx, id)
}
func (p *pooledV2) Close() error               { p.closed = true; return nil }
func (p *pooledV2) Alive(context.Context) bool { return !p.dead }

type accessConnections struct{ access *linearconn.Access }

func (c accessConnections) Status(context.Context, string) (linearconn.Profile, error) {
	return linearconn.Profile{ID: "profile-1", Name: "test", Access: *c.access}, nil
}
func (accessConnections) HTTPClient(context.Context, string) (*http.Client, error) {
	return &http.Client{}, nil
}

const missingIssueV2 = "99999999-9999-4999-8999-999999999999"

type poolHarness struct {
	s      *Service
	scope  bus.Request
	p      *providerV2
	opened []*pooledV2
	ro     []bool
	now    time.Time
	access linearconn.Access
}

func newPoolHarness(t *testing.T, keep bool) *poolHarness {
	t.Helper()
	s, _, p, scope := testV2(t)
	h := &poolHarness{s: s, scope: scope, p: p, now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), access: linearconn.Write}
	s.cfg.Now = func() time.Time { return h.now }
	s.cfg.Connections = accessConnections{&h.access}
	s.cfg.Open = func(_ context.Context, _ *http.Client, ro bool) (Provider, error) {
		c := &pooledV2{providerV2: p}
		h.opened = append(h.opened, c)
		h.ro = append(h.ro, ro)
		return c, nil
	}
	if keep {
		s.KeepProviderOpen(ProviderTTL)
	}
	return h
}

// run executes one MCP command and returns its Linear reads and its error text.
func (h *poolHarness) run(t *testing.T, operation string, args map[string]any) (int, string) {
	t.Helper()
	before := h.reads()
	_, err := h.s.ExecuteV2(context.Background(), h.scope, operation, args)
	text := ""
	if err != nil {
		text = err.Error()
	}
	return h.reads() - before, text
}
func (h *poolHarness) reads() int {
	n := h.p.reads + len(h.p.queries)
	for _, c := range h.opened {
		n += c.teams + c.projects
	}
	return n
}

var poolCommands = []struct {
	operation string
	args      map[string]any
}{
	{"context", map[string]any{}},
	{"issue", map[string]any{"id": issueID}},
	{"issues", map[string]any{}},
	{"issue", map[string]any{"id": missingIssueV2}},
	{"update", map[string]any{"id": issueID, "title": "kept connection", "operation_id": "pool-update"}},
	{"issue", map[string]any{"id": issueID}},
}

func TestLinearV2MCPKeepsOneConnectionWithIdenticalReadsAndErrors(t *testing.T) {
	plain, kept := newPoolHarness(t, false), newPoolHarness(t, true)
	for n, c := range poolCommands {
		wantReads, wantErr := plain.run(t, c.operation, c.args)
		gotReads, gotErr := kept.run(t, c.operation, c.args)
		if gotReads != wantReads || gotErr != wantErr {
			t.Fatalf("%d %s: kept reads=%d err=%q, per-call reads=%d err=%q", n, c.operation, gotReads, gotErr, wantReads, wantErr)
		}
	}
	if plain.p.writes != 1 || kept.p.writes != 1 {
		t.Fatalf("writes plain=%d kept=%d", plain.p.writes, kept.p.writes)
	}
	// Per-call: one connection and tools/list per command, each closed.
	if len(plain.opened) != len(poolCommands) {
		t.Fatalf("per-call opens = %d", len(plain.opened))
	}
	for _, c := range plain.opened {
		if !c.closed {
			t.Fatal("per-call connection left open")
		}
	}
	// Kept: the failed read (command 3) forces exactly one reopen.
	if len(kept.opened) != 2 || !kept.opened[0].closed || kept.opened[1].closed {
		t.Fatalf("kept opens = %d", len(kept.opened))
	}
	if kept.s.Close() != nil || !kept.opened[1].closed {
		t.Fatal("Close left the kept connection open")
	}
}

func TestLinearV2MCPReopensAfterTheTTL(t *testing.T) {
	h := newPoolHarness(t, true)
	read := map[string]any{"id": issueID}
	h.run(t, "issue", read)
	h.now = h.now.Add(ProviderTTL - time.Second)
	h.run(t, "issue", read)
	if len(h.opened) != 1 {
		t.Fatalf("opens within the TTL = %d", len(h.opened))
	}
	h.now = h.now.Add(time.Second)
	if _, e := h.run(t, "issue", read); e != "" {
		t.Fatal(e)
	}
	if len(h.opened) != 2 || !h.opened[0].closed {
		t.Fatalf("opens after the TTL = %d", len(h.opened))
	}
	// A clock that steps back never extends a connection's life.
	h.now = h.now.Add(-time.Hour)
	h.run(t, "issue", read)
	if len(h.opened) != 3 {
		t.Fatalf("opens after the clock stepped back = %d", len(h.opened))
	}
}

func TestLinearV2MCPReopensADeadSessionInsteadOfFailing(t *testing.T) {
	h := newPoolHarness(t, true)
	read := map[string]any{"id": issueID}
	h.run(t, "issue", read)
	h.opened[0].dead = true
	if _, e := h.run(t, "issue", read); e != "" {
		t.Fatal(e)
	}
	if len(h.opened) != 2 || !h.opened[0].closed {
		t.Fatalf("opens after a dead session = %d", len(h.opened))
	}
}

func TestLinearV2MCPReopensOnCapabilityUnavailable(t *testing.T) {
	h := newPoolHarness(t, true)
	h.run(t, "issue", map[string]any{"id": issueID})
	h.p.preflightErr = &linearprovider.Error{Kind: "capability_unavailable"}
	_, e := h.run(t, "update", map[string]any{"id": issueID, "title": "drift", "operation_id": "pool-drift"})
	if e == "" || h.p.writes != 0 {
		t.Fatalf("drifted write: %q writes=%d", e, h.p.writes)
	}
	h.p.preflightErr = nil
	h.run(t, "issue", map[string]any{"id": issueID})
	if len(h.opened) != 2 || !h.opened[0].closed {
		t.Fatalf("opens after capability_unavailable = %d", len(h.opened))
	}
}

func TestLinearV2MCPReopensWhenProfileAccessChanges(t *testing.T) {
	h := newPoolHarness(t, true)
	read := map[string]any{"id": issueID}
	h.run(t, "issue", read)
	h.access = linearconn.Read
	h.run(t, "issue", read)
	if len(h.opened) != 2 || !h.opened[0].closed || h.ro[0] || !h.ro[1] {
		t.Fatalf("opens=%d readonly=%v", len(h.opened), h.ro)
	}
}

func TestProviderPoolClosesARetiredConnectionOnlyAfterItsLastUser(t *testing.T) {
	pool := &providerPool{ttl: ProviderTTL}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	var opened []*pooledV2
	fresh := func() (Provider, error) {
		c := &pooledV2{providerV2: &providerV2{}}
		opened = append(opened, c)
		return c, nil
	}
	_, releaseA, _ := pool.open(context.Background(), false, now, fresh)
	_, releaseB, _ := pool.open(context.Background(), false, now, fresh)
	pool.drop()
	releaseA()
	releaseA()
	if opened[0].closed {
		t.Fatal("closed while still in use")
	}
	releaseB()
	if len(opened) != 1 || !opened[0].closed {
		t.Fatal("retired connection not closed after its last user")
	}
	if _, _, e := pool.open(context.Background(), false, now, func() (Provider, error) { return nil, fmt.Errorf("offline") }); e == nil || pool.current != nil {
		t.Fatal("a failed open was kept")
	}
}
