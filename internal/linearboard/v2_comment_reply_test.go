package linearboard

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

const (
	replyParent = "c5000000-0000-4000-8000-000000000005"
	replyNew    = "c9000000-0000-4000-8000-000000000009"
)

// replier is a V2 provider that can prove a reply's parent and post it.
type replier struct {
	*providerV2
	parents map[string]bool
	// nested maps a parent that is itself a reply to the comment it answers.
	nested  map[string]string
	proofs  []string
	replies []string
	// receiptParent overrides the parent the reply receipt names.
	receiptParent *string
	// replyBodySHA makes the receipt carry the posted body's SHA-256, as
	// the native provider does.
	replyBodySHA bool
}

func (p *replier) IssueHasComment(_ context.Context, issue, _, comment string) (bool, string, error) {
	p.proofs = append(p.proofs, issue+"|"+comment)
	return p.parents[comment], p.nested[comment], nil
}

func (p *replier) ReplyOnIssue(_ context.Context, issue, _, parent, body string) (linearprovider.Receipt, error) {
	p.replies = append(p.replies, parent+"|"+body)
	if p.receiptParent != nil {
		parent = *p.receiptParent
	}
	receipt := linearprovider.Receipt{ID: replyNew, IssueID: issue, ParentID: parent}
	if p.replyBodySHA {
		receipt.BodySHA256 = descriptionSHA256(body)
	}
	return receipt, nil
}

func replyTestV2(t *testing.T) (*Service, *replier, func(op, parent string) (any, error)) {
	t.Helper()
	s, a, p, scope := testV2(t)
	// The session is addressed by name, so the marker shows it.
	a.actor = "ses_0001"
	p.caps.CommentReply = true
	r := &replier{providerV2: p, parents: map[string]bool{replyParent: true}}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return r, nil }
	reply := func(op, parent string) (any, error) {
		args := map[string]any{"id": issueID, "body": "an answer", "operation_id": op}
		if parent != "" {
			args["parent_id"] = parent
		}
		return s.ExecuteV2(context.Background(), scope, "comment", args)
	}
	return s, r, reply
}

func TestLinearV2ReplyProvesTheParentAndNamesItInTheReceipt(t *testing.T) {
	_, r, reply := replyTestV2(t)
	got, e := reply("reply-1", replyParent)
	receipt, ok := got.(ReceiptV2)
	if e != nil || !ok || receipt.Kind != "comment" || receipt.ID != replyNew || receipt.IssueID != issueID || receipt.ParentID != replyParent {
		t.Fatal("reply not accepted with its parent", got, e)
	}
	if len(r.proofs) != 1 || r.proofs[0] != issueID+"|"+replyParent || len(r.replies) != 1 || r.replies[0] != replyParent+"|an answer\n\n— 🤖 ClawBridge agent · session ses" || r.writes != 0 {
		t.Fatal("parent not proven on this issue before one reply", r.proofs, r.replies, r.writes)
	}
	// The same operation is answered from the ledger, sending nothing.
	again, e := reply("reply-1", replyParent)
	if saved, _ := again.(map[string]any); e != nil || saved["id"] != replyNew || saved["parent_id"] != replyParent || len(r.replies) != 1 || len(r.proofs) != 1 {
		t.Fatal("retry was not idempotent", again, e, r.replies)
	}
	// The operation ID is bound to the parent it was first used with.
	if _, e := reply("reply-1", commentOther); e == nil || len(r.replies) != 1 {
		t.Fatal("operation_id reused for another parent", e, r.replies)
	}
	// A plain comment never proves a parent or posts a reply.
	if _, e := reply("plain-1", ""); e != nil || len(r.proofs) != 1 || len(r.replies) != 1 || r.writes != 1 {
		t.Fatal("plain comment", e, r.proofs, r.replies, r.writes)
	}
}

const commentOther = "c6000000-0000-4000-8000-000000000006"

// A parent that is not a comment on this issue is refused before dispatch:
// nothing is sent and the operation ID stays free.
func TestLinearV2ReplyToAForeignParentIsRefusedBeforeDispatch(t *testing.T) {
	_, r, reply := replyTestV2(t)
	if _, e := reply("reply-2", commentOther); e == nil || !strings.Contains(e.Error(), "nothing was sent") || len(r.replies) != 0 {
		t.Fatal("foreign parent not refused before dispatch", e, r.replies)
	}
	r.parents[commentOther] = true
	if _, e := reply("reply-2", commentOther); e != nil || len(r.replies) != 1 {
		t.Fatal("the refused operation left a ledger record", e, r.replies)
	}
}

func TestLinearV2ReplyRefusals(t *testing.T) {
	_, r, reply := replyTestV2(t)
	r.caps.CommentReply = false
	if _, e := reply("reply-3", replyParent); e == nil || !strings.Contains(e.Error(), "threaded reply") || len(r.proofs) != 0 || len(r.replies) != 0 {
		t.Fatal("reply without the capability", e, r.proofs, r.replies)
	}
	r.caps.CommentReply = true
	_, _, _, scope := testV2(t)
	s, _, _ := replyTestV2(t)
	if _, e := s.ExecuteV2(context.Background(), scope, "comment", map[string]any{"id": issueID, "body": "x", "operation_id": "reply-4", "parent_id": nil}); e == nil || !strings.Contains(e.Error(), "parent comment UUID") {
		t.Fatal("null parent accepted", e)
	}
	for _, bad := range []any{"SYN-6", "not-a-uuid", 7} {
		if _, e := s.ExecuteV2(context.Background(), scope, "comment", map[string]any{"id": issueID, "body": "x", "operation_id": "reply-5", "parent_id": bad}); e == nil {
			t.Fatal("parent accepted", bad)
		}
	}
	// A receipt naming another parent is not accepted: the reply was sent,
	// so the outcome is uncertain.
	other := commentOther
	r.receiptParent = &other
	if _, e := reply("reply-6", replyParent); e == nil || !strings.Contains(e.Error(), "uncertain") {
		t.Fatal("receipt with another parent accepted", e)
	}
}

// Every comment ClawBridge posts ends with the agent marker naming the
// session, on the V1 path and on both V2 paths; the ledger keeps the
// caller's body, and the receipt's body_sha256 covers the posted body.
func TestEveryPostedCommentCarriesTheAgentMarker(t *testing.T) {
	const marked = "hello\n\n— 🤖 ClawBridge agent · session ses"
	// The V1 fake's session is addressed by its ID, which never reaches
	// Linear: the marker names no session.
	s, _, p, scope := testService(t)
	if _, e := s.Write(context.Background(), scope, WriteInput{Operation: "comment", OperationID: "v1-comment", IssueID: issueID, Body: "hello"}); e != nil || len(p.bodies) != 1 || p.bodies[0] != "hello\n\n— 🤖 ClawBridge agent" {
		t.Fatal("V1 comment not marked, or marked with the session ID", e, p.bodies)
	}
	sv2, a2, p2, scope2 := testV2(t)
	a2.actor = "ses_0001"
	_, e := sv2.WriteV2(context.Background(), scope2, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "v2-comment", IssueID: issueID, Body: "hello"}})
	if e != nil || len(p2.bodies) != 1 || p2.bodies[0] != marked {
		t.Fatal("V2 comment not marked", e, p2.bodies)
	}
	if again, e := sv2.WriteV2(context.Background(), scope2, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "v2-comment", IssueID: issueID, Body: "hello"}}); e != nil || len(p2.bodies) != 1 || again == nil {
		t.Fatal("a retry with the caller's body did not match the ledger", e, p2.bodies)
	}
	_, r, reply := replyTestV2(t)
	r.replyBodySHA = true
	got, e := reply("reply-marker", replyParent)
	if receipt, ok := got.(ReceiptV2); e != nil || !ok || receipt.BodySHA256 != descriptionSHA256("an answer\n\n— 🤖 ClawBridge agent · session ses") {
		t.Fatal("reply receipt does not cover the posted body", got, e)
	}
	// The marker's room is reserved from the caller's body.
	long := strings.Repeat("x", linearprovider.MaxBodyBytes-linearprovider.CommentMarkerReserve+1)
	if _, e := sv2.WriteV2(context.Background(), scope2, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "v2-long", IssueID: issueID, Body: long}}); e == nil || len(p2.bodies) != 1 {
		t.Fatal("a body without room for the marker was posted", e)
	}
	if _, e := s.Write(context.Background(), scope, WriteInput{Operation: "comment", OperationID: "v1-long", IssueID: issueID, Body: long}); e == nil || len(p.bodies) != 1 {
		t.Fatal("a V1 body without room for the marker was posted", e)
	}
}

// Linear threads are one level deep and Linear re-parents a reply to a
// reply onto the thread root, so such a reply is sent to the root, verified
// against it, and reports both the root and the requested parent.
func TestLinearV2ReplyToAReplyGoesToTheThreadRoot(t *testing.T) {
	_, r, reply := replyTestV2(t)
	r.parents[commentOther] = true
	r.nested = map[string]string{commentOther: replyParent}
	got, e := reply("reply-7", commentOther)
	receipt, ok := got.(ReceiptV2)
	if e != nil || !ok || receipt.ParentID != replyParent || receipt.RequestedParentID != commentOther || len(r.replies) != 1 || !strings.HasPrefix(r.replies[0], replyParent+"|") {
		t.Fatal("reply to a reply not sent to and verified against the root", got, e, r.replies)
	}
	// A direct reply names no requested parent.
	if got, e = reply("reply-8", replyParent); e != nil || got.(ReceiptV2).RequestedParentID != "" {
		t.Fatal("direct reply", got, e)
	}
	// A root that is not a UUID is refused before dispatch.
	r.nested[commentOther] = "?"
	if _, e = reply("reply-9", commentOther); e == nil || !strings.Contains(e.Error(), "nothing was sent") || len(r.replies) != 2 {
		t.Fatal("unreadable root not refused", e, r.replies)
	}
}
