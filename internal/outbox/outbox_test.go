package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

const server = "https://bus.example.test"

var me = Scope{Principal: "ada@example.test", Credential: "oauth"}

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "profile", "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	s.SetNow(c.now)
	s.jitter = func(d time.Duration) time.Duration { return d }
	return s, c
}

func send(op, body string) bus.Request {
	return bus.Request{Operation: "send", Workspace: "w", Project: "p", Session: "s1", Args: map[string]any{"instance": "i1", "to": "s2", "body": body, "operation_id": op}}
}

func reply(op, parent string, ack bool) bus.Request {
	r := bus.Request{Operation: "reply", Workspace: "w", Project: "p", Session: "s1", Args: map[string]any{"instance": "i1", "message_id": parent, "body": "done", "operation_id": op}}
	if ack {
		r.Args["ack"] = map[string]any{"message_id": parent}
	}
	return r
}

func ack(op string, ids ...string) bus.Request {
	acks := []any{}
	for _, id := range ids {
		acks = append(acks, map[string]any{"message_id": id})
	}
	return bus.Request{Operation: "ack", Workspace: "w", Project: "p", Session: "s1", Args: map[string]any{"instance": "i1", "acks": acks, "operation_id": op}}
}

func enqueue(t *testing.T, s *Store, r bus.Request) Queued {
	t.Helper()
	q, err := s.Enqueue(context.Background(), server, me, r)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// sender answers each attempt from script, by operation_id, and records
// what was sent.
type sender struct {
	sent   []bus.Request
	answer func(bus.Request) Answer
}

func (f *sender) send(_ context.Context, r bus.Request) Answer {
	f.sent = append(f.sent, r)
	if f.answer == nil {
		return Answer{Kind: Accepted, Data: map[string]any{}}
	}
	return f.answer(r)
}

func (f *sender) ops() []string {
	out := []string{}
	for _, r := range f.sent {
		op, _ := r.Args["operation_id"].(string)
		if _, bare := r.Args["ack"]; !bare && r.Operation == "reply" {
			op += "(bare)"
		}
		out = append(out, op)
	}
	return out
}

// The allowlist (§1.1, D1): only writes a replay can't change.
func TestQueueable(t *testing.T) {
	coordinated := send("op", "x")
	coordinated.Args["coordination"] = map[string]any{"assignment_revision": 3}
	cases := map[string]struct {
		r    bus.Request
		want bool
	}{
		"send":                  {send("op", "x"), true},
		"reply":                 {reply("op", "m1", true), true},
		"kind message":          {bus.Request{Operation: "send", Args: map[string]any{"kind": "message", "operation_id": "op"}}, true},
		"kind reply":            {bus.Request{Operation: "reply", Args: map[string]any{"kind": "reply", "operation_id": "op"}}, true},
		"decision":              {bus.Request{Operation: "send", Args: map[string]any{"kind": "decision", "operation_id": "op"}}, false},
		"handoff":               {bus.Request{Operation: "send", Args: map[string]any{"kind": "handoff", "operation_id": "op"}}, false},
		"routing-change":        {bus.Request{Operation: "send", Args: map[string]any{"kind": "routing-change", "operation_id": "op"}}, false},
		"coordination":          {coordinated, false},
		"no operation_id":       {bus.Request{Operation: "send", Args: map[string]any{"body": "x"}}, false},
		"ack":                   {ack("op", "m1"), true},
		"empty ack":             {bus.Request{Operation: "ack", Args: map[string]any{}}, false},
		"mark_read":             {bus.Request{Operation: "mark_read", Args: map[string]any{}}, true},
		"heartbeat":             {bus.Request{Operation: "heartbeat"}, false},
		"extend":                {bus.Request{Operation: "extend"}, false},
		"lease":                 {bus.Request{Operation: "lease"}, false},
		"nack":                  {bus.Request{Operation: "nack"}, false},
		"register_session":      {bus.Request{Operation: "register_session"}, false},
		"retire_session":        {bus.Request{Operation: "retire_session"}, false},
		"update_session":        {bus.Request{Operation: "update_session"}, false},
		"coordinator_set":       {bus.Request{Operation: "coordinator_set"}, false},
		"assignment_claim":      {bus.Request{Operation: "assignment_claim"}, false},
		"linear_write":          {bus.Request{Operation: "linear_write", Args: map[string]any{"operation_id": "op"}}, false},
		"invite_create":         {bus.Request{Operation: "invite_create"}, false},
		"inbox":                 {bus.Request{Operation: "inbox"}, false},
		"set_notification":      {bus.Request{Operation: "set_notification_policy"}, false},
		"binding (linear_link)": {bus.Request{Operation: "linear_link"}, false},
	}
	for name, c := range cases {
		if got := Queueable(c.r); got != c.want {
			t.Errorf("%s: Queueable = %v", name, got)
		}
	}
}

func TestOutboxFileIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	s, err := Open(filepath.Join(dir, "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for path, want := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "outbox.db"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", path, info.Mode().Perm(), err)
		}
	}
	if PathFor(filepath.Join(dir, "mirror.db")) != filepath.Join(dir, "outbox.db") || PathFor("") != "" {
		t.Fatal(PathFor(filepath.Join(dir, "mirror.db")))
	}
}

func TestKillSwitch(t *testing.T) {
	t.Setenv("CLAWBRIDGE_CLIENT_OUTBOX", "off")
	if !Disabled() {
		t.Fatal("off")
	}
	t.Setenv("CLAWBRIDGE_CLIENT_OUTBOX", "")
	if Disabled() {
		t.Fatal("on")
	}
}

// A queue drains in order; a transient failure stops it (head of line),
// and the next drain resends the same request with the same operation_id.
func TestDrainInOrderAndHeadOfLine(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	enqueue(t, s, send("op-1", "first"))
	enqueue(t, s, reply("op-2", "m1", false))
	if q := enqueue(t, s, send("op-3", "third")); q.Position != 3 {
		t.Fatal(q)
	}
	down := true
	f := &sender{answer: func(r bus.Request) Answer {
		if down && r.Args["operation_id"] == "op-2" {
			return Answer{Kind: Transient, Class: "unavailable"}
		}
		return Answer{Kind: Accepted, Data: map[string]any{}}
	}}
	report, err := s.Drain(ctx, server, me, nil, f.send)
	if err != nil || report.Sent != 1 || report.Waiting != 2 || strings.Join(f.ops(), ",") != "op-1,op-2(bare)" {
		t.Fatal(report, err, f.ops())
	}
	// Not due yet: the backoff holds the queue.
	if report, _ = s.Drain(ctx, server, me, nil, f.send); report.Sent != 0 || len(f.sent) != 2 {
		t.Fatal(report, f.ops())
	}
	down = false
	c.add(time.Second)
	if report, err = s.Drain(ctx, server, me, nil, f.send); err != nil || report.Sent != 2 || report.Waiting != 0 {
		t.Fatal(report, err)
	}
	if strings.Join(f.ops(), ",") != "op-1,op-2(bare),op-2(bare),op-3" {
		t.Fatal(f.ops())
	}
	// Verbatim (D4): the stored request, with its own scope.
	got := f.sent[2]
	if got.Workspace != "w" || got.Project != "p" || got.Session != "s1" || got.Args["instance"] != "i1" || got.Args["message_id"] != "m1" {
		t.Fatal(got)
	}
}

// Backoff doubles to 60 s; Retry-After wins; maintenance doesn't count.
func TestDrainBackoffAndMaintenance(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	q := enqueue(t, s, send("op-1", "x"))
	answer := Answer{Kind: Transient}
	f := &sender{answer: func(bus.Request) Answer { return answer }}
	due := func() time.Duration {
		var at int64
		s.db.QueryRow(`SELECT next_attempt_at FROM client_outbox WHERE seq=?`, q.ID).Scan(&at)
		return time.Duration(at - s.at())
	}
	attempts := func() int {
		var n int
		s.db.QueryRow(`SELECT attempts FROM client_outbox WHERE seq=?`, q.ID).Scan(&n)
		return n
	}
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		s.Drain(ctx, server, me, nil, f.send)
		if due() != want {
			t.Fatal(due(), want)
		}
		c.add(want)
	}
	s.db.Exec(`UPDATE client_outbox SET attempts=10 WHERE seq=?`, q.ID)
	s.Drain(ctx, server, me, nil, f.send)
	if due() != maxBackoff {
		t.Fatal(due())
	}
	c.add(maxBackoff)
	answer = Answer{Kind: Transient, RetryAfter: 7 * time.Second}
	s.Drain(ctx, server, me, nil, f.send)
	if due() != 7*time.Second {
		t.Fatal(due())
	}
	c.add(7 * time.Second)
	before := attempts()
	answer = Answer{Kind: Maintenance, RetryAfter: 12 * time.Second}
	s.Drain(ctx, server, me, nil, f.send)
	if due() != 12*time.Second || attempts() != before {
		t.Fatal(due(), attempts(), before)
	}
}

// A refusal makes the head a dead letter and the queue goes on.
func TestDrainRefusalIsDeadLetter(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	enqueue(t, s, send("op-1", "x"))
	enqueue(t, s, send("op-2", "y"))
	f := &sender{answer: func(r bus.Request) Answer {
		if r.Args["operation_id"] == "op-1" {
			return Answer{Kind: Refused, Class: "forbidden"}
		}
		return Answer{Kind: Accepted, Data: map[string]any{}}
	}}
	report, err := s.Drain(ctx, server, me, nil, f.send)
	if err != nil || report.Sent != 1 || report.Dead != 1 {
		t.Fatal(report, err)
	}
	dead, _ := s.List(ctx, true)
	if len(dead) != 1 || dead[0].OperationID != "op-1" || dead[0].Error != "forbidden" || dead[0].State != "dead" {
		t.Fatal(dead)
	}
	if st, _ := s.Status(ctx); st.Dead != 1 || st.Waiting != 0 {
		t.Fatal(st)
	}
}

// A write is only sent as who made it (§1.6); no credential waits.
func TestDrainScope(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	enqueue(t, s, send("op-1", "x"))
	f := &sender{answer: func(bus.Request) Answer { return Answer{Kind: NoCredential} }}
	report, err := s.Drain(ctx, server, me, nil, f.send)
	if err != nil || !report.SignIn || report.Waiting != 1 {
		t.Fatal(report, err)
	}
	if report, _ = s.Drain(ctx, "https://other.example.test", me, nil, f.send); len(f.sent) != 1 || report.Waiting != 0 {
		t.Fatal("another server drained it", f.sent)
	}
	other := Scope{Principal: "grace@example.test", Credential: "oauth"}
	if report, _ = s.Drain(ctx, server, other, nil, f.send); report.Dead != 1 || len(f.sent) != 1 {
		t.Fatal(report, f.sent)
	}
	if dead, _ := s.List(ctx, true); len(dead) != 1 || dead[0].Error != ClassAccountChanged {
		t.Fatal(dead)
	}
}

// D2': a reply refused with its ack is resent bare under the same
// operation_id, and only its ack becomes a dead letter.
func TestDrainReplyAckRefusedIsResentBare(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	enqueue(t, s, reply("op-r", "m1", true))
	f := &sender{answer: func(r bus.Request) Answer {
		if _, carries := r.Args["ack"]; carries {
			return Answer{Kind: Refused, Class: "not_found"}
		}
		return Answer{Kind: Accepted, Data: map[string]any{}}
	}}
	report, err := s.Drain(ctx, server, me, nil, f.send)
	if err != nil || report.Dead != 1 || strings.Join(f.ops(), ",") != "op-r,op-r(bare)" {
		t.Fatal(report, err, f.ops())
	}
	dead, _ := s.List(ctx, true)
	if len(dead) != 1 || dead[0].Operation != "ack" || dead[0].Error != "ack_refused:not_found" || dead[0].OperationID != "op-r:ack" {
		t.Fatal(dead)
	}
	if st, _ := s.Status(ctx); st.Waiting != 0 {
		t.Fatal(st)
	}
}

// A reply refused itself (an instance change refuses the bare resend
// too) is the dead letter, with its ack.
func TestDrainReplyRefusedItself(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	enqueue(t, s, reply("op-r", "m1", true))
	f := &sender{answer: func(bus.Request) Answer { return Answer{Kind: Refused, Class: ClassInstance} }}
	if report, err := s.Drain(ctx, server, me, nil, f.send); err != nil || report.Dead != 1 || len(f.sent) != 2 {
		t.Fatal(report, err, f.ops())
	}
	dead, _ := s.List(ctx, true)
	if len(dead) != 1 || dead[0].Operation != "reply" || dead[0].Error != ClassInstance {
		t.Fatal(dead)
	}
}

// A bare resend that can't reach the server keeps the reply, bare, at the
// head; its ack is already the dead letter.
func TestDrainReplyBareResendUnreachable(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	enqueue(t, s, reply("op-r", "m1", true))
	enqueue(t, s, send("op-2", "after"))
	up := false
	f := &sender{answer: func(r bus.Request) Answer {
		if _, carries := r.Args["ack"]; carries {
			return Answer{Kind: Refused, Class: "not_found"}
		}
		if !up {
			return Answer{Kind: Transient}
		}
		return Answer{Kind: Accepted, Data: map[string]any{}}
	}}
	report, _ := s.Drain(ctx, server, me, nil, f.send)
	if report.Waiting != 2 || strings.Join(f.ops(), ",") != "op-r,op-r(bare)" {
		t.Fatal(report, f.ops())
	}
	up = true
	c.add(time.Minute)
	if report, _ = s.Drain(ctx, server, me, nil, f.send); report.Sent != 2 || strings.Join(f.ops(), ",") != "op-r,op-r(bare),op-r(bare),op-2" {
		t.Fatal(report, f.ops())
	}
	if dead, _ := s.List(ctx, true); len(dead) != 1 || dead[0].Operation != "ack" {
		t.Fatal(dead)
	}
}

// §1.8: an ack of a message offered again is found, moved to the head and
// sent first: acks may overtake sends.
func TestPendingAckPromotes(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	enqueue(t, s, send("op-1", "x"))
	enqueue(t, s, ack("op-a", "m1", "m2"))
	enqueue(t, s, reply("op-r", "m3", true))
	key := KeyOf(server, send("", ""))
	for id, want := range map[string]bool{"m1": true, "m2": true, "m3": true, "m": false, "m4": false} {
		if got, err := s.PendingAck(ctx, key, id); err != nil || got != want {
			t.Fatal(id, got, err)
		}
	}
	other := key
	other.Instance = "i2"
	if got, _ := s.PendingAck(ctx, other, "m1"); got {
		t.Fatal("another instance's ack")
	}
	if err := s.Promote(ctx, key, "m2"); err != nil {
		t.Fatal(err)
	}
	f := &sender{}
	s.Drain(ctx, server, me, &key, f.send)
	if strings.Join(f.ops(), ",") != "op-a,op-1,op-r" {
		t.Fatal(f.ops())
	}
}

// An ack that lands on a delivery that died meanwhile is done; it is
// listed once (§1.8) and isn't a failure.
func TestAckedAfterDeadListedOnce(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	enqueue(t, s, ack("op-a", "m1"))
	f := &sender{answer: func(bus.Request) Answer {
		return Answer{Kind: Accepted, Data: map[string]any{"acks": []any{map[string]any{"message_id": "m1", "outcome": "dead"}}}}
	}}
	if report, _ := s.Drain(ctx, server, me, nil, f.send); report.Sent != 1 {
		t.Fatal(report)
	}
	if st, _ := s.Status(ctx); st.Dead != 0 || st.Waiting != 0 {
		t.Fatal(st)
	}
	if dead, _ := s.List(ctx, true); len(dead) != 1 || dead[0].Error != ClassAckedAfterDead {
		t.Fatal(dead)
	}
	if dead, _ := s.List(ctx, true); len(dead) != 0 {
		t.Fatal("listed twice", dead)
	}
}

// Bounds: 1000 writes or 8 MiB; 24 h expiry; dead letters kept 7 days.
func TestBoundsAndExpiry(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	big := strings.Repeat("x", 60000)
	n := 0
	for {
		_, err := s.Enqueue(ctx, server, me, send("op-"+string(rune('a'+n%26))+strings.Repeat("z", n/26), big))
		if errors.Is(err, ErrFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	st, _ := s.Status(ctx)
	if st.Bytes > MaxBytes || st.Bytes+60000 < MaxBytes-1000 || n < 100 {
		t.Fatal(n, st)
	}
	s.db.Exec(`DELETE FROM client_outbox`)
	for i := 0; i < MaxItems; i++ {
		s.db.Exec(`INSERT INTO client_outbox(qkey,server,workspace,project,session,instance,principal,credential,operation,operation_id,recipient,acks,request,size,state,created_at,expires_at) VALUES('k','','','','','','','','send','','','','{}',2,'queued',?,?)`, s.at(), s.at()+int64(Expiry))
	}
	if st, _ = s.Status(ctx); !st.Full() {
		t.Fatal(st)
	}
	if _, err := s.Enqueue(ctx, server, me, send("op-x", "x")); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	s.db.Exec(`DELETE FROM client_outbox`)
	enqueue(t, s, send("op-old", "x"))
	c.add(Expiry - time.Second)
	if st, _ = s.Status(ctx); st.Waiting != 1 || st.Oldest != Expiry-time.Second {
		t.Fatal(st)
	}
	c.add(time.Second)
	f := &sender{}
	if report, _ := s.Drain(ctx, server, me, nil, f.send); len(f.sent) != 0 || report.Waiting != 0 {
		t.Fatal("an expired write was sent")
	}
	if st, _ = s.Status(ctx); st.Dead != 1 || !st.Expired {
		t.Fatal(st)
	}
	c.add(deadKeep)
	enqueue(t, s, send("op-new", "x"))
	if dead, _ := s.List(ctx, true); len(dead) != 0 {
		t.Fatal("kept past 7 days", dead)
	}
}

func TestRetryAndDrop(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	q := enqueue(t, s, send("op-1", "x"))
	enqueue(t, s, send("op-2", "y"))
	f := &sender{answer: func(r bus.Request) Answer {
		if r.Args["operation_id"] == "op-1" {
			return Answer{Kind: Refused, Class: "forbidden"}
		}
		return Answer{Kind: Transient}
	}}
	s.Drain(ctx, server, me, nil, f.send)
	if err := s.Retry(ctx, q.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(ctx, q.ID); err == nil {
		t.Fatal("retried a waiting write")
	}
	waiting, _ := s.List(ctx, false)
	if len(waiting) != 2 || waiting[0].ID != q.ID {
		t.Fatal("not at the head", waiting)
	}
	if err := s.Drop(ctx, q.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Drop(ctx, q.ID); err == nil {
		t.Fatal("dropped twice")
	}
	if waiting, _ = s.List(ctx, false); len(waiting) != 1 || waiting[0].OperationID != "op-2" {
		t.Fatal(waiting)
	}
}

// One drainer per queue: a queue another process holds is skipped until
// its lease runs out.
func TestDrainLease(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	enqueue(t, s, send("op-1", "x"))
	key := KeyOf(server, send("", ""))
	s.db.Exec(`INSERT INTO client_outbox_leases(qkey,owner,until) VALUES(?,?,?)`, key.id(), "other", s.at()+int64(leaseFor))
	f := &sender{}
	if s.Drain(ctx, server, me, nil, f.send); len(f.sent) != 0 {
		t.Fatal("sent under another's lease")
	}
	c.add(leaseFor)
	if s.Drain(ctx, server, me, nil, f.send); len(f.sent) != 1 {
		t.Fatal("not sent after the lease ran out")
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM client_outbox_leases`).Scan(&n)
	if n != 0 {
		t.Fatal("lease kept")
	}
}

// The list never shows a body.
func TestListHasNoBody(t *testing.T) {
	s, _ := newStore(t)
	enqueue(t, s, send("op-1", "the secret plan"))
	items, _ := s.List(context.Background(), false)
	if len(items) != 1 || items[0].To != "s2" || items[0].Size == 0 {
		t.Fatal(items)
	}
	for _, field := range []string{items[0].Operation, items[0].OperationID, items[0].To, items[0].Error} {
		if strings.Contains(field, "secret plan") {
			t.Fatal(items)
		}
	}
}

// Only an ack overtakes: a reply that carries the ack keeps its place.
func TestPromoteMovesOnlyAcks(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	enqueue(t, s, send("op-1", "x"))
	enqueue(t, s, reply("op-r", "m3", true))
	key := KeyOf(server, send("", ""))
	if err := s.Promote(ctx, key, "m3"); err != nil {
		t.Fatal(err)
	}
	f := &sender{}
	s.Drain(ctx, server, me, &key, f.send)
	if strings.Join(f.ops(), ",") != "op-1,op-r" {
		t.Fatal(f.ops())
	}
}

// An ack queues in either shape the bus takes: decoded JSON, as the MCP
// tool sends it, and a Go caller's []map[string]any; both are found as
// pending acks.
func TestAckShapesQueue(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(`{"acks":[{"message_id":"m1","lease_id":"lse_1"}],"operation_id":"op-a"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["instance"] = "i1"
	maps := map[string]any{"instance": "i1", "operation_id": "op-b", "acks": []map[string]any{{"message_id": "m2"}}}
	for _, args := range []map[string]any{decoded, maps} {
		r := bus.Request{Operation: "ack", Workspace: "w", Project: "p", Session: "s1", Args: args}
		if !Queueable(r) {
			t.Fatal("not queueable", args)
		}
		enqueue(t, s, r)
	}
	key := KeyOf(server, send("", ""))
	for _, id := range []string{"m1", "m2"} {
		if pending, err := s.PendingAck(ctx, key, id); err != nil || !pending {
			t.Fatal(id, pending, err)
		}
	}
	for _, empty := range []any{nil, []any{}, []map[string]any{}, []any{"m1"}, "m1"} {
		if Queueable(bus.Request{Operation: "ack", Args: map[string]any{"acks": empty, "operation_id": "op"}}) {
			t.Fatal("queued", empty)
		}
	}
}
