package server

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"github.com/jackc/pgx/v5"
)

// StreamHub wakes the server's streams (MESSAGE-QUEUE-DESIGN §4). One
// dedicated connection LISTENs for queue changes; each stream subscribes
// to its session and is woken through a one-slot channel, so a burst
// coalesces into one read. A wake only says "look": the stream reads its
// queue from the table, so a lost notification costs at most the stream's
// periodic re-read, never a message.
type StreamHub struct {
	mu   sync.Mutex
	subs map[string]map[*streamSub]struct{}
	// closing ends every stream with bye when the server shuts down: a
	// stream never ends on its own before Shutdown's deadline.
	closing chan struct{}
	once    sync.Once
	// open counts each principal's open streams on this server (§8).
	open map[string]int
	// cases are the waiting cases/next polls, woken by any new case.
	cases map[*streamSub]struct{}
	// draining is closed while the server drains: each stream says
	// maintenance, then bye. retry is the retry_after it says.
	draining chan struct{}
	retry    int
	// maintenance, when set, gets each notify on bus.MaintenanceChannel.
	maintenance func()
}

// streamSub is one stream's subscription to its session's queue.
type streamSub struct {
	key string
	// consumer is set for a consume stream: a newer consumer of the same
	// session replaces it.
	consumer string
	wake     chan struct{}
	replaced chan struct{}
	once     sync.Once
}

func NewStreamHub() *StreamHub {
	return &StreamHub{subs: map[string]map[*streamSub]struct{}{}, closing: make(chan struct{}), open: map[string]int{}, cases: map[*streamSub]struct{}{}, draining: make(chan struct{})}
}

// drainSignal is the channel a stream watches for a drain, with its
// retry_after once closed.
func (h *StreamHub) drainSignal() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.draining
}

func (h *StreamHub) retryAfter() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.retry
}

// StartDrain closes every open stream with maintenance, then bye, and
// wakes the waiting cases/next polls, which answer no case.
func (h *StreamHub) StartDrain(retry int) {
	h.mu.Lock()
	select {
	case <-h.draining:
	default:
		h.retry = retry
		close(h.draining)
	}
	h.mu.Unlock()
	h.WakeCases()
}

// EndDrain lets new streams stay open again.
func (h *StreamHub) EndDrain() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.draining:
		h.draining = make(chan struct{})
	default:
	}
}

// acquire counts a stream for principal if it has fewer than limit open.
func (h *StreamHub) acquire(principal string, limit int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[principal] >= limit {
		return false
	}
	h.open[principal]++
	return true
}

// release uncounts a stream that acquire counted.
func (h *StreamHub) release(principal string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open[principal]--; h.open[principal] <= 0 {
		delete(h.open, principal)
	}
}

// Close says bye on every stream; the clients reconnect to another replica
// or to this server once it is back.
func (h *StreamHub) Close() { h.once.Do(func() { close(h.closing) }) }

// streamKey names a session's queue as the notifications do.
func streamKey(projectID, sessionID string) string { return projectID + ":" + sessionID }

func (h *StreamHub) subscribe(key, consumer string) *streamSub {
	sub := &streamSub{key: key, consumer: consumer, wake: make(chan struct{}, 1), replaced: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[key] == nil {
		h.subs[key] = map[*streamSub]struct{}{}
	}
	h.subs[key][sub] = struct{}{}
	return sub
}

func (h *StreamHub) unsubscribe(sub *streamSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs[sub.key], sub)
	if len(h.subs[sub.key]) == 0 {
		delete(h.subs, sub.key)
	}
}

func (s *streamSub) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Wake wakes every stream of one session's queue.
func (h *StreamHub) Wake(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs[key] {
		sub.poke()
	}
}

// WakeAll wakes every stream, each of which re-reads and re-authorizes: after
// the listener reconnects, at start, and when a device is revoked. The
// waiting cases/next polls re-read too.
func (h *StreamHub) WakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, subs := range h.subs {
		for sub := range subs {
			sub.poke()
		}
	}
	for sub := range h.cases {
		sub.poke()
	}
}

// subscribeCases is a cases/next poll's wake for new cases.
func (h *StreamHub) subscribeCases() *streamSub {
	sub := &streamSub{wake: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cases[sub] = struct{}{}
	return sub
}

func (h *StreamHub) unsubscribeCases(sub *streamSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.cases, sub)
}

// WakeCases wakes every waiting cases/next poll: each reads its own
// projects' cases, so a case in another project costs one read.
func (h *StreamHub) WakeCases() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.cases {
		sub.poke()
	}
}

// takeover replaces every consume stream of key but consumer's.
func (h *StreamHub) takeover(key, consumer string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs[key] {
		if sub.consumer != "" && sub.consumer != consumer {
			sub.once.Do(func() { close(sub.replaced) })
		}
	}
}

// notified applies one notification.
func (h *StreamHub) notified(channel, payload string) {
	switch channel {
	case bus.DeliveryChannel:
		h.Wake(payload)
	case bus.CaseChannel:
		h.WakeCases()
	case bus.MaintenanceChannel:
		if h.maintenance != nil {
			h.maintenance()
		}
	case bus.ConsumerChannel:
		if i := strings.LastIndexByte(payload, ':'); i > 0 {
			h.takeover(payload[:i], payload[i+1:])
		}
	}
}

// startStreamHub is serve's hub: its listener carries the serving store's
// name, so it counts as that server.
func startStreamHub(ctx context.Context, store *bus.Store, dsn string) *StreamHub {
	hub := NewStreamHub()
	go hub.ListenAs(ctx, dsn, store.InstanceName())
	return hub
}

// Listen keeps the hub's LISTEN connection open until ctx ends. Each
// (re)connect wakes every stream once, which catches up on whatever was
// notified while the connection was down. The connection string is never
// logged.
func (h *StreamHub) Listen(ctx context.Context, dsn string) {
	h.ListenAs(ctx, dsn, "")
}

// ListenAs is Listen with the serving store's application_name (unless the
// DSN sets one), so the always-open listener counts as that server
// (bus.LiveServerInstances), not as an unnamed client.
func (h *StreamHub) ListenAs(ctx context.Context, dsn, name string) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := h.listenOnce(ctx, dsn, name)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errListenerUp) {
			backoff = time.Second
			log.Print("stream hub: listener connection lost; reconnecting")
		} else {
			log.Print("stream hub: listener unavailable; retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

// errListenerUp is a connection that worked for a while, then dropped.
var errListenerUp = errors.New("listener connection lost")

func (h *StreamHub) listenOnce(ctx context.Context, dsn, name string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return err
	}
	if name != "" && cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = name
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	for _, channel := range []string{bus.DeliveryChannel, bus.ConsumerChannel, bus.CaseChannel, bus.MaintenanceChannel} {
		if _, err = conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
			return err
		}
	}
	h.WakeAll()
	// Notifies missed while the listener was down: re-read the row too.
	if h.maintenance != nil {
		h.maintenance()
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return errListenerUp
		}
		h.notified(n.Channel, n.Payload)
	}
}

// sweepEvery is the sweeper's interval: a run-out lease is offered again
// within this of its lease_until.
const sweepEvery = 15 * time.Second

// deliverySweeper is the hosted authority's sweep.
type deliverySweeper interface {
	SweepDeliveries(context.Context) (bus.SweepResult, error)
}

// webhookPurger is the hosted authority's webhook retention.
type webhookPurger interface {
	PurgeWebhooks(context.Context, time.Time) (int64, error)
}

// webhookPurgeEvery is how often the sweeper purges webhook rows.
const webhookPurgeEvery = time.Hour

// runSweeper sweeps at once and then every interval until ctx ends. Each
// sweep is one transaction under the sweeper's lock, so with several
// replicas one sweeps and the others skip (L2). What it changes notifies
// the streams through the hub.
// A draining server sweeps nothing: a sweep re-offers leases. flush, when
// set, writes the webhook route's due counters each sweep.
func runSweeper(ctx context.Context, store deliverySweeper, every time.Duration, drain *Drain, flush func(context.Context)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	purger, purges := store.(webhookPurger)
	var purged time.Time
	for {
		if done, ok := drain.startWork(); ok {
			if _, err := store.SweepDeliveries(ctx); err != nil && ctx.Err() == nil {
				log.Print("sweeper: sweep failed; retrying at the next interval")
			}
			// Webhook deliveries and counters past their retention, hourly.
			if purges && time.Since(purged) >= webhookPurgeEvery {
				purged = time.Now()
				if _, err := purger.PurgeWebhooks(ctx, purged); err != nil && ctx.Err() == nil {
					log.Print("sweeper: webhook purge failed; retrying in an hour")
				}
			}
			// The webhook route's counters, when a minute is up.
			if flush != nil {
				flush(ctx)
			}
			done()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
