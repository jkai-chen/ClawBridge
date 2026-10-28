// Package linearpace paces ClawBridge's calls to Linear per credential
// (live/LINEAR-QUEUE-DESIGN.md §3). Linear limits each user, or app user,
// with a leaky bucket of requests and of complexity points an hour, and says
// what is left in every response's headers
// (https://linear.app/developers/rate-limiting). A Pacer keeps an estimate
// of that bucket, refilled at the documented rate and corrected by each
// response, and admits a call only when the bucket has room. A call that
// would wait past its bound is never sent: the Transport answers it with a
// 429 of its own, so every caller handles it as Linear's own "not sent,
// retry after".
package linearpace

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Linear's documented hourly request limits per credential.
const (
	OAuthPerHour  = 5000
	APIKeyPerHour = 2500
)

const (
	// writeReserve is the share of the bucket reads leave for writes, so
	// a storm of reads can't starve them.
	writeReserve = 0.10
	// complexityFloor is the most one query may cost (Linear refuses
	// more): below it left, a GraphQL call waits for the complexity reset.
	complexityFloor = 10000
	// maxBackoff caps the wait after a 429 that names no Retry-After.
	maxBackoff = 60 * time.Second
)

// Kind is a call's kind: a read or a write.
type Kind int

const (
	Read Kind = iota
	Write
)

// Bounds are how long a call may wait for room before it is answered
// "not sent": reads up to 20 s, writes up to 5 s (a write that can't go
// soon is queued by its caller instead).
var Bounds = map[Kind]time.Duration{Read: 20 * time.Second, Write: 5 * time.Second}

// Pacer is one credential's bucket. It is safe for concurrent use.
type Pacer struct {
	mu   sync.Mutex
	now  func() time.Time
	wait func(ctx context.Context, d time.Duration) error

	perHour float64
	limit   float64
	tokens  float64
	updated time.Time
	// complexityLeft, complexityLimit and complexityReset are Linear's
	// complexity bucket as last reported, refilled at complexityLimit an
	// hour; complexityKnown says whether it was.
	complexityLeft  float64
	complexityLimit float64
	complexityReset time.Time
	complexityKnown bool
	// blockedUntil holds every call after a 429 until Linear's Retry-After.
	blockedUntil time.Time
	backoff      time.Duration
}

// New is a pacer for a credential Linear allows perHour requests an hour.
// It starts full, as an unused leaky bucket is: Linear's first answer
// corrects it, and a 429 holds every call.
func New(perHour int) *Pacer {
	return &Pacer{now: time.Now, wait: sleep, perHour: float64(perHour), limit: float64(perHour), tokens: float64(perHour)}
}

var (
	registryMu sync.Mutex
	registry   = map[string]*Pacer{}
)

// For is the process's one pacer for a credential key (app:…, profile:…,
// key:…), made on first use: every client of that credential in this
// process shares it.
func For(key string, perHour int) *Pacer {
	registryMu.Lock()
	defer registryMu.Unlock()
	p, ok := registry[key]
	if !ok {
		p = New(perHour)
		registry[key] = p
	}
	return p
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

// refill brings the estimate up to now at Linear's constant rate.
func (p *Pacer) refill(now time.Time) {
	if !p.updated.IsZero() && now.After(p.updated) {
		elapsed := now.Sub(p.updated).Seconds()
		p.tokens = math.Min(p.limit, p.tokens+elapsed*p.perHour/3600)
		if p.complexityLimit > 0 {
			p.complexityLeft = math.Min(p.complexityLimit, p.complexityLeft+elapsed*p.complexityLimit/3600)
		}
	}
	p.updated = now
}

// room is how long until the bucket admits one call of kind, 0 for now.
func (p *Pacer) room(now time.Time, kind Kind, graphql bool) time.Duration {
	var d time.Duration
	if now.Before(p.blockedUntil) {
		d = p.blockedUntil.Sub(now)
	}
	floor := 1.0
	if kind == Read {
		floor += writeReserve * p.limit
	}
	if p.tokens < floor && p.perHour > 0 {
		d = max(d, time.Duration((floor-p.tokens)/p.perHour*3600*float64(time.Second)))
	}
	if graphql && p.complexityKnown && p.complexityLeft < complexityFloor {
		// A leaky bucket refills at Limit an hour: wait for the floor, not
		// for Reset, which is when it is full again (review M2). Without a
		// Limit, Reset is all there is.
		c := time.Duration(0)
		if now.Before(p.complexityReset) {
			c = p.complexityReset.Sub(now)
		}
		if p.complexityLimit > 0 {
			refill := time.Duration((complexityFloor - p.complexityLeft) / p.complexityLimit * 3600 * float64(time.Second))
			if c == 0 || refill < c {
				c = refill
			}
		}
		d = max(d, c)
	}
	return d
}

// Wait admits one call of kind, waiting for room up to bound. Past it, it
// sends nothing and returns when the call could go.
func (p *Pacer) Wait(ctx context.Context, kind Kind, graphql bool, bound time.Duration) (time.Time, bool, error) {
	for {
		p.mu.Lock()
		now := p.now()
		p.refill(now)
		d := p.room(now, kind, graphql)
		if d <= 0 {
			p.tokens--
			p.mu.Unlock()
			return time.Time{}, true, nil
		}
		p.mu.Unlock()
		if deadline, ok := ctx.Deadline(); ok {
			bound = min(bound, time.Until(deadline))
		}
		if d > bound {
			return now.Add(d), false, nil
		}
		if err := p.wait(ctx, d); err != nil {
			return time.Time{}, false, err
		}
		bound -= d
	}
}

// Observe corrects the estimate from one response's headers, and after a
// 429 holds every call until Linear's Retry-After (or a doubling backoff,
// up to a minute, when it names none).
func (p *Pacer) Observe(status int, h http.Header) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.refill(now)
	if limit, ok := number(h, "X-RateLimit-Requests-Limit"); ok && limit > 0 {
		p.limit, p.perHour = limit, limit
	}
	// Remaining doesn't count the calls still in flight that were admitted
	// before this answer: until their answers come, the pacer may admit a
	// few more than Linear has room for (L1). Linear's own 429 then holds
	// every call, so the over-admission is bounded by the calls in flight.
	if left, ok := number(h, "X-RateLimit-Requests-Remaining"); ok {
		p.tokens = math.Min(p.limit, math.Max(0, left))
	}
	if limit, ok := number(h, "X-RateLimit-Complexity-Limit"); ok && limit > 0 {
		p.complexityLimit = limit
	}
	if left, ok := number(h, "X-RateLimit-Complexity-Remaining"); ok {
		p.complexityLeft, p.complexityKnown = left, true
		if reset, ok := number(h, "X-RateLimit-Complexity-Reset"); ok {
			p.complexityReset = time.UnixMilli(int64(reset))
		}
	}
	if status != http.StatusTooManyRequests {
		p.backoff = 0
		return
	}
	wait := time.Duration(0)
	if s, ok := number(h, "Retry-After"); ok && s >= 0 {
		wait = time.Duration(s * float64(time.Second))
	} else if at, err := http.ParseTime(h.Get("Retry-After")); err == nil {
		// Retry-After may be an HTTP date (L3).
		wait = max(0, at.Sub(now))
	} else {
		p.backoff = min(maxBackoff, max(2*time.Second, 2*p.backoff))
		wait = p.backoff
	}
	// Linear's Retry-After, with its Remaining when it sends one, says when
	// to go: the estimate is left to them.
	p.blockedUntil = maxTime(p.blockedUntil, now.Add(wait))
}

func number(h http.Header, name string) (float64, bool) {
	v := h.Get(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(v, 64)
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
