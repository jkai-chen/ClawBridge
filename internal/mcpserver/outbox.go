package mcpserver

import (
	"context"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/outbox"
)

// The client outbox, as the MCP server sees it (ZERO-LOSS-DEPLOY §1.4,
// §1.5, §1.8). The hosted client keeps the writes it couldn't send in the
// profile's outbox; the MCP server, long-lived beside its agent, sends
// them when the server is back, tells the agent they wait (whoami), and
// keeps a message it already handled, whose ack waits, from being handed
// on again.

// outboxClient is an executor with a client outbox: the hosted client.
type outboxClient interface {
	Outbox() *outbox.Store
	DrainOutbox(context.Context, time.Duration) (outbox.Report, error)
	OutboxKicks() <-chan struct{}
	PromoteAck(context.Context, bus.Request, string) (queued, pending bool, err error)
}

// outboxEvery is how often the drainer looks at a non-empty outbox; a
// successful call wakes it sooner.
const outboxEvery = 5 * time.Second

// drainOutbox sends the outbox's waiting writes until ctx ends.
func drainOutbox(ctx context.Context, c outboxClient, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-c.OutboxKicks():
		}
		ob := c.Outbox()
		if ob == nil {
			continue
		}
		if st, err := ob.Status(ctx); err != nil || st.Waiting == 0 {
			continue
		}
		c.DrainOutbox(ctx, 30*time.Second)
	}
}

// outboxStatus is whoami's client_outbox: what waits on this machine, so
// an agent that lost its context learns not to resend it.
func outboxStatus(ctx context.Context, store Executor, data any) {
	c, ok := store.(outboxClient)
	m, isMap := asMap(data)
	if !ok || !isMap {
		return
	}
	ob := c.Outbox()
	if ob == nil {
		return
	}
	st, err := ob.Status(ctx)
	if err != nil {
		return
	}
	m["client_outbox"] = map[string]any{"waiting": st.Waiting, "oldest_age_seconds": st.OldestSeconds, "dead": st.Dead}
}

// ackedLocally reports, for a message offered to this session, whether an
// ack of it waits in the outbox (§1.8). Its ack is moved to the head and
// sent first; queued says the agent already handled it, pending that the
// ack still waits.
func (q *queue) ackedLocally(ctx context.Context, instance, messageID string) (queued, pending bool) {
	c, ok := q.b.store.(outboxClient)
	if !ok || messageID == "" {
		return false, false
	}
	queued, pending, err := c.PromoteAck(ctx, q.request("ack", instance, nil), messageID)
	if err != nil {
		return queued, queued
	}
	return queued, pending
}

// localAckNote is what an agent reads on a message it already handled.
const localAckNote = "you already handled this; its ack is waiting to be sent from this machine's outbox: skip it"

// skipAcked drops from leases the messages whose ack waits locally: they
// were handled, and are not this server's to hand on again.
func (q *queue) skipAcked(ctx context.Context, instance string, leases map[string]heldLease) {
	for id := range leases {
		if queued, _ := q.ackedLocally(ctx, instance, id); queued {
			delete(leases, id)
		}
	}
}

// markAcked takes from an inbox result the messages whose queued ack has
// just been sent, and flags those whose ack still waits.
func (q *queue) markAcked(ctx context.Context, instance string, data any) {
	m, ok := asMap(data)
	if !ok {
		return
	}
	list := rows(data, "messages")
	if len(list) == 0 {
		return
	}
	kept := make([]any, 0, len(list))
	changed := false
	for _, row := range list {
		queued, pending := q.ackedLocally(ctx, instance, str(row["id"]))
		switch {
		case queued && !pending:
			changed = true
			continue
		case pending:
			row["ack_pending_locally"] = true
			row["note"] = localAckNote
			changed = true
		}
		kept = append(kept, row)
	}
	if changed {
		m["messages"] = kept
	}
}

// settledLocally forgets the deliveries a queued ack or reply's ack will
// settle: they were handled.
func (q *queue) settledLocally(operation string, args map[string]any) {
	var ids []string
	switch operation {
	case "ack":
		acks, _ := args["acks"].([]any)
		for _, a := range acks {
			if m, ok := asMap(a); ok {
				ids = append(ids, str(m["message_id"]))
			}
		}
	case "reply", "send":
		if m, ok := asMap(args["ack"]); ok {
			ids = append(ids, str(m["message_id"]))
		}
	}
	if len(ids) == 0 {
		return
	}
	q.mu.Lock()
	for _, id := range ids {
		delete(q.held, id)
	}
	q.mu.Unlock()
	q.save()
	q.poke()
}
