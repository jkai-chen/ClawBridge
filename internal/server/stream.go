package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"clawbridge/internal/bus"
)

// StreamPath is the server's message stream (MESSAGE-QUEUE-DESIGN §3.1):
// Server-Sent Events down, the signed /v1/execute up. A consume stream
// leases the session's deliveries and pushes them; an observe stream pushes
// only its counts.
const StreamPath = "/v1/stream"

// StreamTicketHeader carries the stream ticket, so the URL, which access
// logs keep, never holds a secret (M3).
const StreamTicketHeader = "ClawBridge-Stream-Ticket"

// StreamStore is the hosted authority as the stream uses it.
type StreamStore interface {
	IdentityExecutor
	RedeemStreamTicket(context.Context, bus.Identity, string, bus.StreamScope) (bus.StreamGrant, error)
	AnnounceConsumer(ctx context.Context, projectID, sessionID, consumer string) error
}

// StreamLimits are a stream's intervals and caps.
type StreamLimits struct {
	// Ping is how often a comment line keeps proxies and the client sure the
	// connection lives (25 s).
	Ping time.Duration
	// Reread is how often a stream reads its queue without a wake, which
	// also re-authorizes it (30 s): a lost notification costs at most this.
	Reread time.Duration
	// Lifetime is how long a stream lives before it says bye; the client
	// reconnects, which signs in again (30 min).
	Lifetime time.Duration
	// WriteWait is how long one event may take to write: a client that
	// stops reading would otherwise hold the stream, and its leases, until
	// the connection dies (30 s). Its leases then run out and are offered
	// again.
	WriteWait time.Duration
	// PerPrincipal is how many streams of any mode one principal may hold
	// open on this server at once (8); the next is refused with 429.
	PerPrincipal int
}

func (t StreamLimits) orDefaults() StreamLimits {
	if t.Ping <= 0 {
		t.Ping = 25 * time.Second
	}
	if t.Reread <= 0 {
		t.Reread = 30 * time.Second
	}
	if t.Lifetime <= 0 {
		t.Lifetime = 30 * time.Minute
	}
	if t.WriteWait <= 0 {
		t.WriteWait = 30 * time.Second
	}
	if t.PerPrincipal <= 0 {
		t.PerPrincipal = 8
	}
	return t
}

type streamHandler struct {
	store        StreamStore
	hub          *StreamHub
	authenticate func(context.Context, http.ResponseWriter, *http.Request) (bus.Identity, []byte, bool)
	limit        *limiter
	limits       StreamLimits
	now          func() time.Time
	// drain refuses a new stream while the server drains; nil never does.
	drain *Drain
}

// streamScope reads the stream's URL: workspace, project, session and mode,
// each once, and nothing else. An instance or a ticket in the URL is
// refused, so a client bug can't leak either into a log.
func streamScope(r *http.Request) (bus.StreamScope, error) {
	q := r.URL.Query()
	if q.Has("instance") || q.Has("ticket") {
		return bus.StreamScope{}, errors.New("the stream URL must not carry the instance or a ticket; send the ticket in the " + StreamTicketHeader + " header")
	}
	for k, v := range q {
		switch k {
		case "workspace", "project", "session", "mode":
			if len(v) != 1 {
				return bus.StreamScope{}, fmt.Errorf("%s must be given once", k)
			}
		default:
			return bus.StreamScope{}, fmt.Errorf("unsupported stream parameter %q", k)
		}
	}
	scope := bus.StreamScope{Workspace: q.Get("workspace"), Project: q.Get("project"), Session: q.Get("session"), Mode: q.Get("mode")}
	if scope.Workspace == "" || scope.Project == "" || scope.Session == "" {
		return bus.StreamScope{}, errors.New("workspace, project and session are required")
	}
	if scope.Mode != bus.StreamConsume && scope.Mode != bus.StreamObserve {
		return bus.StreamScope{}, errors.New("mode must be consume or observe")
	}
	return scope, nil
}

func (h *streamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, 405, "method_not_allowed", "GET required")
		return
	}
	scope, err := streamScope(r)
	if err != nil {
		fail(w, 400, "invalid", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	verified, _, ok := h.authenticate(ctx, w, r)
	if !ok {
		cancel()
		return
	}
	if !h.limit.allow(verified.Principal) {
		cancel()
		fail(w, 429, "rate_limited", "principal request limit reached")
		return
	}
	// A draining server opens no stream: a consume stream leases.
	if h.drain != nil {
		if retry, on := h.drain.Check(ctx); on {
			cancel()
			failMaintenance(w, retry)
			return
		}
	}
	tickets := r.Header.Values(StreamTicketHeader)
	if len(tickets) != 1 {
		cancel()
		fail(w, 400, "invalid", "one "+StreamTicketHeader+" header is required; ask for a ticket with stream_ticket")
		return
	}
	// The cap is checked before the ticket is used, so a refused stream
	// can retry with the same ticket once one closes.
	if !h.hub.acquire(verified.Principal, h.limits.PerPrincipal) {
		cancel()
		fail(w, 429, "rate_limited", "this account has too many streams open; close one first")
		return
	}
	defer h.hub.release(verified.Principal)
	grant, err := h.store.RedeemStreamTicket(ctx, verified, tickets[0], scope)
	cancel()
	if errors.Is(err, bus.ErrStreamTicket) {
		fail(w, 403, "forbidden", err.Error())
		return
	}
	if err != nil {
		fail(w, 503, "unavailable", publicMessage("unavailable"))
		return
	}
	s := &stream{h: h, w: w, rc: http.NewResponseController(w), id: verified, scope: scope, grant: grant}
	s.run(r.Context())
}

// stream is one open stream.
type stream struct {
	h     *streamHandler
	w     http.ResponseWriter
	rc    *http.ResponseController
	id    bus.Identity
	scope bus.StreamScope
	grant bus.StreamGrant
	// consumer names a consume stream's leases, so a stream that replaces
	// it takes them over.
	consumer string
	sub      *streamSub
	// status is the last status an observe stream sent.
	status map[string]any
}

func (s *stream) run(ctx context.Context) {
	// The server's deadlines are for requests; a stream ends at its
	// lifetime, its token's expiry, a revoke, shutdown, or a write that
	// takes longer than WriteWait (write sets that deadline each time). The
	// read deadline is cleared too, so no server deadline but the stream's
	// own can end it. Each read re-checks what a request's transaction
	// does; a device's dormancy is checked when a request is signed, so a
	// device that turns dormant mid-stream is caught at its next connect,
	// within the lifetime.
	_ = s.rc.SetReadDeadline(time.Time{})
	lifetime := s.h.limits.Lifetime
	if !s.id.Expires.IsZero() {
		lifetime = min(lifetime, s.id.Expires.Sub(s.h.now()))
	}
	key := streamKey(s.grant.ProjectID, s.grant.SessionID)
	if s.scope.Mode == bus.StreamConsume {
		s.consumer = newConsumerID()
	}
	sub := s.h.hub.subscribe(key, s.consumer)
	s.sub = sub
	defer s.h.hub.unsubscribe(sub)
	if s.consumer != "" {
		// This replica's older stream is replaced at once, every other
		// replica's when the announcement arrives.
		s.h.hub.takeover(key, s.consumer)
		announce, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.h.store.AnnounceConsumer(announce, s.grant.ProjectID, s.grant.SessionID, s.consumer)
		cancel()
		if err != nil {
			fail(s.w, 503, "unavailable", publicMessage("unavailable"))
			return
		}
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	if s.rc.Flush() != nil {
		return
	}
	end := time.NewTimer(lifetime)
	defer end.Stop()
	ping := time.NewTicker(s.h.limits.Ping)
	defer ping.Stop()
	reread := time.NewTicker(s.h.limits.Reread)
	defer reread.Stop()
	drain := s.h.hub.drainSignal()
	sub.poke()
	for {
		select {
		case <-ctx.Done():
			return
		case <-drain:
			// The client waits retry_after, then reconnects (§2.4).
			s.event("maintenance", map[string]any{"retry_after": s.h.hub.retryAfter()})
			s.event("bye", map[string]any{})
			return
		case <-sub.replaced:
			s.event("replaced", map[string]any{})
			return
		case <-end.C:
			s.event("bye", map[string]any{})
			return
		case <-s.h.hub.closing:
			s.event("bye", map[string]any{})
			return
		case <-ping.C:
			if !s.write(": ping\n\n") {
				return
			}
		case <-reread.C:
			if !s.read(ctx) {
				return
			}
		case <-sub.wake:
			if !s.read(ctx) {
				return
			}
		}
	}
}

// read runs the stream's read as the caller, through the same checks every
// request gets (M4): the session active at the ticket's instance, the
// principal still a member, the device live, the account bound. A refusal
// closes the stream with revoked before anything is leased or pushed; an
// unavailable authority closes it with bye, and the client reconnects.
func (s *stream) read(ctx context.Context) bool {
	// A replaced stream leases nothing more.
	select {
	case <-s.sub.replaced:
		s.event("replaced", map[string]any{})
		return false
	default:
	}
	request := bus.Request{Workspace: s.scope.Workspace, Project: s.scope.Project, Session: s.scope.Session, Operation: "notification_status", Args: map[string]any{"instance": s.grant.Instance, "queue": true}}
	if s.consumer != "" {
		request.Operation, request.Args = "lease", map[string]any{"instance": s.grant.Instance, "consumer": s.consumer}
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	result, err := s.h.store.ExecuteAsIdentity(rctx, s.id, request)
	cancel()
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		if code, _ := serviceError(err); code == "unavailable" {
			s.event("bye", map[string]any{})
		} else {
			s.event("revoked", map[string]any{})
		}
		return false
	}
	data, _ := result.Data.(map[string]any)
	if s.consumer == "" {
		status := map[string]any{"unacked": data["unacked_count"], "latest_seq": data["latest_seq"], "dead": data["dead_count"]}
		if fmt.Sprint(status) == fmt.Sprint(s.status) {
			return true
		}
		s.status = status
		return s.event("status", status)
	}
	messages, _ := data["messages"].([]map[string]any)
	for _, m := range messages {
		event := map[string]any{"recipient_seq": m["recipient_seq"], "lease_id": m["lease_id"], "lease_until": m["lease_until"], "attempt": m["attempt"]}
		row := make(map[string]any, len(m))
		for k, v := range m {
			if _, lease := event[k]; !lease {
				row[k] = v
			}
		}
		event["message"] = row
		if !s.eventWithID(fmt.Sprint(m["recipient_seq"]), "message", event) {
			return false
		}
	}
	return true
}

func (s *stream) event(name string, data any) bool { return s.eventWithID("", name, data) }

func (s *stream) eventWithID(id, name string, data any) bool {
	b, err := json.Marshal(data)
	if err != nil {
		return false
	}
	line := ""
	if id != "" {
		line = "id: " + id + "\n"
	}
	return s.write(line + "event: " + name + "\ndata: " + string(b) + "\n\n")
}

// write sends one event and flushes it, so no proxy or buffer holds it.
// A client that doesn't take it within WriteWait ends the stream.
func (s *stream) write(text string) bool {
	if s.rc.SetWriteDeadline(time.Now().Add(s.h.limits.WriteWait)) != nil {
		return false
	}
	if _, err := s.w.Write([]byte(text)); err != nil {
		return false
	}
	return s.rc.Flush() == nil
}

func newConsumerID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "stm_" + base64.RawURLEncoding.EncodeToString(b)
}
