package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/notify"
	"clawbridge/internal/remote"
)

// The MCP server is its agent's consumer of the message queue
// (MESSAGE-QUEUE-DESIGN §5). It holds the session's consume stream, or on
// a server without one (and in local mode) polls lease, and keeps the
// leased deliveries in memory: the inbox tool marks them with their lease,
// ack and reply's ack take their lease from it, and the held leases are
// extended while the agent is active. It also keeps a private record for
// the agent's hooks, which run as their own processes.

// streamer is an executor that can follow a session's stream: the hosted
// client.
type streamer interface {
	Stream(context.Context, remote.StreamScope, remote.StreamOptions, func(remote.StreamEvent) error) error
}

// Queue defaults; Options can shorten them for tests.
const (
	leasePollEvery = 2 * time.Second
	extendEvery    = 5 * time.Minute
	activeWindow   = 10 * time.Minute
	extendSeconds  = 600
	recordEvery    = time.Minute
	// streamSafety is how often the inbox resource still checks while the
	// stream is open, in case a wake was missed.
	streamSafety = 60 * time.Second
)

// heldLease is one delivery this server holds.
type heldLease struct {
	LeaseID    string `json:"lease_id"`
	Attempt    any    `json:"attempt,omitempty"`
	LeaseUntil any    `json:"lease_until,omitempty"`
}

type queue struct {
	b       *binding
	options Options
	// wake pokes the inbox resource when the queue changes.
	wake chan struct{}
	// lastCall is when the agent last called a tool, in Unix nanoseconds.
	lastCall atomic.Int64

	mu       sync.Mutex
	held     map[string]heldLease
	state    string
	instance string
	cancel   context.CancelFunc
	// epoch tells a stopped consumer's late events from the current one's.
	epoch uint64
	// closed stops writing the record once the server has ended.
	closed bool
	// wanted is set by the agent's first message tool call: only then does
	// the server take deliveries, so a server that is merely started or
	// listed makes no calls to the authority. ctx is the current
	// instance's, for that start.
	wanted bool
	ctx    context.Context
}

func newQueue(b *binding, o Options) *queue {
	q := &queue{b: b, options: o, wake: make(chan struct{}, 1), held: map[string]heldLease{}, state: "off"}
	q.lastCall.Store(q.now().UnixNano())
	return q
}

func (q *queue) now() time.Time {
	if q.options.Now != nil {
		return q.options.Now()
	}
	return time.Now()
}

func (q *queue) diagnostics() io.Writer {
	if q.options.Diagnostics != nil {
		return q.options.Diagnostics
	}
	return os.Stderr
}

func (q *queue) poke() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// touch notes a tool call: the agent is active.
func (q *queue) touch() { q.lastCall.Store(q.now().UnixNano()) }

// streaming says whether the consume stream is open, so the inbox resource
// waits on its wakes rather than polling.
func (q *queue) streaming() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.state == "stream"
}

// restart follows the queue of instance, from scratch: what an earlier
// instance held is no longer this session's to handle. It keeps the
// record, and takes deliveries once the agent has called a message tool.
func (q *queue) restart(instance string) {
	q.mu.Lock()
	if q.cancel != nil {
		q.cancel()
		q.cancel, q.ctx = nil, nil
	}
	q.epoch++
	q.held = map[string]heldLease{}
	q.instance = instance
	q.state = "off"
	if instance == "" || q.options.Context == nil {
		q.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(q.options.Context)
	q.cancel, q.ctx = cancel, ctx
	epoch := q.epoch
	wanted := q.wanted
	if wanted {
		q.state = "connecting"
	}
	q.mu.Unlock()
	q.save()
	go q.maintain(ctx, epoch, instance)
	if wanted {
		go q.consume(ctx, epoch, instance)
	}
}

// want starts taking deliveries, on the agent's first message tool call.
func (q *queue) want() {
	q.mu.Lock()
	if q.wanted {
		q.mu.Unlock()
		return
	}
	q.wanted = true
	ctx, epoch, instance := q.ctx, q.epoch, q.instance
	if ctx == nil {
		q.mu.Unlock()
		return
	}
	q.state = "connecting"
	q.mu.Unlock()
	q.save()
	go q.consume(ctx, epoch, instance)
}

func (q *queue) request(operation, instance string, args map[string]any) bus.Request {
	if args == nil {
		args = map[string]any{}
	}
	args["instance"] = instance
	return bus.Request{Operation: operation, Workspace: q.b.workspace, Project: q.b.project, Session: q.b.session, Args: args}
}

// setState records how the queue is followed, for the current consumer only.
func (q *queue) setState(epoch uint64, state string) {
	q.mu.Lock()
	if q.epoch != epoch || q.state == state {
		q.mu.Unlock()
		return
	}
	q.state = state
	if state == "ended" || state == "off" {
		q.held = map[string]heldLease{}
	}
	q.mu.Unlock()
	q.save()
	q.poke()
}

// consume follows the stream, or polls lease without one, until ctx ends
// or the queue ends for good.
func (q *queue) consume(ctx context.Context, epoch uint64, instance string) {
	if s, ok := q.b.store.(streamer); ok {
		scope := remote.StreamScope{Workspace: q.b.workspace, Project: q.b.project, Session: q.b.session, Instance: instance, Mode: "consume"}
		err := s.Stream(ctx, scope, remote.StreamOptions{OnConnect: func() { q.setState(epoch, "stream") }}, func(e remote.StreamEvent) error {
			if e.Name == "message" {
				q.offered(epoch, e.Data)
			}
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, remote.ErrStreamUnsupported) {
			// Replaced by another consumer of the session, revoked, or
			// refused: the leases are no longer this server's.
			q.setState(epoch, "ended")
			fmt.Fprintln(q.diagnostics(), "clawbridge: the message stream ended; the inbox tool still reads the inbox")
			return
		}
	}
	q.poll(ctx, epoch, instance)
}

// offered takes one delivery from a stream's message event.
func (q *queue) offered(epoch uint64, data json.RawMessage) {
	var e struct {
		LeaseID    string `json:"lease_id"`
		LeaseUntil any    `json:"lease_until"`
		Attempt    any    `json:"attempt"`
		Message    struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if json.Unmarshal(data, &e) != nil || e.Message.ID == "" || e.LeaseID == "" {
		return
	}
	q.hold(epoch, map[string]heldLease{e.Message.ID: {LeaseID: e.LeaseID, Attempt: e.Attempt, LeaseUntil: e.LeaseUntil}})
}

func (q *queue) hold(epoch uint64, leases map[string]heldLease) {
	q.mu.Lock()
	if q.epoch != epoch {
		q.mu.Unlock()
		return
	}
	for id, l := range leases {
		q.held[id] = l
	}
	q.mu.Unlock()
	q.save()
	q.poke()
}

// poll leases the session's deliveries every couple of seconds: local mode,
// and a server with the queue but no stream. A server without the queue
// refuses lease, and the inbox works as it always did.
func (q *queue) poll(ctx context.Context, epoch uint64, instance string) {
	consumer := newConsumerID()
	every := q.options.LeasePoll
	if every <= 0 {
		every = leasePollEvery
	}
	delay := time.Duration(0)
	for {
		if notify.Wait(ctx, delay) != nil {
			return
		}
		result, err := q.b.store.Execute(ctx, q.request("lease", instance, map[string]any{"consumer": consumer}))
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if strings.Contains(err.Error(), "unsupported operation") || notify.IsPermanent(err) {
				q.setState(epoch, "off")
				return
			}
			delay = min(30*time.Second, max(every, 2*delay))
			continue
		}
		delay = every
		q.setState(epoch, "polling")
		leases := map[string]heldLease{}
		for _, m := range rows(result.Data, "messages") {
			if id, lease := str(m["id"]), str(m["lease_id"]); id != "" && lease != "" {
				leases[id] = heldLease{LeaseID: lease, Attempt: m["attempt"], LeaseUntil: m["lease_until"]}
			}
		}
		if len(leases) > 0 {
			q.hold(epoch, leases)
		}
	}
}

// maintain keeps the record fresh, extends the held leases while the agent
// is active (§11.2), and ends a compaction's hold that has outlived any
// compaction.
func (q *queue) maintain(ctx context.Context, epoch uint64, instance string) {
	every := q.options.RecordEvery
	if every <= 0 {
		every = recordEvery
	}
	extend := q.options.ExtendEvery
	if extend <= 0 {
		extend = extendEvery
	}
	tick := time.NewTicker(min(every, extend))
	defer tick.Stop()
	lastExtend := q.now()
	// A hold left by a compaction when this server starts is from an agent
	// that died mid-compaction: compactions don't restart MCP servers.
	q.endStaleHold(ctx, instance, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		q.save()
		q.endStaleHold(ctx, instance, false)
		if q.now().Sub(lastExtend) >= extend {
			lastExtend = q.now()
			q.extend(ctx, epoch, instance)
		}
	}
}

// active says whether the agent called a tool lately: a dead or stuck agent
// stops extending, and its leases run out.
func (q *queue) active() bool {
	window := q.options.ActiveWindow
	if window <= 0 {
		window = activeWindow
	}
	return q.now().Sub(time.Unix(0, q.lastCall.Load())) < window
}

func (q *queue) extend(ctx context.Context, epoch uint64, instance string) {
	if !q.active() {
		return
	}
	q.mu.Lock()
	leases := make([]any, 0, len(q.held))
	for _, l := range q.held {
		leases = append(leases, l.LeaseID)
	}
	q.mu.Unlock()
	if len(leases) == 0 {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := q.b.store.Execute(bounded, q.request("extend", instance, map[string]any{"leases": leases, "seconds": extendSeconds}))
	if err != nil {
		return
	}
	outcomes := map[string]map[string]any{}
	for _, r := range rows(result.Data, "leases") {
		outcomes[str(r["lease_id"])] = r
	}
	q.mu.Lock()
	if q.epoch != epoch {
		q.mu.Unlock()
		return
	}
	for id, l := range q.held {
		o, ok := outcomes[l.LeaseID]
		switch {
		case !ok:
		case o["outcome"] == "extended":
			l.LeaseUntil = o["lease_until"]
			q.held[id] = l
		default:
			// Acked elsewhere, or run out and offered again: gone.
			delete(q.held, id)
		}
	}
	q.mu.Unlock()
	q.save()
}

// endStaleHold sets the policy back to accept when `context compacting`
// held it for a compaction longer ago than any compaction takes, or, at
// start, at all. The policy is the session's, whichever instance held it.
func (q *queue) endStaleHold(ctx context.Context, instance string, starting bool) {
	dir := q.options.ContextDir
	if dir == "" {
		return
	}
	h, ok := ctxwatch.LoadCompactionHold(dir, q.b.session)
	if !ok {
		return
	}
	at, err := time.Parse(time.RFC3339, h.HeldAt)
	if !starting && err == nil && q.now().Sub(at) < ctxwatch.CompactionHoldLimit {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err = q.b.store.Execute(bounded, q.request("set_notification_policy", instance, map[string]any{"policy": "accept"})); err != nil {
		return
	}
	ctxwatch.RemoveCompactionHold(dir, q.b.session)
	fmt.Fprintln(q.diagnostics(), "clawbridge: a compaction's hold outlived its limit; notifications are accepted again")
}

// annotate marks each message of an inbox result that this server holds
// with its lease, for ack.
func (q *queue) annotate(data any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.held) == 0 {
		return
	}
	for _, m := range rows(data, "messages") {
		if l, ok := q.held[str(m["id"])]; ok {
			m["lease"] = l
		}
	}
}

// fillAcks gives each ack without a lease_id the lease this server holds.
func (q *queue) fillAcks(args map[string]any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	acks, _ := args["acks"].([]any)
	for _, a := range acks {
		m, ok := a.(map[string]any)
		if !ok || str(m["lease_id"]) != "" {
			continue
		}
		if l, held := q.held[str(m["message_id"])]; held {
			m["lease_id"] = l.LeaseID
		}
	}
}

// replyAck turns reply's ack:true into the ack the server commits with
// the reply, in one transaction.
func (q *queue) replyAck(args map[string]any) {
	if ack, _ := args["ack"].(bool); !ack {
		return
	}
	id := str(args["message_id"])
	item := map[string]any{"message_id": id}
	q.mu.Lock()
	if l, held := q.held[id]; held {
		item["lease_id"] = l.LeaseID
	}
	q.mu.Unlock()
	args["ack"] = item
}

// settled forgets deliveries an ack or a reply's ack settled, whatever
// the outcome: acked, already acked, dead or not found, none is this
// server's to handle any more.
func (q *queue) settled(operation string, data any) {
	var ids []string
	switch operation {
	case "ack":
		for _, a := range rows(data, "acks") {
			ids = append(ids, str(a["message_id"]))
		}
	case "reply", "send":
		if m, ok := asMap(data); ok {
			if a, ok := asMap(m["ack"]); ok {
				ids = append(ids, str(a["message_id"]))
			}
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

// save writes the hooks' record, when there is a context directory and an
// instance.
func (q *queue) save() {
	dir := q.options.ContextDir
	if dir == "" {
		return
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	r := ctxwatch.QueueRecord{Session: q.b.session, Workspace: q.b.workspace, Project: q.b.project, Instance: q.instance, Server: q.options.Server, DB: q.options.DB,
		MCPPID: os.Getpid(), AgentPID: os.Getppid(), Stream: q.state, Leases: []ctxwatch.QueueLease{}, UpdatedAt: q.now().UTC().Format(time.RFC3339)}
	for id, l := range q.held {
		r.Leases = append(r.Leases, ctxwatch.QueueLease{MessageID: id, LeaseID: l.LeaseID})
	}
	if r.Instance == "" || (r.Server == "") == (r.DB == "") {
		q.mu.Unlock()
		return
	}
	slices.SortFunc(r.Leases, func(a, b ctxwatch.QueueLease) int { return strings.Compare(a.MessageID, b.MessageID) })
	// Written under the lock, so close can't remove it before a late write.
	_ = ctxwatch.SaveQueueRecord(dir, r)
	q.mu.Unlock()
}

// close removes the record when the server ends.
func (q *queue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	if q.options.ContextDir != "" {
		ctxwatch.RemoveQueueRecord(q.options.ContextDir, q.b.session, os.Getpid())
	}
}

func newConsumerID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "mcp_" + hex.EncodeToString(b)
}

// rows reads a list of objects under key from a local result (Go maps) or
// a hosted one (decoded JSON).
func rows(data any, key string) []map[string]any {
	m, ok := asMap(data)
	if !ok {
		return nil
	}
	switch list := m[key].(type) {
	case []map[string]any:
		return list
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, x := range list {
			if r, ok := x.(map[string]any); ok {
				out = append(out, r)
			}
		}
		return out
	}
	return nil
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
