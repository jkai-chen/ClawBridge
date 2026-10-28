package bus

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// Pins the client outbox rests on (live/ZERO-LOSS-DEPLOY-DESIGN.md §1.8,
// §1.9): an ack queued past its lease still lands.

// An ack after the message was leased again answers acked: the work was
// done. The new lease's own ack then answers already_acked, and the message
// is never offered again. nack stays the current lease's only (stale).
func TestStaleLeaseAckIsAccepted(t *testing.T) {
	h := newHarness(t)
	move := skew(t)
	id := h.send("pin")["id"]
	first := h.lease("b", nil)
	move(11 * time.Minute)
	again := h.lease("b", nil)
	if len(again) != 1 || again[0]["lease_id"] == first[0]["lease_id"] {
		t.Fatal("not leased again:", again)
	}
	other := h.send("pin-nack")["id"]
	held := h.lease("b", nil)
	move(11 * time.Minute)
	if regained := h.lease("b", nil); len(regained) != 2 {
		t.Fatal(regained)
	}
	if o := outcomes(h.call("ack", "b", ackOne(id, first[0]["lease_id"])), "acks"); !slices.Equal(o, []string{"acked"}) {
		t.Fatal("a stale lease's ack:", o)
	}
	if o := outcomes(h.call("ack", "b", ackOne(id, again[0]["lease_id"])), "acks"); !slices.Equal(o, []string{"already_acked"}) {
		t.Fatal("the new lease's ack:", o)
	}
	nack := h.call("nack", "b", map[string]any{"message_id": other, "lease_id": held[0]["lease_id"]}).(map[string]any)
	if nack["outcome"] != "stale" {
		t.Fatal("nack of a superseded lease:", nack)
	}
	move(11 * time.Minute)
	for _, m := range h.lease("b", nil) {
		if m["id"] == id {
			t.Fatal("an acked message was offered again")
		}
	}
}

// withAck's outcomes: a reply whose ack is acked, already acked or dead
// lands with the outcome; one whose ack names a message not in the queue
// is refused whole. The same reply sent again without the ack (the ack
// isn't in its digest) lands once, under the same operation ID (D2').
func TestReplyAckOutcomes(t *testing.T) {
	h := newHarness(t)
	move := skew(t)
	h.s.queue.Attempts = 1
	reply := func(to any, op string, ack map[string]any) map[string]any {
		a := map[string]any{"message_id": to, "body": "answer", "operation_id": op}
		if ack != nil {
			a["ack"] = ack
		}
		return h.call("reply", "b", a).(map[string]any)
	}
	acked := h.send("o-acked")["id"]
	l := h.lease("b", nil)[0]["lease_id"]
	if out := reply(acked, "r-acked", map[string]any{"message_id": acked, "lease_id": l}); out["ack"].(map[string]any)["outcome"] != "acked" {
		t.Fatal(out)
	}
	if out := reply(acked, "r-acked-2", map[string]any{"message_id": acked}); out["ack"].(map[string]any)["outcome"] != "already_acked" {
		t.Fatal(out)
	}
	dead := h.send("o-dead")["id"]
	h.lease("b", nil)
	move(11 * time.Minute)
	if _, err := h.s.SweepDeliveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	if out := reply(dead, "r-dead", map[string]any{"message_id": dead}); out["ack"].(map[string]any)["outcome"] != "dead" {
		t.Fatal(out)
	}
	asked := h.send("o-missing")["id"]
	missing := map[string]any{"message_id": asked, "body": "answer", "operation_id": "r-missing", "ack": map[string]any{"message_id": "msg_not_queued"}}
	h.fail("reply", "b", missing)
	delete(missing, "ack")
	first := h.call("reply", "b", missing).(map[string]any)
	if first["ack"] != nil {
		t.Fatal(first)
	}
	if replay := h.call("reply", "b", missing).(map[string]any); replay["id"] != first["id"] {
		t.Fatal("the bare resend wasn't deduplicated:", replay["id"], first["id"])
	}
	var n int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE sender_id=? AND operation_id='r-missing'`, h.sessionID("b")).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

// An old instance's reply and its ack are refused as a whole by Execute's
// instance fence: neither lands, and the new instance's reply does.
func TestOldInstanceReplyWithAckRefusedWhole(t *testing.T) {
	h := newHarness(t)
	id := h.send("o-instance")["id"]
	l := h.lease("b", nil)[0]["lease_id"]
	// The session's instance changes (a restart took it over).
	if _, err := h.s.db.Exec(`UPDATE sessions SET instance='b-new' WHERE id=?`, h.sessionID("b")); err != nil {
		t.Fatal(err)
	}
	old := map[string]any{"message_id": id, "body": "answer", "operation_id": "r-old", "ack": map[string]any{"message_id": id, "lease_id": l}}
	h.fail("reply", "b", old)
	var n int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE operation_id='r-old'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("the old instance's reply landed:", n, err)
	}
	fresh := map[string]any{"message_id": id, "body": "answer", "operation_id": fmt.Sprintf("r-new-%v", id), "instance": "b-new", "ack": map[string]any{"message_id": id}}
	if out := h.call("reply", "b", fresh).(map[string]any); out["ack"].(map[string]any)["outcome"] != "acked" {
		t.Fatal(out)
	}
}
