package linearpace

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a fake clock whose waits advance it.
type clock struct {
	at     time.Time
	waited []time.Duration
}

func (c *clock) now() time.Time { return c.at }
func (c *clock) wait(_ context.Context, d time.Duration) error {
	c.waited = append(c.waited, d)
	c.at = c.at.Add(d)
	return nil
}

func fakePacer(perHour int) (*Pacer, *clock) {
	c := &clock{at: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	p := New(perHour)
	p.now, p.wait = c.now, c.wait
	return p, c
}

func headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// A fresh pacer admits at once; Linear's Remaining corrects it, and a
// bucket with no room makes a call wait for the refill, at Linear's rate.
func TestPacerFollowsLinearsRemaining(t *testing.T) {
	p, c := fakePacer(OAuthPerHour)
	if _, ok, err := p.Wait(context.Background(), Write, false, time.Second); !ok || err != nil || len(c.waited) != 0 {
		t.Fatal("a fresh pacer held a call", ok, err, c.waited)
	}
	p.Observe(200, headers("X-RateLimit-Requests-Limit", "3600", "X-RateLimit-Requests-Remaining", "0"))
	// 3600 an hour is one a second: the next write waits a second.
	if _, ok, err := p.Wait(context.Background(), Write, false, 5*time.Second); !ok || err != nil || len(c.waited) != 1 || c.waited[0] != time.Second {
		t.Fatal("the write did not wait for one refill:", ok, err, c.waited)
	}
}

// Reads leave a tenth of the bucket for writes.
func TestReadsLeaveRoomForWrites(t *testing.T) {
	p, c := fakePacer(OAuthPerHour)
	p.Observe(200, headers("X-RateLimit-Requests-Limit", "1000", "X-RateLimit-Requests-Remaining", "50"))
	at, ok, err := p.Wait(context.Background(), Read, false, time.Second)
	if ok || err != nil || !at.After(c.at) {
		t.Fatal("a read took the writes' reserve:", ok, err, at)
	}
	if _, ok, _ = p.Wait(context.Background(), Write, false, time.Second); !ok {
		t.Fatal("a write was held with room in the reserve")
	}
}

// A call that would wait past its bound is not admitted, and says when it
// could go; the context's deadline bounds it too.
func TestPacerBound(t *testing.T) {
	p, c := fakePacer(3600)
	p.Observe(200, headers("X-RateLimit-Requests-Remaining", "0"))
	at, ok, err := p.Wait(context.Background(), Write, false, 500*time.Millisecond)
	if ok || err != nil || at != c.at.Add(time.Second) || len(c.waited) != 0 {
		t.Fatal(at, ok, err, c.waited)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, ok, _ = p.Wait(ctx, Write, false, time.Hour); ok {
		t.Fatal("waited past the context's deadline")
	}
}

// A 429 holds every call until Linear's Retry-After; without one, a
// doubling backoff from 2 s to a minute.
func TestPacerHoldsAfterA429(t *testing.T) {
	p, c := fakePacer(OAuthPerHour)
	p.Observe(429, headers("Retry-After", "30"))
	at, ok, _ := p.Wait(context.Background(), Write, false, time.Second)
	if ok || at.Sub(c.at) < 30*time.Second {
		t.Fatal("a call went inside Retry-After:", at.Sub(c.at))
	}
	q, d := fakePacer(OAuthPerHour)
	for i, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		q.Observe(429, http.Header{})
		at, _, _ := q.Wait(context.Background(), Write, false, 0)
		if got := at.Sub(d.at); got < want {
			t.Fatal(i, got, want)
		}
		d.at = d.at.Add(time.Hour)
	}
	for range 10 {
		q.Observe(429, http.Header{})
	}
	if q.backoff != maxBackoff {
		t.Fatal("backoff not capped:", q.backoff)
	}
	q.Observe(200, http.Header{})
	if q.backoff != 0 {
		t.Fatal("a success kept the backoff")
	}
}

// A GraphQL call waits for the complexity reset when less than one
// query's maximum is left; an MCP call doesn't.
func TestPacerComplexity(t *testing.T) {
	p, c := fakePacer(OAuthPerHour)
	reset := c.at.Add(10 * time.Second)
	p.Observe(200, headers("X-RateLimit-Complexity-Remaining", "500", "X-RateLimit-Complexity-Reset", itoa(reset.UnixMilli())))
	if at, ok, _ := p.Wait(context.Background(), Read, true, time.Second); ok || !at.Equal(reset) {
		t.Fatal("a GraphQL read went with no complexity left:", at, ok)
	}
	if _, ok, _ := p.Wait(context.Background(), Read, false, time.Second); !ok {
		t.Fatal("an MCP read waited for GraphQL complexity")
	}
}

// A leaky complexity bucket refills at Limit an hour: a dip to 5k of 2M
// waits the ~9 s to the 10k floor, not the hour to Reset, and is admitted
// after it (review M2). Reset still caps the wait.
func TestPacerComplexityRefills(t *testing.T) {
	p, c := fakePacer(OAuthPerHour)
	p.Observe(200, headers("X-RateLimit-Complexity-Limit", "2000000", "X-RateLimit-Complexity-Remaining", "5000", "X-RateLimit-Complexity-Reset", itoa(c.at.Add(time.Hour).UnixMilli())))
	if at, ok, _ := p.Wait(context.Background(), Read, true, time.Second); ok || at.Sub(c.at) != 9*time.Second {
		t.Fatal("a 5k dip did not wait ~9 s:", at.Sub(c.at), ok)
	}
	if _, ok, _ := p.Wait(context.Background(), Read, true, 10*time.Second); !ok || len(c.waited) != 1 || c.waited[0] != 9*time.Second {
		t.Fatal("the refilled complexity was not admitted:", ok, c.waited)
	}
	q, d := fakePacer(OAuthPerHour)
	q.Observe(200, headers("X-RateLimit-Complexity-Limit", "2000000", "X-RateLimit-Complexity-Remaining", "0", "X-RateLimit-Complexity-Reset", itoa(d.at.Add(4*time.Second).UnixMilli())))
	if at, _, _ := q.Wait(context.Background(), Read, true, 0); at.Sub(d.at) != 4*time.Second {
		t.Fatal("the wait was not capped at Reset:", at.Sub(d.at))
	} // Without a Reset, the refill alone sets the wait.
	r, e := fakePacer(OAuthPerHour)
	r.Observe(200, headers("X-RateLimit-Complexity-Limit", "2000000", "X-RateLimit-Complexity-Remaining", "5000"))
	if at, _, _ := r.Wait(context.Background(), Read, true, 0); at.Sub(e.at) != 9*time.Second {
		t.Fatal("no Reset: the refill was not waited for:", at.Sub(e.at))
	}
}

// A Retry-After given as an HTTP date holds calls until that time (L3).
func TestPacerRetryAfterDate(t *testing.T) {
	p, c := fakePacer(OAuthPerHour)
	p.Observe(429, headers("Retry-After", c.at.Add(30*time.Second).UTC().Format(http.TimeFormat)))
	if at, ok, _ := p.Wait(context.Background(), Write, false, time.Second); ok || at.Sub(c.at) != 30*time.Second || p.backoff != 0 {
		t.Fatal("an HTTP-date Retry-After was not followed:", at.Sub(c.at), ok, p.backoff)
	}
}

// A GraphQL request no sender marked a read (a mutation, like the app's
// scope probe) is sent exactly once on Linear's 429 (review M1).
func TestTransportNeverResendsUnmarkedGraphQL(t *testing.T) {
	p, _ := fakePacer(OAuthPerHour)
	f := &fakeLinear{answers: []*http.Response{answer(429, headers("Retry-After", "1")), answer(200, http.Header{})}}
	res, err := Transport{Base: f, Pacer: p}.RoundTrip(post(t, context.Background(), "https://api.linear.app/graphql"))
	if err != nil || res.StatusCode != 429 || len(f.sent) != 1 {
		t.Fatal("a GraphQL mutation was re-sent:", res, err, f.sent)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// fakeLinear answers each request with the next status and headers.
type fakeLinear struct {
	answers []*http.Response
	sent    []string
}

func (f *fakeLinear) RoundTrip(r *http.Request) (*http.Response, error) {
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	f.sent = append(f.sent, r.Method+" "+r.URL.Path+" "+body)
	if len(f.answers) == 0 {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, nil
}

func answer(status int, h http.Header) *http.Response {
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("{}"))}
}

func post(t *testing.T, ctx context.Context, url string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// read is a GraphQL request its sender marks a read.
func read(t *testing.T, url string) *http.Request {
	return post(t, WithKind(context.Background(), Read), url)
}

// A call the pacer holds past its bound is never sent: the Transport
// answers a 429 of its own, marked, with Linear's RATELIMITED shape.
func TestTransportAnswersAHeldCallWithoutSending(t *testing.T) {
	p, _ := fakePacer(3600)
	p.Observe(429, headers("Retry-After", "42"))
	f := &fakeLinear{}
	res, err := Transport{Base: f, Pacer: p}.RoundTrip(post(t, context.Background(), "https://mcp.linear.app/mcp"))
	if err != nil || res.StatusCode != 429 || res.Header.Get(PacedHeader) != "1" || res.Header.Get("Retry-After") != "42" || len(f.sent) != 0 {
		t.Fatal(res, err, f.sent)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `"code":"RATELIMITED"`) || !strings.Contains(string(body), "nothing was sent") {
		t.Fatal(string(body))
	}
}

// Linear's 429 on a read is sent once more within its bound; on a write it
// goes back to the caller unsent again.
func TestTransportRetriesAReadOnce(t *testing.T) {
	p, c := fakePacer(OAuthPerHour)
	f := &fakeLinear{answers: []*http.Response{answer(429, headers("Retry-After", "3")), answer(200, http.Header{})}}
	res, err := Transport{Base: f, Pacer: p}.RoundTrip(read(t, "https://api.linear.app/graphql"))
	if err != nil || res.StatusCode != 200 || len(f.sent) != 2 || f.sent[1] != `POST /graphql {"query":"q"}` || len(c.waited) != 1 || c.waited[0] != 3*time.Second {
		t.Fatal(res, err, f.sent, c.waited)
	}
	p2, _ := fakePacer(OAuthPerHour)
	f = &fakeLinear{answers: []*http.Response{answer(429, headers("Retry-After", "3"))}}
	res, err = Transport{Base: f, Pacer: p2}.RoundTrip(post(t, context.Background(), "https://mcp.linear.app/mcp"))
	if err != nil || res.StatusCode != 429 || res.Header.Get(PacedHeader) != "" || len(f.sent) != 1 {
		t.Fatal("a write was retried, or Linear's 429 was lost:", res, err, f.sent)
	}
	// A read whose bound can't fit Linear's Retry-After returns Linear's 429.
	p3, _ := fakePacer(OAuthPerHour)
	f = &fakeLinear{answers: []*http.Response{answer(429, headers("Retry-After", "600"))}}
	res, _ = Transport{Base: f, Pacer: p3}.RoundTrip(read(t, "https://api.linear.app/graphql"))
	if res.StatusCode != 429 || len(f.sent) != 1 {
		t.Fatal(res.StatusCode, f.sent)
	}
}

// Token requests and an MCP stream's GET pass through unpaced; the read-only
// MCP endpoint is a read, the write endpoint a write, and GraphQL a write
// unless a context marks it a read.
func TestTransportClassifies(t *testing.T) {
	get, _ := http.NewRequest(http.MethodGet, "https://mcp.linear.app/mcp", nil)
	token, _ := http.NewRequest(http.MethodPost, "https://api.linear.app/oauth/token", nil)
	for _, r := range []*http.Request{get, token} {
		if _, _, skip := classify(r); !skip {
			t.Fatal("paced:", r.Method, r.URL)
		}
	}
	for url, want := range map[string]Kind{"https://mcp.linear.app/mcp/readonly": Read, "https://mcp.linear.app/mcp": Write, "https://api.linear.app/graphql": Write} {
		r, _ := http.NewRequest(http.MethodPost, url, nil)
		if kind, _, skip := classify(r); skip || kind != want {
			t.Fatal(url, kind, skip)
		}
	}
	r, _ := http.NewRequestWithContext(WithKind(context.Background(), Read), http.MethodPost, "https://api.linear.app/graphql", nil)
	if kind, graphql, _ := classify(r); kind != Read || !graphql {
		t.Fatal(kind, graphql)
	}
	// Unpaced requests reach Linear even with the bucket held.
	p, _ := fakePacer(OAuthPerHour)
	p.Observe(429, headers("Retry-After", "600"))
	f := &fakeLinear{}
	if _, err := (Transport{Base: f, Pacer: p}).RoundTrip(token); err != nil || len(f.sent) != 1 {
		t.Fatal(err, f.sent)
	}
}

// One pacer per credential key in the process.
func TestForSharesAPacerPerCredential(t *testing.T) {
	a, b, c := For("app:test-shared", OAuthPerHour), For("app:test-shared", OAuthPerHour), For("app:test-other", OAuthPerHour)
	if a != b || a == c {
		t.Fatal("pacers not shared per key")
	}
}

// A context cancelled while waiting stops the wait, unsent.
func TestPacerWaitStopsWithTheContext(t *testing.T) {
	p := New(3600)
	p.Observe(200, headers("X-RateLimit-Requests-Remaining", "0"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok, err := p.Wait(ctx, Write, false, time.Minute); ok || !errors.Is(err, context.Canceled) {
		t.Fatal(ok, err)
	}
}

// Each admitted call takes one from the bucket.
func TestPacerSpendsOnePerCall(t *testing.T) {
	p, c := fakePacer(3600)
	p.Observe(200, headers("X-RateLimit-Requests-Remaining", "1"))
	if _, ok, _ := p.Wait(context.Background(), Write, false, time.Second); !ok || len(c.waited) != 0 {
		t.Fatal("the last token was not admitted at once:", c.waited)
	}
	if _, ok, _ := p.Wait(context.Background(), Write, false, 5*time.Second); !ok || len(c.waited) != 1 || c.waited[0] != time.Second {
		t.Fatal("a spent bucket admitted a call without the refill:", c.waited)
	}
}

// A read Linear refuses again after its one retry goes back to the
// caller: it is sent twice at most.
func TestTransportRetriesAReadOnlyOnce(t *testing.T) {
	p, _ := fakePacer(OAuthPerHour)
	f := &fakeLinear{answers: []*http.Response{answer(429, headers("Retry-After", "1")), answer(429, headers("Retry-After", "1")), answer(200, http.Header{})}}
	res, err := Transport{Base: f, Pacer: p}.RoundTrip(read(t, "https://api.linear.app/graphql"))
	if err != nil || res.StatusCode != 429 || len(f.sent) != 2 {
		t.Fatal(res, err, f.sent)
	}
}

// A counter in the context counts the calls sent, not those held.
func TestTransportCountsSentCalls(t *testing.T) {
	p, _ := fakePacer(OAuthPerHour)
	var n atomic.Int64
	ctx := WithCounter(context.Background(), &n)
	f := &fakeLinear{answers: []*http.Response{answer(200, http.Header{}), answer(429, headers("Retry-After", "600"))}}
	tr := Transport{Base: f, Pacer: p}
	tr.RoundTrip(post(t, ctx, "https://mcp.linear.app/mcp"))
	tr.RoundTrip(post(t, ctx, "https://mcp.linear.app/mcp"))
	tr.RoundTrip(post(t, ctx, "https://mcp.linear.app/mcp"))
	if n.Load() != 2 || len(f.sent) != 2 {
		t.Fatal("counted", n.Load(), "sent", len(f.sent))
	}
}
