package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// fakeStreamServer issues stream tickets and answers each GET /v1/stream
// with the next of its scripted answers.
type fakeStreamServer struct {
	mu      sync.Mutex
	tickets []string
	gets    []*http.Request
	answers []func(w http.ResponseWriter)
	// ticketStatus refuses stream_ticket with this status when set.
	ticketStatus int
	// noRoute makes the server an older one: no stream at all.
	noRoute bool
}

func (f *fakeStreamServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/v1/execute":
		var req bus.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Operation != "stream_ticket" || f.noRoute || f.ticketStatus != 0 {
			status := f.ticketStatus
			if status == 0 {
				status = 400
			}
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"code":"invalid","message":"no"}}`)
			return
		}
		ticket := fmt.Sprintf("stk_%d", len(f.tickets)+1)
		f.tickets = append(f.tickets, ticket)
		fmt.Fprintf(w, `{"source":"server","data":{"ticket":%q,"mode":%q}}`, ticket, req.Args["mode"])
	case r.URL.Path == StreamPath && !f.noRoute:
		f.gets = append(f.gets, r)
		if len(f.answers) == 0 {
			w.WriteHeader(503)
			return
		}
		answer := f.answers[0]
		f.answers = f.answers[1:]
		f.mu.Unlock()
		answer(w)
		f.mu.Lock()
	default:
		w.WriteHeader(404)
	}
}

func sse(events ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, e := range events {
			io.WriteString(w, e)
			w.(http.Flusher).Flush()
		}
	}
}

func status(code int) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

func streamClient(t *testing.T, f *fakeStreamServer) *Client {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	client, err := NewWithTokenSource(Config{URL: server.URL, Audience: "audience", Principal: "ada@example.com", CachePath: filepath.Join(t.TempDir(), "cache.db")}, TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

var streamScope = StreamScope{Workspace: "w", Project: "p", Session: "b", Instance: "b-secret-instance", Mode: "consume"}

// waits records Stream's backoffs without sleeping.
type waits struct {
	mu sync.Mutex
	d  []time.Duration
}

func (w *waits) wait(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.d = append(w.d, d)
	return ctx.Err()
}

// Events reach the handler in order; a bye reconnects at once with a fresh
// ticket; revoked ends the stream for good. The ticket travels in a header,
// never in the URL, and neither does the instance.
func TestStreamEventsByeAndRevoked(t *testing.T) {
	f := &fakeStreamServer{answers: []func(http.ResponseWriter){
		sse("id: 1\nevent: message\ndata: {\"recipient_seq\":1}\n\n", ": ping\n\n", "event: status\ndata: {\"unacked\":1}\n\n", "event: bye\ndata: {}\n\n"),
		sse("event: revoked\ndata: {}\n\n"),
	}}
	client := streamClient(t, f)
	var got []string
	w := &waits{}
	err := client.Stream(context.Background(), streamScope, StreamOptions{Wait: w.wait}, func(e StreamEvent) error {
		got = append(got, e.Name+":"+e.ID+":"+string(e.Data))
		return nil
	})
	var ended *StreamEnded
	if !errors.As(err, &ended) || ended.Reason != "revoked" {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != `message:1:{"recipient_seq":1}|status::{"unacked":1}` || len(w.d) != 0 {
		t.Fatal(got, w.d)
	}
	if len(f.tickets) != 2 || len(f.gets) != 2 {
		t.Fatal(f.tickets, len(f.gets))
	}
	for i, r := range f.gets {
		if r.Header.Get(StreamTicketHeader) != f.tickets[i] || strings.Contains(r.URL.RawQuery, "instance") || strings.Contains(r.URL.RawQuery, "stk_") || r.URL.Query().Get("mode") != "consume" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Fatal(r.URL, r.Header)
		}
	}
}

// A 429 (too many streams), a 403 (a used or expired ticket) and an outage
// are retried with a growing, jittered backoff, each with a fresh ticket;
// a connect that opens resets the backoff.
func TestStreamRetriesWithFreshTickets(t *testing.T) {
	f := &fakeStreamServer{answers: []func(http.ResponseWriter){
		status(429), status(403), status(503), sse("event: bye\ndata: {}\n\n"), status(503), sse("event: replaced\ndata: {}\n\n"),
	}}
	client := streamClient(t, f)
	w := &waits{}
	err := client.Stream(context.Background(), streamScope, StreamOptions{Wait: w.wait}, func(StreamEvent) error { return nil })
	var ended *StreamEnded
	if !errors.As(err, &ended) || ended.Reason != "replaced" {
		t.Fatal(err)
	}
	if len(f.tickets) != 6 || len(w.d) != 4 {
		t.Fatal(f.tickets, w.d)
	}
	for i, max := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, time.Second} {
		if w.d[i] < max/2 || w.d[i] > max {
			t.Fatal(i, w.d)
		}
	}
}

// The backoff stops doubling at its cap.
func TestStreamBackoffCap(t *testing.T) {
	f := &fakeStreamServer{}
	for range 8 {
		f.answers = append(f.answers, status(503))
	}
	f.answers = append(f.answers, sse("event: revoked\ndata: {}\n\n"))
	client := streamClient(t, f)
	w := &waits{}
	client.Stream(context.Background(), streamScope, StreamOptions{Wait: w.wait, MaxBackoff: 4 * time.Second}, func(StreamEvent) error { return nil })
	if len(w.d) != 8 || w.d[7] > 4*time.Second || w.d[7] < 2*time.Second {
		t.Fatal(w.d)
	}
}

// An older server has neither the ticket nor the route: the caller is
// told to poll.
func TestStreamOldServer(t *testing.T) {
	client := streamClient(t, &fakeStreamServer{noRoute: true})
	err := client.Stream(context.Background(), streamScope, StreamOptions{Wait: (&waits{}).wait}, func(StreamEvent) error { return nil })
	if !errors.Is(err, ErrStreamUnsupported) {
		t.Fatal(err)
	}
}

// A ticket the server refuses for good (a moved instance, a removed
// member) ends the stream; a rate-limited one is retried.
func TestStreamTicketRefused(t *testing.T) {
	for _, code := range []int{400, 403, 404} {
		client := streamClient(t, &fakeStreamServer{ticketStatus: code})
		err := client.Stream(context.Background(), streamScope, StreamOptions{Wait: (&waits{}).wait}, func(StreamEvent) error { return nil })
		var ended *StreamEnded
		if !errors.As(err, &ended) || ended.Reason != "refused" {
			t.Fatal(code, err)
		}
	}
	f := &fakeStreamServer{ticketStatus: 429}
	client := streamClient(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	w := &waits{}
	stop := func(c context.Context, d time.Duration) error {
		if len(w.d) == 2 {
			cancel()
		}
		return w.wait(c, d)
	}
	if err := client.Stream(ctx, streamScope, StreamOptions{Wait: stop}, func(StreamEvent) error { return nil }); !errors.Is(err, context.Canceled) || len(w.d) < 2 {
		t.Fatal(err, w.d)
	}
}

// A connection that goes quiet past the liveness window is dropped, and
// the handler's own error ends the stream.
func TestStreamLivenessAndHandlerError(t *testing.T) {
	quiet := make(chan struct{})
	f := &fakeStreamServer{answers: []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			sse(": ping\n\n")(w)
			select {
			case <-quiet:
			case <-time.After(10 * time.Second):
			}
		},
	}}
	client := streamClient(t, f)
	// After the server's Close in cleanup order, which waits on the handler.
	t.Cleanup(func() { close(quiet) })
	start := time.Now()
	connected, err := client.StreamOnce(context.Background(), streamScope, StreamOptions{Liveness: 200 * time.Millisecond}, func(StreamEvent) error { return nil })
	if !connected || err == nil || time.Since(start) > 5*time.Second {
		t.Fatal(connected, err, time.Since(start))
	}
	f.answers = []func(http.ResponseWriter){sse("event: message\ndata: {}\n\n")}
	stop := errors.New("stop")
	if err = client.Stream(context.Background(), streamScope, StreamOptions{Wait: (&waits{}).wait}, func(StreamEvent) error { return stop }); err != stop {
		t.Fatal(err)
	}
}
