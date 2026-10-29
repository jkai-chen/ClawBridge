package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/outbox"
)

// The client outbox (ZERO-LOSS-DEPLOY §1): a queueable write that can't
// reach the server is retried in process for a while, then kept in the
// profile's outbox and sent, in order and exactly once, when the server is
// back.

// queueAfter is how long a queueable write is retried in process before
// it waits in the outbox (§1.2).
var queueAfter = 15 * time.Second

// drainFirst bounds the drain a new write waits for when its queue already
// holds writes: it never overtakes them.
var drainFirst = 2 * time.Second

// outboxNotice receives one line when a write is queued or a stored write
// is sent; the CLI prints it, an MCP server leaves it unset.
var outboxNotice func(string)

// SetOutboxNotice sets (or, with nil, clears) the receiver of this
// process's outbox lines.
func SetOutboxNotice(f func(string)) { outboxNotice = f }

// opened is every outbox this process opened, for CloseOutboxes.
var opened struct {
	sync.Mutex
	stores []*outbox.Store
}

// CloseOutboxes closes every outbox this process opened: a command's end,
// so its file isn't held open after it (Windows can't remove a file held
// open). A write after it fails as without an outbox.
func CloseOutboxes() {
	opened.Lock()
	defer opened.Unlock()
	for _, s := range opened.stores {
		s.Close()
	}
	opened.stores = nil
}

// outbox is the profile's outbox, or nil: switched off, no profile cache
// to sit beside, or unopenable (a write then fails fast, as before).
func (c *Client) outbox() *outbox.Store {
	if outbox.Disabled() || c.config.CachePath == "" || c.config.Principal == "" {
		return nil
	}
	c.outboxOnce.Do(func() {
		s, err := outbox.Open(outbox.PathFor(c.config.CachePath))
		if err != nil {
			fmt.Fprintln(os.Stderr, "clawbridge: the local outbox can't be opened; writes that can't reach the server fail as before:", err)
			return
		}
		c.outboxStore.Store(s)
		opened.Lock()
		opened.stores = append(opened.stores, s)
		opened.Unlock()
	})
	return c.outboxStore.Load()
}

// Outbox is the profile's outbox, for the outbox commands and doctor; nil
// when it is off.
func (c *Client) Outbox() *outbox.Store { return c.outbox() }

// OutboxKicks is signalled when a call succeeds while writes may wait: a
// drainer drains then rather than at its next tick.
func (c *Client) OutboxKicks() <-chan struct{} { return c.outboxKick }

// Server is the client's server URL.
func (c *Client) Server() string { return c.config.URL }

func (c *Client) scope() outbox.Scope {
	return outbox.Scope{Principal: c.config.Principal, Credential: c.config.CredentialSource}
}

// DrainOutbox sends this server's waiting writes for up to budget.
func (c *Client) DrainOutbox(ctx context.Context, budget time.Duration) (outbox.Report, error) {
	ob := c.outbox()
	if ob == nil {
		return outbox.Report{}, nil
	}
	return c.drain(ctx, ob, nil, budget)
}

func (c *Client) drain(ctx context.Context, ob *outbox.Store, only *outbox.Key, budget time.Duration) (outbox.Report, error) {
	bounded, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	report, err := ob.Drain(bounded, c.config.URL, c.scope(), only, c.attempt)
	if report.Sent > 0 && outboxNotice != nil {
		outboxNotice(fmt.Sprintf("clawbridge: sent %d queued write(s) from this machine's outbox; %d still waiting", report.Sent, report.Waiting))
	}
	return report, err
}

// PromoteAck moves a queued ack of messageID to the head of its queue and
// drains that queue briefly: the message is being offered again (§1.8).
// queued says whether an ack of it was waiting; pending, whether one still
// is after the drain.
func (c *Client) PromoteAck(ctx context.Context, r bus.Request, messageID string) (queued, pending bool, err error) {
	ob := c.outbox()
	if ob == nil {
		return false, false, nil
	}
	key := outbox.KeyOf(c.config.URL, r)
	if queued, err = ob.PendingAck(ctx, key, messageID); err != nil || !queued {
		return false, false, err
	}
	if err = ob.Promote(ctx, key, messageID); err != nil {
		return true, true, err
	}
	if _, err = c.drain(ctx, ob, &key, drainFirst); err != nil {
		return true, true, err
	}
	pending, err = ob.PendingAck(ctx, key, messageID)
	return true, pending || err != nil, err
}

// attempt is the outbox's sender: one call with fresh credentials.
func (c *Client) attempt(ctx context.Context, r bus.Request) outbox.Answer {
	result, unavailable, err := c.call(ctx, r)
	return classify(ctx, result, unavailable, err)
}

// classify sorts a call's outcome for the outbox (§1.2, §1.6, §2.4).
func classify(ctx context.Context, result bus.Result, unavailable bool, err error) outbox.Answer {
	var httpErr *HTTPError
	switch {
	case err == nil:
		return outbox.Answer{Kind: outbox.Accepted, Data: result.Data}
	case IsCredentialFailure(err):
		return outbox.Answer{Kind: outbox.NoCredential, Class: "unauthenticated"}
	case errors.As(err, &httpErr) && httpErr.Code == "maintenance":
		return outbox.Answer{Kind: outbox.Maintenance, Class: "maintenance", RetryAfter: httpErr.RetryAfter}
	case unavailable || (ctx.Err() != nil && !errors.As(err, &httpErr)):
		// Unreachable, a 502/503/504, or the request outlived its
		// deadline: the outcome may be uncertain, and the identical
		// replay is exactly-once.
		answer := outbox.Answer{Kind: outbox.Transient, Class: "unavailable"}
		if httpErr != nil {
			answer.RetryAfter = httpErr.RetryAfter
		}
		return answer
	case errors.As(err, &httpErr):
		class := httpErr.Code
		if strings.Contains(httpErr.Message, instanceMismatch) {
			class = outbox.ClassInstance
		}
		return outbox.Answer{Kind: outbox.Refused, Class: class}
	}
	return outbox.Answer{Kind: outbox.Refused, Class: "invalid_response"}
}

// instanceMismatch is the server's refusal of an old instance (Execute's
// fence): a queued write from before the session re-registered.
const instanceMismatch = "session instance mismatch"

// executeQueueable sends a write that may wait in the outbox: behind any
// write already waiting in its queue; else tried for up to queueAfter;
// then stored, answering queued_local (§1.2).
func (c *Client) executeQueueable(ctx context.Context, ob *outbox.Store, r bus.Request) (bus.Result, error) {
	r = withOperationID(r)
	key := outbox.KeyOf(c.config.URL, r)
	if n, err := ob.Waiting(ctx, key); err == nil && n > 0 {
		c.drain(ctx, ob, &key, drainFirst)
		if n, err = ob.Waiting(ctx, key); err == nil && n > 0 {
			return c.queue(ctx, ob, r, "the server hasn't taken this session's earlier writes yet")
		}
	}
	deadline := time.Now().Add(queueAfter)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	wait := time.Second
	for {
		result, unavailable, err := c.call(ctx, r)
		answer := classify(ctx, result, unavailable, err)
		why := "the server is unreachable"
		switch answer.Kind {
		case outbox.Accepted:
			return result, nil
		case outbox.Maintenance:
			why = "the server is in maintenance"
		case outbox.Transient:
		default:
			return c.answer(ctx, r, result, unavailable, err)
		}
		next := wait
		if answer.RetryAfter > 0 {
			next = answer.RetryAfter
		}
		if ctx.Err() != nil || time.Now().Add(next).After(deadline) || sleep(ctx, next) != nil {
			return c.queue(ctx, ob, r, why)
		}
		wait *= 2
	}
}

// withOperationID gives an ack without an operation_id one, so its replay
// is recorded as the same ack (§1.1).
func withOperationID(r bus.Request) bus.Request {
	if r.Operation != "ack" {
		return r
	}
	if op, _ := r.Args["operation_id"].(string); op != "" {
		return r
	}
	args := make(map[string]any, len(r.Args)+1)
	for k, v := range r.Args {
		args[k] = v
	}
	b := make([]byte, 12)
	rand.Read(b)
	args["operation_id"] = "ack-" + hex.EncodeToString(b)
	r.Args = args
	return r
}

// queue stores r and answers queued_local, as a result: the write will be
// sent, and must not be resent under a new operation ID.
func (c *Client) queue(ctx context.Context, ob *outbox.Store, r bus.Request, why string) (bus.Result, error) {
	q, err := ob.Enqueue(context.WithoutCancel(ctx), c.config.URL, c.scope(), r)
	if err != nil {
		reason := "the local outbox is full"
		if !errors.Is(err, outbox.ErrFull) {
			reason = "the local outbox can't store it: " + err.Error()
		}
		return bus.Result{}, errors.New("cloud write failed or its outcome is uncertain; no write queued (" + reason + "); preserve the operation ID for an identical retry and tell the human")
	}
	op, _ := r.Args["operation_id"].(string)
	message := fmt.Sprintf("%s; %s is queued on this machine (position %d) and will be sent when it's back; %s", why, queuedWhat(r, op), q.Position, queuedAdvice(op))
	if outboxNotice != nil {
		outboxNotice("clawbridge: " + message)
	}
	return bus.Result{Source: "client_outbox", Data: map[string]any{
		"outcome":      "queued_local",
		"operation":    r.Operation,
		"operation_id": op,
		"position":     q.Position,
		"message":      message,
	}}, nil
}

// queuedNamed is how many messages a queued mark-read's notice names.
const queuedNamed = 5

// queuedWhat names a queued write: its operation and operation ID, or,
// for a mark-read, which carries no operation ID, the messages it marks.
func queuedWhat(r bus.Request, op string) string {
	if op != "" {
		return r.Operation + " " + op
	}
	if r.Operation == "mark_read" {
		var ids []string
		switch v := r.Args["ids"].(type) {
		case []string:
			ids = v
		case []any:
			for _, id := range v {
				if s, ok := id.(string); ok {
					ids = append(ids, s)
				}
			}
		}
		if len(ids) == 0 {
			return "mark-read"
		}
		// A mark-read takes up to 1000 IDs; the notice names the first few.
		if len(ids) > queuedNamed {
			return fmt.Sprintf("mark-read of %s and %d more", strings.Join(ids[:queuedNamed], ", "), len(ids)-queuedNamed)
		}
		return "mark-read of " + strings.Join(ids, ", ")
	}
	return r.Operation
}

// queuedAdvice is what not to do while a write waits: a write with an
// operation ID must not be resent under a new one; one without has no ID
// to keep, and needs no rerun.
func queuedAdvice(op string) string {
	if op == "" {
		return "don't run it again"
	}
	return "don't resend it with a new operation ID"
}

// kickOutbox wakes a drainer after a successful call, when writes may wait.
func (c *Client) kickOutbox() {
	if c.outboxStore.Load() == nil {
		return
	}
	select {
	case c.outboxKick <- struct{}{}:
	default:
	}
}

// retryAfter reads a Retry-After of whole seconds, capped at an hour.
func retryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(min(n, 3600)) * time.Second
}

// Maintenance reports whether err is a server's maintenance refusal, and
// how long it asked to wait (§2.4).
func Maintenance(err error) (time.Duration, bool) {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.Code == "maintenance" {
		return max(httpErr.RetryAfter, time.Second), true
	}
	return 0, false
}

// MaintenanceUntilHeader is the server's: on a read it serves during a
// drain, when the drain ends.
const MaintenanceUntilHeader = "ClawBridge-Maintenance-Until"

// noteMaintenance keeps what a served answer says of a drain.
func (c *Client) noteMaintenance(h http.Header) {
	until, err := time.Parse(time.RFC3339, h.Get(MaintenanceUntilHeader))
	if err != nil {
		c.maintenanceUntil.Store(0)
		return
	}
	c.maintenanceUntil.Store(until.UnixNano())
}

// MaintenanceUntil is when the server's maintenance ends, as its last
// answer said, for doctor (§2.4).
func (c *Client) MaintenanceUntil() (time.Time, bool) {
	n := c.maintenanceUntil.Load()
	if n == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}
