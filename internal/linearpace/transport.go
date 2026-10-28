package linearpace

import (
	"context"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// PacedHeader marks a 429 the Transport answered itself, without sending.
const PacedHeader = "X-Clawbridge-Paced"

type kindKey struct{}

// WithKind marks a request's context with its kind, for a call the
// Transport can't tell by its path: a GraphQL request is a Write unless
// its sender marks it a Read.
func WithKind(ctx context.Context, k Kind) context.Context {
	return context.WithValue(ctx, kindKey{}, k)
}

type counterKey struct{}

// WithCounter counts in n every paced call sent to Linear under ctx: what
// one write spent, for its project's hourly budget.
func WithCounter(ctx context.Context, n *atomic.Int64) context.Context {
	return context.WithValue(ctx, counterKey{}, n)
}

// Transport paces every Linear API and MCP call through one credential's
// Pacer. Token and OAuth requests, and an MCP stream's GET or DELETE, pass
// through unpaced: they aren't API calls Linear counts here.
type Transport struct {
	Base  http.RoundTripper
	Pacer *Pacer
}

// classify is a request's kind and whether it is GraphQL, or skip.
func classify(r *http.Request) (kind Kind, graphql, skip bool) {
	if r.Method != http.MethodPost {
		return Read, false, true
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/graphql"):
		// A mutation must never be re-sent on a 429: GraphQL is a write
		// unless its sender says it is a read (review M1).
		kind, graphql = Write, true
	case r.URL.Path == "/mcp/readonly":
		kind = Read
	case r.URL.Path == "/mcp":
		// The write connection's reads and writes are told apart by
		// nothing cheaper than the body: all of it is paced as writes,
		// and never retried here.
		kind = Write
	default:
		return Read, false, true
	}
	if k, ok := r.Context().Value(kindKey{}).(Kind); ok {
		kind = k
	}
	return kind, graphql, false
}

func (t Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

// RoundTrip admits the call, sends it, and learns from Linear's answer. A
// call with no room within its bound is answered with a 429 that says when
// to retry and that nothing was sent. A read Linear itself refused with a
// 429 is sent once more if Linear's Retry-After fits its bound; a write
// never is: its caller decides.
func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	kind, graphql, skip := classify(r)
	if skip || t.Pacer == nil {
		return t.base().RoundTrip(r)
	}
	bound := Bounds[kind]
	for attempt := 0; ; attempt++ {
		start := t.Pacer.now()
		until, ok, err := t.Pacer.Wait(r.Context(), kind, graphql, bound)
		if err != nil {
			return nil, err
		}
		if !ok {
			return paced(r, until.Sub(t.Pacer.now())), nil
		}
		bound -= t.Pacer.now().Sub(start)
		if n, ok := r.Context().Value(counterKey{}).(*atomic.Int64); ok {
			n.Add(1)
		}
		res, err := t.base().RoundTrip(r)
		if err != nil {
			return res, err
		}
		t.Pacer.Observe(res.StatusCode, res.Header)
		if res.StatusCode != http.StatusTooManyRequests || kind != Read || attempt > 0 || r.GetBody == nil || bound <= 0 {
			return res, nil
		}
		body, err := r.GetBody()
		if err != nil {
			return res, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		res.Body.Close()
		r = r.Clone(r.Context())
		r.Body = body
	}
}

// paced is the 429 for a call the pacer held: Linear's own shape, with a
// whole-second Retry-After of at least 1.
func paced(r *http.Request, wait time.Duration) *http.Response {
	seconds := int(math.Max(1, math.Ceil(wait.Seconds())))
	body := `{"errors":[{"message":"paced by ClawBridge: Linear's rate limit for this credential has no room now; nothing was sent","extensions":{"code":"RATELIMITED"}}]}`
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Retry-After", strconv.Itoa(seconds))
	h.Set(PacedHeader, "1")
	return &http.Response{
		Status:        "429 Too Many Requests",
		StatusCode:    http.StatusTooManyRequests,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       r,
	}
}
