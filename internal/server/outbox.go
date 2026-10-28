package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"regexp"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// The write outbox (live/LINEAR-QUEUE-DESIGN.md §4): a client that sends
// accept_queued has a write that can't go now queued instead of refused,
// and the server's worker sends it later through the same path, as the
// same person and session, with the same checks.

const (
	// outboxLease outlasts the send (appWriteTimeout, 60 s) and the
	// ledger's in-progress window (75 s), so a reclaimed write is answered
	// by the ledger, never sent twice (R4).
	outboxLease = 90 * time.Second
	// outboxExpiry is how long a write may wait: the hourly cap can hold one
	// for up to an hour, with margin. It is never sent after it.
	outboxExpiry = 3 * time.Hour
	outboxEvery  = 5 * time.Second
	// outboxBatch is the most writes one cycle sends.
	outboxBatch = 20
	// outboxBusy is how soon a write another call is still sending is tried
	// again.
	outboxBusy = 30 * time.Second
)

// outboxLadder spaces the tries of a write Linear kept refusing.
var outboxLadder = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// Reasons a write is queued.
const (
	reasonRateLimit = "linear_rate_limit"
	reasonCap       = "project_hourly_cap"
	reasonOrdered   = "ordered_behind"
)

// outboxRequest is a queued write as the server accepted it: who asked,
// where, and the write itself. It is the server's own record and never
// shown (R3).
type outboxRequest struct {
	Principal    string         `json:"principal"`
	Issuer       string         `json:"issuer"`
	Subject      string         `json:"subject"`
	HostedDomain string         `json:"hosted_domain,omitempty"`
	Workspace    string         `json:"workspace"`
	Project      string         `json:"project"`
	Session      string         `json:"session,omitempty"`
	Instance     string         `json:"instance,omitempty"`
	Operation    string         `json:"operation"`
	Args         map[string]any `json:"args"`
	// AgentMarker is the client's agent_marker (intake attribution, A2),
	// so the deferred send records it as the client sent it; nil for a
	// client that sent none, which is never credited.
	AgentMarker *string `json:"agent_marker,omitempty"`
}

func (q outboxRequest) identity() bus.Identity {
	return bus.Identity{Principal: q.Principal, Issuer: q.Issuer, Subject: q.Subject, HostedDomain: q.HostedDomain}
}

// queuedMarker is the agent_marker a queued write keeps: nil when the
// client sent none.
func queuedMarker(args map[string]any) *string {
	m, ok := args["agent_marker"].(string)
	if !ok {
		return nil
	}
	return &m
}

func (q outboxRequest) request() bus.Request {
	r := bus.Request{Operation: "linear_write", Workspace: q.Workspace, Project: q.Project, Session: q.Session, Args: map[string]any{"instance": q.Instance, "operation": q.Operation, "args": q.Args}}
	if q.AgentMarker != nil {
		r.Args["agent_marker"] = *q.AgentMarker
	}
	return r
}

// orderKey is what a write is ordered by (§4.3): the issue it changes, the
// from issue of a relation, and a new issue's parent, else its project or
// team.
func orderKey(operation string, args map[string]any) (key, issue string) {
	s := func(k string) string { v, _ := args[k].(string); return v }
	switch operation {
	case "comment", "update":
		return "issue:" + s("id"), s("id")
	case "relation_add", "relation_remove":
		return "issue:" + s("from_id"), s("from_id")
	}
	switch {
	case s("parent_id") != "":
		return "issue:" + s("parent_id"), s("parent_id")
	case s("project_id") != "":
		return "project:" + s("project_id"), ""
	}
	return "team:" + s("team_id"), ""
}

// writeDigest is a write's operation and arguments, to refuse an
// operation_id reused for a different write.
func writeDigest(operation string, args map[string]any) string {
	raw, _ := json.Marshal([]any{operation, args})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (w *AppWriter) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// access is the caller's project and session as the bus resolves them,
// the same the ledger keys the write by.
func (w *AppWriter) access(ctx context.Context, identity bus.Identity, r bus.Request) (bus.TaskboardAccess, error) {
	instance, _ := r.Args["instance"].(string)
	out, err := w.Store.ExecuteAsIdentity(ctx, identity, bus.Request{Operation: "taskboard_access", Workspace: r.Workspace, Project: r.Project, Session: r.Session, Args: map[string]any{"instance": instance, "ledger": 1}})
	if err != nil {
		return bus.TaskboardAccess{}, err
	}
	raw, _ := json.Marshal(out.Data)
	var a bus.TaskboardAccess
	if err = json.Unmarshal(raw, &a); err != nil || a.ProjectID == "" {
		return a, errors.New("taskboard access unreadable")
	}
	return a, nil
}

// queueable runs a write on a server with the outbox. It keeps an issue's
// writes in order and the project's hourly cap (§5), which agent writes now
// count against. For a client that accepts queueing (queue), a write that
// can't go now is queued; an older client is refused as before, with
// rate_limited and when to retry. A write already queued answers from the
// outbox.
func (w *AppWriter) queueable(parent context.Context, identity bus.Identity, r bus.Request, operation string, args map[string]any, queue bool) (bus.Result, error) {
	access, err := w.access(parent, identity, r)
	if err != nil || !access.CanWrite {
		// The direct path gives the caller its own refusal.
		return w.answer(w.send(parent, identity, r, operation, args, ""))
	}
	unavailable := bus.PublicError("unavailable", "the Linear write queue is unavailable; nothing was sent")
	opID, _ := args["operation_id"].(string)
	digest := writeDigest(operation, args)
	key, issue := orderKey(operation, args)
	existing, found, err := w.Store.OutboxGet(parent, access.ProjectID, access.SessionID, opID)
	if err != nil {
		return bus.Result{}, unavailable
	}
	switch {
	case found && existing.Digest != digest:
		return bus.Result{}, bus.PublicError("invalid", "operation_id was already used for a different write; use a new operation_id")
	case found && existing.State != bus.OutboxDone:
		return queuedAnswer(existing), nil
	case found:
		// Sent: the ledger replays its receipt.
		return w.answer(w.send(parent, identity, r, operation, args, ""))
	}
	wait := func(reason string, next time.Time) (bus.Result, error) {
		if !queue {
			return bus.Result{}, bus.PublicError("rate_limited", waitMessage(reason, next))
		}
		instance, _ := r.Args["instance"].(string)
		raw, _ := json.Marshal(outboxRequest{AgentMarker: queuedMarker(r.Args), Principal: identity.Principal, Issuer: identity.Issuer, Subject: identity.Subject, HostedDomain: identity.HostedDomain, Workspace: r.Workspace, Project: r.Project, Session: r.Session, Instance: instance, Operation: operation, Args: args})
		now := w.now()
		q, _, err := w.Store.OutboxEnqueue(parent, bus.OutboxWrite{ProjectID: access.ProjectID, SessionID: access.SessionID, Principal: identity.Principal, OperationID: opID, Digest: digest, Request: string(raw), Operation: operation, Issue: issue, OrderKey: key, Reason: reason, NextAttemptAt: next, CreatedAt: now, ExpiresAt: now.Add(outboxExpiry)})
		if err != nil {
			if _, public := bus.PublicMessage(err); public {
				return bus.Result{}, err
			}
			return bus.Result{}, unavailable
		}
		return queuedAnswer(q), nil
	}
	// Not atomic with the send below: a write that checks while another on
	// the same issue is being queued may go first. Writes sent at the same
	// moment had no order to keep; once one is queued, every later one
	// queues behind it.
	behind, err := w.Store.OutboxBehind(parent, access.ProjectID, key)
	if err != nil {
		return bus.Result{}, unavailable
	}
	if behind > 0 {
		return wait(reasonOrdered, w.now())
	}
	room, _, _, resetAt, err := w.Store.LinearBudgetRoom(parent, access.ProjectID)
	if err != nil {
		return bus.Result{}, unavailable
	}
	if !room {
		return wait(reasonCap, resetAt)
	}
	data, err := w.send(parent, identity, r, operation, args, access.ProjectID)
	if limited, ok := linearboard.RateLimitedNotSent(err); ok && queue {
		return wait(reasonRateLimit, w.now().Add(max(limited, time.Second)))
	}
	return w.answer(data, err)
}

// waitMessage is an older client's refusal for a write that can't go now:
// nothing was sent, and when to retry the same operation_id.
func waitMessage(reason string, next time.Time) string {
	why := "earlier writes on this issue are still queued"
	if reason == reasonCap {
		why = "this project's Linear budget for the hour is spent"
	}
	return why + "; nothing was sent; retry the same operation_id at " + next.UTC().Format(time.RFC3339)
}

// queuedAnswer is a queued write's answer: typed fields, never its text.
func queuedAnswer(q bus.OutboxWrite) bus.Result {
	out := q.View()
	out["outcome"] = q.State
	if q.State == bus.OutboxQueued || q.State == bus.OutboxSending {
		out["outcome"] = "queued"
		out["estimated_at"] = q.NextAttemptAt.UTC().Format(time.RFC3339)
	}
	return bus.Result{Data: out}
}

// writeStatus lists the caller's queued and recent writes (this session's,
// or for a sessionless caller their own), or one by operation_id: typed
// fields only, never a write's text (R3).
func (w *AppWriter) writeStatus(parent context.Context, identity bus.Identity, r bus.Request) (bus.Result, error) {
	for k := range r.Args {
		if k != "instance" && k != "operation_id" {
			return bus.Result{}, appRefusal("linear_write_status takes instance and operation_id only")
		}
	}
	if w.Store == nil || !w.Store.OutboxAvailable() {
		return bus.Result{}, appRefusal("this server does not queue Linear writes")
	}
	if err := w.member(parent, identity, r.Workspace); err != nil {
		return bus.Result{}, err
	}
	access, err := w.access(parent, identity, r)
	if err != nil {
		return bus.Result{}, err
	}
	writes, err := w.Store.OutboxList(parent, access.ProjectID, access.SessionID, identity.Principal, 100)
	if err != nil {
		return bus.Result{}, bus.PublicError("unavailable", "the Linear write queue is unavailable")
	}
	op, _ := r.Args["operation_id"].(string)
	out := []any{}
	for _, q := range writes {
		if op == "" || q.OperationID == op {
			out = append(out, q.View())
		}
	}
	if op != "" && len(out) == 0 {
		return bus.Result{}, bus.PublicError("not_found", "no queued write with that operation_id for this session; a write that was never queued has its outcome in its own answer")
	}
	return bus.Result{Data: map[string]any{"writes": out}}, nil
}

// runOutbox sends queued writes every interval until ctx ends.
func (w *AppWriter) runOutbox(ctx context.Context, every time.Duration) {
	var b [8]byte
	rand.Read(b[:])
	owner := "outbox-" + hex.EncodeToString(b[:])
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		w.outboxCycle(ctx, owner)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// outboxCycle reclaims run-out leases, expires stale writes, sends the
// due head of each issue's queue, and sends owed notices.
func (w *AppWriter) outboxCycle(ctx context.Context, owner string) {
	now := w.now()
	if _, err := w.Store.OutboxReclaim(ctx, now); err != nil {
		log.Printf("clawbridge: linear outbox: reclaim failed")
		return
	}
	w.Store.OutboxExpire(ctx, now)
	w.Store.OutboxPrune(ctx, now)
	// One write at a time, each leased just before its send, so no write is
	// sent after its lease ran out and another replica reclaimed it.
	for range outboxBatch {
		if ctx.Err() != nil {
			return
		}
		leased, err := w.Store.OutboxLease(ctx, owner, w.now(), outboxLease, 1)
		if err != nil {
			log.Printf("clawbridge: linear outbox: lease failed")
			break
		}
		if len(leased) == 0 {
			break
		}
		w.deliver(ctx, owner, leased[0])
	}
	ids, _ := w.Store.OutboxUnnotified(ctx, 50)
	for _, id := range ids {
		w.notify(ctx, id)
	}
}

// deliver sends one leased write as the person and session that asked,
// through the direct path: every check runs again (R1), and the ledger
// answers a write that was already sent.
func (w *AppWriter) deliver(ctx context.Context, owner string, q bus.OutboxWrite) {
	var req outboxRequest
	if err := json.Unmarshal([]byte(q.Request), &req); err != nil {
		w.Store.OutboxSettle(ctx, q.ID, owner, bus.OutboxFailed, "unreadable", "", w.now())
		return
	}
	// The person who asked proves nothing afresh: the write goes only while
	// their device is live or their account still bound (R1), and never
	// binds an account again.
	switch live, err := w.Store.IdentityLive(ctx, req.identity()); {
	case err != nil:
		w.Store.OutboxRetry(ctx, q.ID, owner, q.Reason, w.now().Add(time.Minute), false, w.now())
		return
	case !live:
		w.Store.OutboxSettle(ctx, q.ID, owner, bus.OutboxFailed, "identity_revoked", "", w.now())
		w.notify(ctx, q.ID)
		return
	}
	ctx = bus.WithoutBinding(ctx)
	if room, _, _, resetAt, err := w.Store.LinearBudgetRoom(ctx, q.ProjectID); err != nil || !room {
		// Not sent: the cap holds it to the next hour, without a try.
		next := resetAt
		if err != nil {
			next = w.now().Add(time.Minute)
		}
		w.Store.OutboxRetry(ctx, q.ID, owner, reasonCap, next, false, w.now())
		return
	}
	data, err := w.send(ctx, req.identity(), req.request(), req.Operation, req.Args, q.ProjectID)
	now := w.now()
	var receipt *linearboard.ReceiptError
	switch wait, limited := linearboard.RateLimitedNotSent(err); {
	case err == nil:
		w.Store.OutboxSettle(ctx, q.ID, owner, bus.OutboxDone, "", receiptSummary(req.Operation, data, ""), now)
	case data != nil && errors.As(err, &receipt):
		w.Store.OutboxSettle(ctx, q.ID, owner, bus.OutboxDone, "", receiptSummary(req.Operation, data, receipt.Outcome), now)
	case limited:
		step := outboxLadder[min(q.Attempts, len(outboxLadder)-1)]
		w.Store.OutboxRetry(ctx, q.ID, owner, reasonRateLimit, now.Add(max(wait, step)), true, now)
	case linearboard.IsInProgress(err):
		w.Store.OutboxRetry(ctx, q.ID, owner, q.Reason, now.Add(outboxBusy), false, now)
	case linearboard.IsUncertain(err):
		w.Store.OutboxSettle(ctx, q.ID, owner, bus.OutboxUncertain, "uncertain", "", now)
	default:
		w.Store.OutboxSettle(ctx, q.ID, owner, bus.OutboxFailed, publicReason(err), "", now)
	}
	w.notify(ctx, q.ID)
}

// notify tells the session how its queued write ended.
func (w *AppWriter) notify(ctx context.Context, id string) {
	if err := w.Store.OutboxNotify(ctx, id, w.now()); err != nil {
		log.Printf("clawbridge: linear outbox: notice failed")
	}
}

// safeID admits an ID or outcome word into a summary.
var safeID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// receiptSummary is a receipt's typed fields: its kind (else the
// operation) and ID.
func receiptSummary(operation string, data any, outcome string) string {
	raw, _ := json.Marshal(data)
	var r linearboard.ReceiptV2
	json.Unmarshal(raw, &r)
	s := r.Kind
	if !safeID.MatchString(s) {
		s = operation
	}
	for _, v := range []string{r.Identifier, r.ID} {
		if v != "" && safeID.MatchString(v) {
			s += " " + v
			break
		}
	}
	if outcome != "" && safeID.MatchString(outcome) {
		s += " (" + outcome + ")"
	}
	return s
}

// publicReason is a refusal's message as the caller would see it: the
// board's own text, never Linear's.
func publicReason(err error) string {
	if message, public := bus.PublicMessage(writeError(err)); public {
		return message
	}
	return "refused"
}
