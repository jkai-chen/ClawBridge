package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"clawbridge/internal/bus"
)

// The client side of the server's message stream (MESSAGE-QUEUE-DESIGN
// §3.1): Server-Sent Events on GET /v1/stream. Each connect first asks for
// a single-use stream ticket on the signed /v1/execute, then opens the GET
// with the ticket in a header, so no secret is ever in a URL.

// StreamPath and StreamTicketHeader are the server's.
const (
	StreamPath         = "/v1/stream"
	StreamTicketHeader = "ClawBridge-Stream-Ticket"
)

// StreamScope is the session a stream follows, at its instance, and the
// mode: "consume" leases and pushes its deliveries, "observe" only its
// counts.
type StreamScope struct {
	Workspace, Project, Session, Instance, Mode string
}

// StreamEvent is one event from the server: "message" or "status", with
// its JSON data. ID is a message's recipient_seq.
type StreamEvent struct {
	Name string
	ID   string
	Data json.RawMessage
}

// ErrStreamUnsupported is an older server, which has no stream: the
// caller polls as before.
var ErrStreamUnsupported = errors.New("this ClawBridge server has no message stream; polling instead")

// StreamEnded is a stream the server ended for good: "revoked" (the
// session, membership or device no longer allows it), "replaced" (a newer
// consume stream of the session took over) or "refused" (the server
// refused the ticket itself, for Err). Reconnecting won't help.
type StreamEnded struct {
	Reason string
	Err    error
}

func (e *StreamEnded) Error() string {
	switch e.Reason {
	case "replaced":
		return "another consume stream of this session took over"
	case "refused":
		return "the server refused this session's stream: " + e.Err.Error()
	}
	return "the server ended the stream: this session can no longer read it (" + e.Reason + ")"
}

func (e *StreamEnded) Unwrap() error { return e.Err }

// streamLiveness is how long a stream may go without a line before it is
// taken for dead; the server pings every 25 s.
const streamLiveness = 90 * time.Second

// StreamOptions tune Stream; zero fields take the defaults.
type StreamOptions struct {
	// Liveness is how long without any line a connection is given (90 s).
	Liveness time.Duration
	// MaxBackoff caps the wait between failed connects (60 s); the first
	// wait is a second, doubling, each jittered.
	MaxBackoff time.Duration
	// Wait sleeps for d unless ctx ends; nil waits on a timer.
	Wait func(ctx context.Context, d time.Duration) error
	// OnConnect, when set, is called each time a stream opens, so a caller
	// can re-read what it may have missed while disconnected.
	OnConnect func()
}

// Stream follows a session's stream until ctx ends, handing each event to
// handle. It reconnects at once after a bye and with a jittered backoff
// after a failure; a refused or expired ticket and a 429 retry the same way,
// each connect asking for a fresh ticket. It returns ErrStreamUnsupported
// for an older server, a *StreamEnded when the server ended the stream for
// good, a credential failure, or handle's own error.
func (c *Client) Stream(ctx context.Context, scope StreamScope, opts StreamOptions, handle func(StreamEvent) error) error {
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 60 * time.Second
	}
	if opts.Wait == nil {
		opts.Wait = sleep
	}
	backoff := time.Second
	for {
		connected, err := c.StreamOnce(ctx, scope, opts, handle)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ended *StreamEnded
		var handled *handlerError
		switch {
		case errors.Is(err, ErrStreamUnsupported), errors.As(err, &ended), IsCredentialFailure(err):
			return err
		case errors.As(err, &handled):
			return handled.err
		}
		if connected {
			backoff = time.Second
		}
		if err == nil {
			// bye: the server's lifetime or shutdown; reconnect now.
			continue
		}
		wait := backoff/2 + rand.N(backoff/2+1)
		if e := opts.Wait(ctx, wait); e != nil {
			return e
		}
		backoff = min(2*backoff, opts.MaxBackoff)
	}
}

// handlerError carries handle's own error out of StreamOnce.
type handlerError struct{ err error }

func (e *handlerError) Error() string { return e.err.Error() }

// StreamOnce opens one stream and reads it until it ends. connected says
// whether the stream opened; a nil error is the server's bye.
func (c *Client) StreamOnce(ctx context.Context, scope StreamScope, opts StreamOptions, handle func(StreamEvent) error) (connected bool, err error) {
	liveness := opts.Liveness
	if liveness <= 0 {
		liveness = streamLiveness
	}
	ticket, err := c.streamTicket(ctx, scope)
	if err != nil {
		return false, err
	}
	token := ""
	if c.signer == nil {
		if token, err = c.tokens.Token(ctx); err != nil {
			return false, asCredentialError(err, "cannot acquire the configured ClawBridge credential; run login again")
		}
	}
	target := StreamPath + "?" + url.Values{"workspace": {scope.Workspace}, "project": {scope.Project}, "session": {scope.Session}, "mode": {scope.Mode}}.Encode()
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := c.newRequest(streamCtx, http.MethodGet, target, nil, token)
	if err != nil {
		return false, err
	}
	req.Header.Set(StreamTicketHeader, ticket)
	req.Header.Set("Accept", "text/event-stream")
	// The client's request timeout is for requests; a stream lives until
	// the server ends it, and liveness catches a dead connection.
	client := *c.http
	client.Timeout = 0
	response, err := client.Do(req)
	if err != nil {
		return false, errors.New("cloud transport unavailable; the stream will reconnect")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		encoded, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if response.StatusCode == http.StatusNotFound {
			return false, ErrStreamUnsupported
		}
		if response.StatusCode == http.StatusUnauthorized {
			if serverTime, skewed := clockSkewTime(encoded); skewed && c.signer != nil {
				if offset, e := c.signer.Correct(serverTime); e == nil && c.config.SaveClockOffset != nil {
					_, at := c.signer.Offset()
					c.config.SaveClockOffset(offset, at)
				}
				return false, errors.New("the device clock was corrected; the stream will reconnect")
			}
			return false, &credentialError{errors.New("the server refused this credential for the stream")}
		}
		// 403 (the ticket was used or ran out), 429 (too many streams for
		// now) and 5xx are retried with a new ticket after the backoff.
		return false, &HTTPError{Status: response.StatusCode, Code: "stream_refused"}
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		return false, errors.New("the server's stream answer isn't an event stream")
	}
	// Liveness: any line, a ping included, keeps the connection.
	alive := time.AfterFunc(liveness, cancel)
	defer alive.Stop()
	if opts.OnConnect != nil {
		opts.OnConnect()
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var event StreamEvent
	for scanner.Scan() {
		alive.Reset(liveness)
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id: "):
			event.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			event.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			event.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		case line == "" && event.Name == "":
			// A block without an event name carries nothing.
			event = StreamEvent{}
		case line == "":
			switch event.Name {
			case "bye":
				return true, nil
			case "revoked", "replaced":
				return true, &StreamEnded{Reason: event.Name}
			}
			if !json.Valid(event.Data) {
				return true, errors.New("the server sent an unreadable stream event")
			}
			if err = handle(event); err != nil {
				return true, &handlerError{err}
			}
			event = StreamEvent{}
		}
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	return true, errors.New("the stream connection was lost; it will reconnect")
}

// streamTicket asks for one connect's ticket. An older server doesn't know
// the operation; it also has no stream route, which tells it apart from
// any other refusal.
func (c *Client) streamTicket(ctx context.Context, scope StreamScope) (string, error) {
	result, _, err := c.call(ctx, bus.Request{Operation: "stream_ticket", Workspace: scope.Workspace, Project: scope.Project, Session: scope.Session, Args: map[string]any{"instance": scope.Instance, "mode": scope.Mode}})
	var refused *HTTPError
	if errors.As(err, &refused) && refused.Status == http.StatusBadRequest && !c.hasStreamRoute(ctx) {
		return "", ErrStreamUnsupported
	}
	// A definite refusal (a moved instance, a removed member, a retired
	// session, a bad request) won't pass on a retry; anything else (a rate
	// limit, a timeout, a conflict, an outage) may.
	if errors.As(err, &refused) && ticketRefusedForGood(refused.Status) {
		return "", &StreamEnded{Reason: "refused", Err: err}
	}
	if err != nil {
		return "", err
	}
	data, _ := result.Data.(map[string]any)
	ticket, _ := data["ticket"].(string)
	if !strings.HasPrefix(ticket, "stk_") || len(ticket) > 128 {
		return "", errors.New("the server's stream ticket is unreadable")
	}
	return ticket, nil
}

// ticketRefusedForGood is a stream_ticket answer that no retry changes.
func ticketRefusedForGood(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// hasStreamRoute asks, without credentials, whether the server routes
// /v1/stream at all: an older server answers 404 before any sign-in.
func (c *Client) hasStreamRoute(ctx context.Context) bool {
	req, err := c.newRequest(ctx, http.MethodGet, StreamPath, nil, "")
	if err != nil {
		return true
	}
	req.Header.Del("Authorization")
	response, err := c.http.Do(req)
	if err != nil {
		return true
	}
	response.Body.Close()
	return response.StatusCode != http.StatusNotFound
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
