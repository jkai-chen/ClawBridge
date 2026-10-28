package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/notify"
	"clawbridge/internal/remote"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// queueStore answers the queue's operations and records every call.
type queueStore struct {
	mu     sync.Mutex
	calls  []bus.Request
	answer func(bus.Request) (bus.Result, error)
}

func (s *queueStore) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, r)
	answer := s.answer
	s.mu.Unlock()
	if answer != nil {
		return answer(r)
	}
	return bus.Result{Source: "server", Data: map[string]any{}}, nil
}

func (s *queueStore) called(operation string) []bus.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []bus.Request
	for _, c := range s.calls {
		if c.Operation == operation {
			out = append(out, c)
		}
	}
	return out
}

// streamingStore is a hosted client: it follows a fake consume stream.
type streamingStore struct {
	*queueStore
	scopes chan remote.StreamScope
	events chan string
	end    error
}

func (s *streamingStore) Stream(ctx context.Context, scope remote.StreamScope, opts remote.StreamOptions, handle func(remote.StreamEvent) error) error {
	s.scopes <- scope
	if s.end != nil {
		return s.end
	}
	opts.OnConnect()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case data, ok := <-s.events:
			if !ok {
				return &remote.StreamEnded{Reason: "replaced"}
			}
			if err := handle(remote.StreamEvent{Name: "message", Data: json.RawMessage(data)}); err != nil {
				return err
			}
		}
	}
}

func offer(id, lease string) string {
	return fmt.Sprintf(`{"recipient_seq":1,"lease_id":%q,"lease_until":"2026-10-28T00:10:00Z","attempt":1,"message":{"id":%q,"body":"hi"}}`, lease, id)
}

func queueSession(t *testing.T, store Executor, o Options) (context.Context, *mcp.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// The server removes its record as it ends, before the directory goes.
	t.Cleanup(func() {
		cancel()
		if o.ContextDir != "" {
			waitFor(t, "the record outlived the server", func() bool { _, ok, _ := ctxwatch.LoadQueueRecord(o.ContextDir, "agent"); return !ok })
		}
	})
	o.Context = ctx
	server := NewServerWithOptions(store, "w", "p", "agent", "inst-1", o)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "queue-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return ctx, cs
}

func callTool(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || r.IsError {
		t.Fatalf("%s: %v %+v", name, err, r)
	}
	var out map[string]any
	raw, _ := json.Marshal(r.StructuredContent)
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readRecord(t *testing.T, dir string) (ctxwatch.QueueRecord, bool) {
	t.Helper()
	r, ok, err := ctxwatch.LoadQueueRecord(dir, "agent")
	if err != nil {
		t.Fatal(err)
	}
	return r, ok
}

// The consume stream's offers are held: inbox marks them with their lease,
// ack and reply's ack take the lease from them, and the record the hooks
// read lists them until they are settled. The record goes when the server
// does.
func TestQueueHoldsTheStreamsLeases(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "context")
	base := &queueStore{answer: func(r bus.Request) (bus.Result, error) {
		switch r.Operation {
		case "inbox":
			return bus.Result{Source: "server", Data: map[string]any{"messages": []any{
				map[string]any{"id": "msg-1", "body": "one"}, map[string]any{"id": "msg-2", "body": "two"}, map[string]any{"id": "msg-3", "body": "three"}, map[string]any{"id": "msg-9", "body": "not offered"},
			}}}, nil
		case "ack":
			var out []any
			for _, a := range r.Args["acks"].([]any) {
				out = append(out, map[string]any{"message_id": a.(map[string]any)["message_id"], "outcome": "acked"})
			}
			return bus.Result{Source: "server", Data: map[string]any{"acks": out}}, nil
		case "reply":
			ack, _ := r.Args["ack"].(map[string]any)
			return bus.Result{Source: "server", Data: map[string]any{"id": "msg-r", "ack": map[string]any{"message_id": ack["message_id"], "outcome": "acked"}}}, nil
		}
		return bus.Result{Source: "server", Data: map[string]any{}}, nil
	}}
	store := &streamingStore{queueStore: base, scopes: make(chan remote.StreamScope, 1), events: make(chan string)}
	ctx, cs := queueSession(t, store, Options{ContextDir: dir, Server: "https://clawbridge.example.test", Diagnostics: &lines{}})
	// The record is there from the start; the stream only from the agent's
	// first message tool call.
	waitFor(t, "no record at the start", func() bool { r, ok := readRecord(t, dir); return ok && r.Stream == "off" })
	select {
	case <-store.scopes:
		t.Fatal("the stream opened before any message tool call")
	case <-time.After(50 * time.Millisecond):
	}
	callTool(t, ctx, cs, "inbox", map[string]any{})
	scope := <-store.scopes
	if scope.Mode != "consume" || scope.Instance != "inst-1" || scope.Session != "agent" {
		t.Fatal(scope)
	}
	store.events <- offer("msg-1", "lse_1")
	store.events <- offer("msg-2", "lse_2")
	store.events <- offer("msg-3", "lse_3")
	waitFor(t, "the record never listed the leases", func() bool { r, ok := readRecord(t, dir); return ok && len(r.Leases) == 3 && r.Stream == "stream" })
	r, _ := readRecord(t, dir)
	if r.Instance != "inst-1" || r.Server != "https://clawbridge.example.test" || r.MCPPID != os.Getpid() || r.AgentPID != os.Getppid() || r.Leases[0] != (ctxwatch.QueueLease{MessageID: "msg-1", LeaseID: "lse_1"}) {
		t.Fatalf("%+v", r)
	}

	inbox := callTool(t, ctx, cs, "inbox", map[string]any{})
	data := inbox["data"].(map[string]any)["messages"].([]any)
	lease := func(i int) any { return data[i].(map[string]any)["lease"] }
	if l, _ := lease(0).(map[string]any); l["lease_id"] != "lse_1" || l["attempt"] != float64(1) || lease(3) != nil {
		t.Fatal(data)
	}

	// An ack without a lease_id gets the held one; a given one is kept,
	// even for a held message.
	callTool(t, ctx, cs, "ack", map[string]any{"acks": []any{map[string]any{"message_id": "msg-1"}, map[string]any{"message_id": "msg-3", "lease_id": "lse_given"}}, "operation_id": "msg:msg-1:done"})
	acks := base.called("ack")[0].Args
	if fmt.Sprint(acks["acks"]) != "[map[lease_id:lse_1 message_id:msg-1] map[lease_id:lse_given message_id:msg-3]]" || acks["operation_id"] != "msg:msg-1:done" || acks["instance"] != "inst-1" {
		t.Fatal(acks)
	}
	waitFor(t, "an acked lease stayed in the record", func() bool { r, _ := readRecord(t, dir); return len(r.Leases) == 1 })

	// reply ack=true is the reply's own ack, with the held lease.
	callTool(t, ctx, cs, "reply", map[string]any{"message_id": "msg-2", "body": "done", "operation_id": "msg:msg-2:reply", "ack": true})
	if ack := base.called("reply")[0].Args["ack"]; fmt.Sprint(ack) != "map[lease_id:lse_2 message_id:msg-2]" {
		t.Fatal(ack)
	}
	waitFor(t, "a reply's ack left the lease held", func() bool { r, _ := readRecord(t, dir); return len(r.Leases) == 0 })
	// Without ack, reply sends none.
	callTool(t, ctx, cs, "reply", map[string]any{"message_id": "msg-2", "body": "again", "operation_id": "msg:msg-2:again"})
	if _, ok := base.called("reply")[1].Args["ack"]; ok {
		t.Fatal("reply without ack acked")
	}
	// The stream ends (replaced by another consumer): nothing is held.
	store.events <- offer("msg-4", "lse_4")
	waitFor(t, "no lease", func() bool { r, _ := readRecord(t, dir); return len(r.Leases) == 1 })
	close(store.events)
	waitFor(t, "an ended stream kept its leases", func() bool { r, _ := readRecord(t, dir); return r.Stream == "ended" && len(r.Leases) == 0 })
}

// The record goes with the server.
func TestQueueRecordRemovedOnExit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "context")
	ctx, cancel := context.WithCancel(context.Background())
	store := &streamingStore{queueStore: &queueStore{}, scopes: make(chan remote.StreamScope, 1), events: make(chan string)}
	NewServerWithOptions(store, "w", "p", "agent", "inst-1", Options{Context: ctx, ContextDir: dir, Server: "https://clawbridge.example.test"})
	waitFor(t, "no record", func() bool { _, ok := readRecord(t, dir); return ok })
	cancel()
	waitFor(t, "the record outlived the server", func() bool { _, ok := readRecord(t, dir); return !ok })
}

// Without a stream (local mode, or a server with the queue but no stream)
// the server polls lease with its own consumer; a server without the queue
// refuses lease, and nothing is held.
func TestQueuePollsLeaseWithoutAStream(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "context")
	store := &queueStore{answer: func(r bus.Request) (bus.Result, error) {
		if r.Operation == "lease" {
			return bus.Result{Source: "local-authority", Data: map[string]any{"messages": []map[string]any{{"id": "msg-1", "lease_id": "lse_1", "attempt": int64(1)}}}}, nil
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{}}, nil
	}}
	ctx, cs := queueSession(t, store, Options{ContextDir: dir, DB: "/var/clawbridge/local.db", LeasePoll: 10 * time.Millisecond})
	time.Sleep(50 * time.Millisecond)
	store.mu.Lock()
	early := len(store.calls)
	store.mu.Unlock()
	if early != 0 {
		t.Fatal("polled before any message tool call")
	}
	callTool(t, ctx, cs, "inbox", map[string]any{})
	waitFor(t, "never polled", func() bool { r, ok := readRecord(t, dir); return ok && r.Stream == "polling" && len(r.Leases) == 1 })
	waitFor(t, "polled once only", func() bool { return len(store.called("lease")) >= 3 })
	first, second := store.called("lease")[0].Args, store.called("lease")[1].Args
	if c, _ := first["consumer"].(string); !strings.HasPrefix(c, "mcp_") || second["consumer"] != c || first["instance"] != "inst-1" {
		t.Fatal(first, second)
	}

	old := &queueStore{answer: func(r bus.Request) (bus.Result, error) {
		if r.Operation == "lease" {
			return bus.Result{}, errors.New("unsupported operation")
		}
		return bus.Result{Source: "server", Data: map[string]any{}}, nil
	}}
	oldDir := filepath.Join(t.TempDir(), "context")
	oldCtx, oldCS := queueSession(t, old, Options{ContextDir: oldDir, Server: "https://old.example.test", LeasePoll: 10 * time.Millisecond})
	callTool(t, oldCtx, oldCS, "ack", map[string]any{"acks": []any{map[string]any{"message_id": "msg-1"}}})
	waitFor(t, "an old server's refusal wasn't taken", func() bool { r, ok := readRecord(t, oldDir); return ok && r.Stream == "off" })
	time.Sleep(50 * time.Millisecond)
	if n := len(old.called("lease")); n != 1 {
		t.Fatalf("polled an old server %d times", n)
	}
}

// While the agent is active, the held leases are extended; a lease the
// server no longer has is dropped. An idle agent's leases are left to run
// out.
func TestQueueExtendsWhileActive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "context")
	var mu sync.Mutex
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	base := &queueStore{answer: func(r bus.Request) (bus.Result, error) {
		if r.Operation == "extend" {
			var out []any
			for _, l := range r.Args["leases"].([]any) {
				o := map[string]any{"lease_id": l, "outcome": "extended", "lease_until": "later"}
				if l == "lse_2" {
					o = map[string]any{"lease_id": l, "outcome": "not_found"}
				}
				out = append(out, o)
			}
			return bus.Result{Source: "server", Data: map[string]any{"leases": out}}, nil
		}
		return bus.Result{Source: "server", Data: map[string]any{"messages": []any{}}}, nil
	}}
	store := &streamingStore{queueStore: base, scopes: make(chan remote.StreamScope, 1), events: make(chan string)}
	ctx, cs := queueSession(t, store, Options{ContextDir: dir, Server: "https://clawbridge.example.test", Now: clock, RecordEvery: 5 * time.Millisecond, ExtendEvery: time.Minute, ActiveWindow: 10 * time.Minute})
	callTool(t, ctx, cs, "inbox", map[string]any{})
	<-store.scopes
	store.events <- offer("msg-1", "lse_1")
	store.events <- offer("msg-2", "lse_2")
	waitFor(t, "no leases", func() bool { r, _ := readRecord(t, dir); return len(r.Leases) == 2 })
	callTool(t, ctx, cs, "inbox", map[string]any{})
	advance(time.Minute)
	waitFor(t, "an active agent's leases weren't extended", func() bool { return len(base.called("extend")) == 1 })
	e := base.called("extend")[0].Args
	if e["seconds"] != extendSeconds || len(e["leases"].([]any)) != 2 || e["instance"] != "inst-1" {
		t.Fatal(e)
	}
	waitFor(t, "a lease the server no longer has stayed held", func() bool { r, _ := readRecord(t, dir); return len(r.Leases) == 1 && r.Leases[0].LeaseID == "lse_1" })
	// Eleven minutes without a tool call: no more extends.
	advance(11 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if n := len(base.called("extend")); n != 1 {
		t.Fatalf("an idle agent's leases were extended: %d", n)
	}
}

// A compaction's hold the server finds at its start is from an agent that
// died mid-compaction, and a later one that outlives the limit is stale:
// both are set back to accept, and said so.
func TestQueueEndsAStaleCompactionHold(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "context")
	if err := ctxwatch.SaveCompactionHold(dir, "agent", ctxwatch.CompactionHold{Instance: "older", HeldAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	store := &queueStore{}
	var diagnostics lines
	queueSession(t, store, Options{ContextDir: dir, DB: "/var/clawbridge/local.db", Diagnostics: &diagnostics, LeasePoll: time.Hour})
	waitFor(t, "a hold left from before the start stayed", func() bool { return len(store.called("set_notification_policy")) == 1 })
	if p := store.called("set_notification_policy")[0].Args; p["policy"] != "accept" || p["instance"] != "inst-1" {
		t.Fatal(p)
	}
	waitFor(t, "the mark stayed", func() bool { _, held := ctxwatch.LoadCompactionHold(dir, "agent"); return !held })
	waitFor(t, "a lifted hold wasn't said", func() bool { return strings.Contains(diagnostics.String(), "accepted again") })

	// While running, a fresh hold stays; one past the limit is lifted.
	dir = filepath.Join(t.TempDir(), "context")
	var mu sync.Mutex
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	store = &queueStore{}
	queueSession(t, store, Options{ContextDir: dir, DB: "/var/clawbridge/local.db", Now: clock, RecordEvery: 5 * time.Millisecond, LeasePoll: time.Hour, Diagnostics: &lines{}})
	time.Sleep(30 * time.Millisecond)
	if err := ctxwatch.SaveCompactionHold(dir, "agent", ctxwatch.CompactionHold{Instance: "inst-1", HeldAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(store.called("set_notification_policy")); n != 0 {
		t.Fatal("a fresh compaction's hold was lifted")
	}
	mu.Lock()
	now = now.Add(ctxwatch.CompactionHoldLimit)
	mu.Unlock()
	waitFor(t, "a stale hold stayed", func() bool { return len(store.called("set_notification_policy")) == 1 })
}

// lines is a goroutine-safe writer.
type lines struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// With the stream open, the inbox resource is updated from the stream's
// offers, not by polling: here the poll never ticks.
func TestQueueResourceUpdatesFromTheStream(t *testing.T) {
	var mu sync.Mutex
	unread := int64(0)
	base := &queueStore{answer: func(r bus.Request) (bus.Result, error) {
		if r.Operation == "notification_status" {
			mu.Lock()
			defer mu.Unlock()
			return bus.Result{Source: "server", Data: notify.NotificationStatus{Status: notify.Status{WorkspaceID: "ws", ProjectID: "pr", SessionID: "se", UnreadCount: unread, LatestSequence: unread, NewCount: unread}, Policy: "accept"}}, nil
		}
		return bus.Result{Source: "server", Data: map[string]any{}}, nil
	}}
	store := &streamingStore{queueStore: base, scopes: make(chan remote.StreamScope, 1), events: make(chan string)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	never := func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }
	server := NewServerWithOptions(store, "w", "p", "agent", "inst-1", Options{Context: ctx, Wait: never, Diagnostics: &lines{}})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	events := make(chan string, 4)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "queue-test", Version: "1"}, &mcp.ClientOptions{ResourceUpdatedHandler: func(_ context.Context, r *mcp.ResourceUpdatedNotificationRequest) { events <- r.Params.URI }}).Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if _, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "inbox", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	<-store.scopes
	if err = cs.Subscribe(ctx, &mcp.SubscribeParams{URI: InboxStatusURI}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "no first check", func() bool { return len(base.called("notification_status")) >= 2 })
	mu.Lock()
	unread = 1
	mu.Unlock()
	store.events <- offer("msg-1", "lse_1")
	select {
	case u := <-events:
		if u != InboxStatusURI {
			t.Fatal(u)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stream's offer didn't update the resource")
	}
}

// Once the server has ended, a late write never brings its record back.
func TestQueueRecordNotWrittenAfterClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "context")
	q := newQueue(&binding{workspace: "w", project: "p", session: "agent"}, Options{ContextDir: dir, DB: "/var/clawbridge/local.db"})
	q.instance = "inst-1"
	q.save()
	if _, ok := readRecord(t, dir); !ok {
		t.Fatal("no record")
	}
	q.close()
	q.save()
	if _, ok := readRecord(t, dir); ok {
		t.Fatal("a write after close brought the record back")
	}
}

// An agent that never read the skill still learns to settle what it handles:
// the server's own instructions say ack, the reply's ack, the mark_read
// fallback and the msg:<message_id>:<step> operation IDs. Without them, its
// messages are offered again and end as dead letters.
func TestInstructionsSayHowToSettleAMessage(t *testing.T) {
	_, cs := queueSession(t, &queueStore{}, Options{})
	text := cs.InitializeResult().Instructions
	for _, required := range []string{"Handle each message, then settle it", "call ack", "reply with ack: true", "mark_read settles it", "operation_id msg:<message_id>:<step>", "offered again"} {
		if !strings.Contains(text, required) {
			t.Fatalf("instructions miss %q:\n%s", required, text)
		}
	}
}
