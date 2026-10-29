package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/outbox"
)

// outboxServer answers /v1/execute by mode: "up" accepts, "down" is 503,
// "maintenance" is a drain's refusal, "refuse" a definitive 400.
type outboxServer struct {
	*httptest.Server
	mode     atomic.Value
	mu       sync.Mutex
	accepted []bus.Request
	tried    int
	tokens   atomic.Int64
}

func newOutboxServer(t *testing.T) *outboxServer {
	t.Helper()
	s := &outboxServer{}
	s.mode.Store("up")
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request bus.Request
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&request); err != nil {
			t.Error(err)
		}
		s.mu.Lock()
		s.tried++
		s.mu.Unlock()
		switch s.mode.Load().(string) {
		case "down":
			w.WriteHeader(503)
			return
		case "maintenance":
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(503)
			w.Write([]byte(`{"error":{"code":"maintenance","message":"the server is in maintenance; retry after 1 s"}}`))
			return
		case "refuse":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"code":"invalid","message":"session instance mismatch; pass the instance returned by register"}}`))
			return
		}
		s.mu.Lock()
		s.accepted = append(s.accepted, request)
		s.mu.Unlock()
		json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: map[string]any{"id": "msg"}})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *outboxServer) ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for _, r := range s.accepted {
		op, _ := r.Args["operation_id"].(string)
		out = append(out, r.Operation+":"+op)
	}
	return out
}

func outboxClient(t *testing.T, s *outboxServer) (*Client, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "profile")
	client, err := NewWithTokenSource(Config{URL: s.URL, Audience: "audience", Principal: "ada@example.test", CachePath: filepath.Join(dir, "cache.db")}, TokenSourceFunc(func(context.Context) (string, error) {
		s.tokens.Add(1)
		return "synthetic-token", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ob := client.Outbox(); ob != nil {
			ob.Close()
		}
	})
	return client, filepath.Join(dir, "outbox.db")
}

// CloseOutboxes closes what the process opened: the file isn't held
// after a command (Windows can't remove a file held open).
func TestCloseOutboxes(t *testing.T) {
	s := newOutboxServer(t)
	client, _ := outboxClient(t, s)
	ob := client.Outbox()
	if _, err := ob.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	CloseOutboxes()
	if _, err := ob.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatal("the outbox is still open:", err)
	}
	opened.Lock()
	defer opened.Unlock()
	if len(opened.stores) != 0 {
		t.Fatal(opened.stores)
	}
}

func shortWindow(t *testing.T, d time.Duration) {
	before, beforeFirst := queueAfter, drainFirst
	queueAfter, drainFirst = d, time.Second
	t.Cleanup(func() { queueAfter, drainFirst = before, beforeFirst })
}

func message(op, body string) bus.Request {
	return bus.Request{Operation: "send", Workspace: "ws_1", Project: "prj_1", Session: "worker", Args: map[string]any{"instance": "instance-1", "to": "peer", "body": body, "operation_id": op}}
}

// A write that can't reach the server is retried in process, then queued
// (queued_local, as a result); the next write of its queue waits behind
// it; and the drain sends both, in order, verbatim, with fresh
// credentials.
func TestOutboxQueuesAndDrains(t *testing.T) {
	shortWindow(t, 1500*time.Millisecond)
	s := newOutboxServer(t)
	client, path := outboxClient(t, s)
	ctx := context.Background()
	s.mode.Store("down")
	started := time.Now()
	result, err := client.Execute(ctx, message("op-1", "first"))
	if err != nil || result.Source != "client_outbox" {
		t.Fatal(result, err)
	}
	data := result.Data.(map[string]any)
	if data["outcome"] != "queued_local" || data["operation_id"] != "op-1" || data["position"] != 1 || !strings.Contains(data["message"].(string), "don't resend it with a new operation ID") {
		t.Fatal(data)
	}
	if s.tried < 2 || time.Since(started) > 5*time.Second {
		t.Fatalf("retried %d times in %v", s.tried, time.Since(started))
	}
	tried := s.tried
	result, err = client.Execute(ctx, message("op-2", "second"))
	if err != nil || result.Data.(map[string]any)["position"] != 2 {
		t.Fatal(result, err)
	}
	// Behind a waiting write, only the drain of its queue is tried: the
	// head, never the new write first.
	if s.tried > tried+1 {
		t.Fatal("the new write was tried ahead of the queue", s.tried, tried)
	}
	if st, _ := client.Outbox().Status(ctx); st.Waiting != 2 {
		t.Fatal(st)
	}
	s.mode.Store("up")
	tokens := s.tokens.Load()
	client.Outbox().SetNow(func() time.Time { return time.Now().Add(time.Hour) })
	report, err := client.DrainOutbox(ctx, 5*time.Second)
	if err != nil || report.Sent != 2 || report.Waiting != 0 {
		t.Fatal(report, err)
	}
	if strings.Join(s.ops(), ",") != "send:op-1,send:op-2" || s.tokens.Load() < tokens+2 {
		t.Fatal(s.ops(), s.tokens.Load(), tokens)
	}
	got := s.accepted[0]
	if got.Workspace != "ws_1" || got.Project != "prj_1" || got.Session != "worker" || got.Args["instance"] != "instance-1" || got.Args["body"] != "first" || got.Args["to"] != "peer" {
		t.Fatal("not verbatim", got)
	}
	if path != outbox.PathFor(filepath.Join(filepath.Dir(path), "cache.db")) {
		t.Fatal(path)
	}
}

// A write behind waiting writes is sent after them once the server is
// back: the drain runs first.
func TestOutboxDrainsBeforeANewWrite(t *testing.T) {
	shortWindow(t, 200*time.Millisecond)
	s := newOutboxServer(t)
	client, _ := outboxClient(t, s)
	ctx := context.Background()
	s.mode.Store("down")
	if result, err := client.Execute(ctx, message("op-1", "first")); err != nil || result.Source != "client_outbox" {
		t.Fatal(result, err)
	}
	s.mode.Store("up")
	client.Outbox().SetNow(func() time.Time { return time.Now().Add(time.Hour) })
	if result, err := client.Execute(ctx, message("op-2", "second")); err != nil || result.Source != "server" {
		t.Fatal(result, err)
	}
	if strings.Join(s.ops(), ",") != "send:op-1,send:op-2" {
		t.Fatal(s.ops())
	}
}

// §2.4: a maintenance refusal is definitive (nothing was sent): a write
// that never queues gets it as an error that says when to retry; a
// queueable write waits Retry-After, then queues.
func TestOutboxMaintenance(t *testing.T) {
	shortWindow(t, 500*time.Millisecond)
	s := newOutboxServer(t)
	client, _ := outboxClient(t, s)
	ctx := context.Background()
	s.mode.Store("maintenance")
	register := bus.Request{Operation: "register_session", Workspace: "ws_1", Project: "prj_1", Args: map[string]any{"name": "worker"}}
	_, err := client.Execute(ctx, register)
	wait, ok := Maintenance(err)
	if !ok || wait != time.Second || !strings.Contains(err.Error(), "the server is in maintenance; nothing was sent; retry after 1 s with the same operation ID") || strings.Contains(err.Error(), "uncertain") {
		t.Fatal(wait, ok, err)
	}
	tried := s.tried
	result, err := client.Execute(ctx, message("op-1", "x"))
	if err != nil || !strings.Contains(result.Data.(map[string]any)["message"].(string), "the server is in maintenance") {
		t.Fatal(result, err)
	}
	// Retry-After (1 s) is past the window: queued after one try.
	if s.tried != tried+1 {
		t.Fatal("tried", s.tried-tried)
	}
	if _, ok := Maintenance(errors.New("other")); ok {
		t.Fatal("not maintenance")
	}
}

// A definitive refusal never queues.
func TestOutboxRefusalFailsFast(t *testing.T) {
	shortWindow(t, time.Second)
	s := newOutboxServer(t)
	client, _ := outboxClient(t, s)
	s.mode.Store("refuse")
	_, err := client.Execute(context.Background(), message("op-1", "x"))
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 400 || s.tried != 1 {
		t.Fatal(err, s.tried)
	}
	if st, _ := client.Outbox().Status(context.Background()); st.Waiting != 0 {
		t.Fatal(st)
	}
}

// A queued ack without an operation_id gets one, so its replay is the
// same ack; a write that never queues fails fast as before.
func TestOutboxAckAndNonQueueable(t *testing.T) {
	shortWindow(t, 100*time.Millisecond)
	s := newOutboxServer(t)
	client, _ := outboxClient(t, s)
	ctx := context.Background()
	s.mode.Store("down")
	ack := bus.Request{Operation: "ack", Workspace: "ws_1", Project: "prj_1", Session: "worker", Args: map[string]any{"instance": "instance-1", "acks": []any{map[string]any{"message_id": "msg_1"}}}}
	result, err := client.Execute(ctx, ack)
	if err != nil || !strings.HasPrefix(result.Data.(map[string]any)["operation_id"].(string), "ack-") {
		t.Fatal(result, err)
	}
	if _, set := ack.Args["operation_id"]; set {
		t.Fatal("the caller's arguments were changed")
	}
	key := outbox.KeyOf(s.URL, ack)
	if pending, _ := client.Outbox().PendingAck(ctx, key, "msg_1"); !pending {
		t.Fatal("ack not pending")
	}
	tried := s.tried
	_, err = client.Execute(ctx, bus.Request{Operation: "heartbeat", Workspace: "ws_1", Project: "prj_1", Session: "worker", Args: map[string]any{"instance": "instance-1"}})
	if err == nil || !strings.Contains(err.Error(), "no write queued") || s.tried != tried+1 {
		t.Fatal(err, s.tried-tried)
	}
	// §1.8: a re-offered message's ack is promoted and sent first.
	s.mode.Store("up")
	queued, pending, err := client.PromoteAck(ctx, ack, "msg_1")
	if err != nil || !queued || pending || strings.Join(s.ops(), ",") != "ack:"+result.Data.(map[string]any)["operation_id"].(string) {
		t.Fatal(pending, err, s.ops())
	}
}

// The kill switch: nothing queues.
func TestOutboxKillSwitch(t *testing.T) {
	shortWindow(t, 100*time.Millisecond)
	t.Setenv("CLAWBRIDGE_CLIENT_OUTBOX", "off")
	s := newOutboxServer(t)
	client, _ := outboxClient(t, s)
	s.mode.Store("down")
	if _, err := client.Execute(context.Background(), message("op-1", "x")); err == nil || !strings.Contains(err.Error(), "no write queued") || s.tried != 1 {
		t.Fatal(err, s.tried)
	}
	if client.Outbox() != nil {
		t.Fatal("outbox opened")
	}
}

// The drain's view of a refusal of an old instance (§1.8, §1.9).
func TestClassify(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		err         error
		unavailable bool
		kind        outbox.AnswerKind
		class       string
	}{
		{nil, false, outbox.Accepted, ""},
		{CredentialFailure(errors.New("x")), false, outbox.NoCredential, "unauthenticated"},
		{&HTTPError{Status: 503, Code: "maintenance", RetryAfter: 3 * time.Second}, false, outbox.Maintenance, "maintenance"},
		{&HTTPError{Status: 503, Code: "rejected"}, true, outbox.Transient, "unavailable"},
		{errors.New("cloud transport unavailable"), true, outbox.Transient, "unavailable"},
		{&HTTPError{Status: 403, Code: "forbidden"}, false, outbox.Refused, "forbidden"},
		{&HTTPError{Status: 404, Code: "not_found"}, false, outbox.Refused, "not_found"},
		{&HTTPError{Status: 400, Code: "invalid", Message: "session instance mismatch; pass the instance returned by register"}, false, outbox.Refused, outbox.ClassInstance},
		{errors.New("invalid authoritative cloud response"), false, outbox.Refused, "invalid_response"},
	}
	for _, c := range cases {
		got := classify(ctx, bus.Result{}, c.unavailable, c.err)
		if got.Kind != c.kind || got.Class != c.class {
			t.Errorf("%v: %+v", c.err, got)
		}
	}
	if got := classify(ctx, bus.Result{}, false, &HTTPError{Status: 503, Code: "maintenance", RetryAfter: 3 * time.Second}); got.RetryAfter != 3*time.Second {
		t.Fatal(got)
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if got := classify(done, bus.Result{}, false, errors.New("context canceled")); got.Kind != outbox.Transient {
		t.Fatal("an outcome cut off by the deadline is uncertain", got)
	}
}

func TestRetryAfterHeader(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 0, "x": 0, "-1": 0, "0": 0, "7": 7 * time.Second, "99999": time.Hour} {
		h := http.Header{}
		if value != "" {
			h.Set("Retry-After", value)
		}
		if got := retryAfter(h); got != want {
			t.Errorf("%q: %v", value, got)
		}
	}
}

// A read the draining server serves says when the drain ends; the next
// answer without it clears it.
func TestMaintenanceUntil(t *testing.T) {
	until := ""
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if until != "" {
			w.Header().Set(MaintenanceUntilHeader, until)
		}
		json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: map[string]any{}})
	}))
	defer host.Close()
	client, err := NewWithTokenSource(Config{URL: host.URL, Audience: "audience"}, TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	read := bus.Request{Operation: "list_projects", Workspace: "w", Args: map[string]any{}}
	until = "2026-10-28T22:10:00Z"
	if _, err = client.Execute(context.Background(), read); err != nil {
		t.Fatal(err)
	}
	if got, ok := client.MaintenanceUntil(); !ok || !got.Equal(time.Date(2026, 10, 28, 22, 10, 0, 0, time.UTC)) {
		t.Fatal(got, ok)
	}
	until = ""
	client.Execute(context.Background(), read)
	if _, ok := client.MaintenanceUntil(); ok {
		t.Fatal("kept after the drain")
	}
}

// A queued mark-read carries no operation ID, so its notice names the
// messages it marks and asks for no rerun; a write with one still says not
// to resend it under a new one.
func TestOutboxQueuedMarkReadNamesItsMessages(t *testing.T) {
	shortWindow(t, 100*time.Millisecond)
	s := newOutboxServer(t)
	client, _ := outboxClient(t, s)
	ctx := context.Background()
	s.mode.Store("down")
	read := bus.Request{Operation: "mark_read", Workspace: "ws_1", Project: "prj_1", Session: "worker", Args: map[string]any{"instance": "instance-1", "ids": []any{"msg_1", "msg_2"}}}
	result, err := client.Execute(ctx, read)
	if err != nil {
		t.Fatal(err)
	}
	msg := result.Data.(map[string]any)["message"].(string)
	if !strings.Contains(msg, "; mark-read of msg_1, msg_2 is queued on this machine (position 1)") || !strings.HasSuffix(msg, "; don't run it again") || strings.Contains(msg, "  ") || strings.Contains(msg, "operation ID") {
		t.Fatal(msg)
	}
	// A long list is cut to the first five.
	read.Args = map[string]any{"instance": "instance-1", "ids": []any{"m1", "m2", "m3", "m4", "m5", "m6", "m7"}}
	result, err = client.Execute(ctx, read)
	if msg = result.Data.(map[string]any)["message"].(string); err != nil || !strings.Contains(msg, "; mark-read of m1, m2, m3, m4, m5 and 2 more is queued") {
		t.Fatal(msg, err)
	}
	result, err = client.Execute(ctx, message("op-1", "x"))
	if msg = result.Data.(map[string]any)["message"].(string); err != nil || !strings.Contains(msg, "; send op-1 is queued") || !strings.HasSuffix(msg, "; don't resend it with a new operation ID") {
		t.Fatal(msg, err)
	}
}

// A long-lived client's drainer (FR11) sends what waits on its own: on its
// interval while anything waits, and at once after a successful call, with
// no command run; it stops with its context.
func TestDrainOutboxWhile(t *testing.T) {
	shortWindow(t, 100*time.Millisecond)
	for name, every := range map[string]time.Duration{"interval": 50 * time.Millisecond, "kick": time.Hour} {
		t.Run(name, func(t *testing.T) {
			s := newOutboxServer(t)
			client, _ := outboxClient(t, s)
			s.mode.Store("down")
			if result, err := client.Execute(context.Background(), message("op-1", "first")); err != nil || result.Source != "client_outbox" {
				t.Fatal(result, err)
			}
			client.Outbox().SetNow(func() time.Time { return time.Now().Add(time.Hour) })
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { client.DrainOutboxWhile(ctx, every); close(done) }()
			s.mode.Store("up")
			if name == "kick" {
				// A successful call of another kind wakes the drainer.
				if _, err := client.Execute(context.Background(), bus.Request{Operation: "whoami", Args: map[string]any{}}); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for !slices.Contains(s.ops(), "send:op-1") && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if !slices.Contains(s.ops(), "send:op-1") {
				t.Fatal("the drainer sent nothing:", s.ops())
			}
			if st, _ := client.Outbox().Status(context.Background()); st.Waiting != 0 {
				t.Fatal(st)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the drainer didn't stop with its context")
			}
		})
	}
}

// The drainer looks the outbox up each turn: a client whose outbox can't
// open yet keeps running, and stops with its context.
func TestDrainOutboxWhileWithoutAnOutbox(t *testing.T) {
	client, err := NewWithTokenSource(Config{URL: "https://bus.example.test", Audience: "audience", Principal: "ada@example.test"}, TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	if client.Outbox() != nil {
		t.Skip("this client has an outbox")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { client.DrainOutboxWhile(ctx, 10*time.Millisecond); close(done) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the drainer stopped without an outbox")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drainer didn't stop with its context")
	}
}
