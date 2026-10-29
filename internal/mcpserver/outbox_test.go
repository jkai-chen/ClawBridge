package mcpserver

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/outbox"
	"clawbridge/internal/remote"
)

// The hosted client is the MCP server's outbox client.
var _ outboxClient = (*remote.Client)(nil)

// outboxStore is a hosted client with an outbox: acks named in localAcks
// wait locally, and those in sendable are sent when promoted.
type outboxStore struct {
	*queueStore
	ob       *outbox.Store
	mu       sync.Mutex
	drains   int
	every    time.Duration
	promoted []string
	// localAcks maps a message to whether its ack still waits after a
	// promote (true) or is sent by it (false).
	localAcks map[string]bool
}

func newOutboxStore(t *testing.T, q *queueStore) *outboxStore {
	t.Helper()
	ob, err := outbox.Open(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ob.Close() })
	return &outboxStore{queueStore: q, ob: ob, localAcks: map[string]bool{}}
}

func (s *outboxStore) Outbox() *outbox.Store { return s.ob }
func (s *outboxStore) DrainOutboxWhile(_ context.Context, every time.Duration) {
	s.mu.Lock()
	s.drains++
	s.every = every
	s.mu.Unlock()
}
func (s *outboxStore) PromoteAck(_ context.Context, r bus.Request, id string) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, queued := s.localAcks[id]
	if queued {
		s.promoted = append(s.promoted, id+"@"+r.Args["instance"].(string))
	}
	return queued, pending, nil
}

func (s *outboxStore) drained() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drains
}

func ids(data map[string]any) []string {
	out := []string{}
	for _, m := range rows(data, "messages") {
		out = append(out, str(m["id"]))
	}
	return out
}

// whoami says what waits on this machine (§1.5).
func TestWhoamiShowsTheClientOutbox(t *testing.T) {
	store := newOutboxStore(t, &queueStore{})
	r := bus.Request{Operation: "send", Workspace: "w", Project: "p", Session: "agent", Args: map[string]any{"instance": "inst-1", "to": "peer", "body": "x", "operation_id": "op-1"}}
	if _, err := store.ob.Enqueue(context.Background(), "https://bus.example.test", outbox.Scope{}, r); err != nil {
		t.Fatal(err)
	}
	ctx, cs := queueSession(t, store, Options{})
	out := callTool(t, ctx, cs, "whoami", map[string]any{})
	data, _ := out["data"].(map[string]any)
	got, _ := data["client_outbox"].(map[string]any)
	if got["waiting"] != float64(1) || got["dead"] != float64(0) {
		t.Fatal(out)
	}
}

// §1.8: a message whose ack waits locally isn't handed on again: inbox
// drops it once its promoted ack is sent, and flags it while the ack
// still waits; a lease of it isn't held.
func TestInboxSkipsMessagesAckedLocally(t *testing.T) {
	q := &queueStore{answer: func(r bus.Request) (bus.Result, error) {
		if r.Operation == "inbox" {
			return bus.Result{Source: "server", Data: map[string]any{"messages": []any{
				map[string]any{"id": "msg-new"}, map[string]any{"id": "msg-sent"}, map[string]any{"id": "msg-waiting"},
			}}}, nil
		}
		if r.Operation == "lease" {
			return bus.Result{Source: "server", Data: map[string]any{"messages": []any{
				map[string]any{"id": "msg-new", "lease_id": "lse_1"}, map[string]any{"id": "msg-waiting", "lease_id": "lse_2"},
			}}}, nil
		}
		return bus.Result{Source: "server", Data: map[string]any{}}, nil
	}}
	store := newOutboxStore(t, q)
	store.localAcks["msg-sent"] = false
	store.localAcks["msg-waiting"] = true
	dir := filepath.Join(t.TempDir(), "context")
	ctx, cs := queueSession(t, store, Options{ContextDir: dir, Server: "https://bus.example.test", LeasePoll: 10 * time.Millisecond})
	out := callTool(t, ctx, cs, "inbox", map[string]any{})
	data, _ := out["data"].(map[string]any)
	if strings.Join(ids(data), ",") != "msg-new,msg-waiting" {
		t.Fatal(ids(data))
	}
	for _, m := range rows(data, "messages") {
		flagged := m["ack_pending_locally"] == true && m["note"] == localAckNote
		if flagged != (m["id"] == "msg-waiting") {
			t.Fatal(m)
		}
	}
	waitFor(t, "the lease of a new message wasn't held", func() bool { r, ok := readRecord(t, dir); return ok && len(r.Leases) == 1 })
	r, _ := readRecord(t, dir)
	if r.Leases[0].MessageID != "msg-new" {
		t.Fatal("held a message acked locally", r.Leases)
	}
	store.mu.Lock()
	promoted := strings.Join(store.promoted, ",")
	store.mu.Unlock()
	if !strings.Contains(promoted, "msg-waiting@inst-1") {
		t.Fatal(promoted)
	}
}

// A queued ack settles its lease here as a sent one would: the agent
// handled the message.
func TestQueuedAckSettlesTheLease(t *testing.T) {
	q := &queueStore{answer: func(r bus.Request) (bus.Result, error) {
		switch r.Operation {
		case "lease":
			return bus.Result{Source: "server", Data: map[string]any{"messages": []any{map[string]any{"id": "msg-1", "lease_id": "lse_1"}}}}, nil
		case "ack":
			return bus.Result{Source: "client_outbox", Data: map[string]any{"outcome": "queued_local", "operation_id": "ack-1"}}, nil
		}
		return bus.Result{Source: "server", Data: map[string]any{}}, nil
	}}
	store := newOutboxStore(t, q)
	dir := filepath.Join(t.TempDir(), "context")
	ctx, cs := queueSession(t, store, Options{ContextDir: dir, Server: "https://bus.example.test", LeasePoll: time.Hour})
	callTool(t, ctx, cs, "inbox", map[string]any{})
	waitFor(t, "not held", func() bool { r, ok := readRecord(t, dir); return ok && len(r.Leases) == 1 })
	out := callTool(t, ctx, cs, "ack", map[string]any{"acks": []any{map[string]any{"message_id": "msg-1"}}})
	if data, _ := out["data"].(map[string]any); data["outcome"] != "queued_local" {
		t.Fatal(out)
	}
	waitFor(t, "the queued ack didn't settle the lease", func() bool { r, ok := readRecord(t, dir); return ok && len(r.Leases) == 0 })
}

// The MCP server runs the client's outbox drainer for its lifetime, at
// its interval (the loop itself is remote's, tested there).
func TestMCPRunsTheOutboxDrainer(t *testing.T) {
	store := newOutboxStore(t, &queueStore{})
	queueSession(t, store, Options{})
	waitFor(t, "the drainer never started", func() bool { return store.drained() == 1 })
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.every != outboxEvery {
		t.Fatal(store.every)
	}
}

// §2.4: the lease loop waits out a maintenance refusal without counting a
// failure or turning off.
func TestLeaseLoopWaitsOutMaintenance(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	q := &queueStore{answer: func(r bus.Request) (bus.Result, error) {
		if r.Operation == "lease" {
			mu.Lock()
			defer mu.Unlock()
			at = append(at, time.Now())
			if len(at) == 1 {
				return bus.Result{}, &remote.HTTPError{Status: 503, Code: "maintenance", RetryAfter: time.Second}
			}
		}
		return bus.Result{Source: "server", Data: map[string]any{}}, nil
	}}
	dir := filepath.Join(t.TempDir(), "context")
	ctx, cs := queueSession(t, q, Options{ContextDir: dir, Server: "https://bus.example.test", LeasePoll: 10 * time.Millisecond})
	callTool(t, ctx, cs, "inbox", map[string]any{})
	waitFor(t, "never polled again", func() bool { mu.Lock(); defer mu.Unlock(); return len(at) >= 2 })
	mu.Lock()
	gap := at[1].Sub(at[0])
	mu.Unlock()
	if gap < 900*time.Millisecond {
		t.Fatal("didn't wait out the maintenance", gap)
	}
	// The loop records itself polling after that lease returns.
	waitFor(t, "never recorded itself polling", func() bool { r, ok := readRecord(t, dir); return ok && r.Stream == "polling" })
}

func TestInstructionsSayWritesWaitInTheOutbox(t *testing.T) {
	_, cs := queueSession(t, &queueStore{}, Options{})
	text := cs.InitializeResult().Instructions
	for _, required := range []string{"queued_local", "never resend it", "client_outbox", "ack_pending_locally", "Every other write fails fast"} {
		if !strings.Contains(text, required) {
			t.Fatalf("instructions miss %q", required)
		}
	}
}

// The ack tool's request, exactly as the MCP server sends it, queues in
// the outbox and is found as the message's pending ack (P2).
func TestAckToolRequestQueues(t *testing.T) {
	q := &queueStore{}
	ctx, cs := queueSession(t, q, Options{})
	callTool(t, ctx, cs, "ack", map[string]any{"acks": []any{map[string]any{"message_id": "msg-1"}}, "operation_id": "msg:msg-1:ack"})
	sent := q.called("ack")
	if len(sent) != 1 || !outbox.Queueable(sent[0]) {
		t.Fatal(sent)
	}
	ob, err := outbox.Open(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	if _, err = ob.Enqueue(context.Background(), "https://bus.example.test", outbox.Scope{}, sent[0]); err != nil {
		t.Fatal(err)
	}
	if pending, err := ob.PendingAck(context.Background(), outbox.KeyOf("https://bus.example.test", sent[0]), "msg-1"); err != nil || !pending {
		t.Fatal(pending, err)
	}
}
