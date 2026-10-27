package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/api/idtoken"
)

// TickPath is where Cloud Scheduler calls the server's own watcher
// (live/INTAKE-DESIGN.md §2).
const TickPath = "/internal/watch/tick"

// tickDeadline bounds one tick inside the scheduler's request.
const tickDeadline = 50 * time.Second

// TickVerifier admits only the scheduler's OIDC token. It is separate from
// the user sign-in verifier, and no principal's token passes it.
type TickVerifier interface {
	VerifyTick(ctx context.Context, token string) error
}

// Ticker runs one watcher tick and reports it.
type Ticker interface {
	Tick(ctx context.Context) any
}

// singleFlight runs one tick at a time, shared by the in-process clock and
// the tick endpoint (W-L2): a tick asked for while one runs is not started.
type singleFlight struct {
	ticker  Ticker
	running atomic.Bool
	// after, when set, gets each finished tick once the flight is free.
	after func(got any)
}

// tickRunning is what a tick asked for while another runs reports.
var tickRunning = map[string]any{"running": true}

func oneAtATime(t Ticker) *singleFlight {
	if s, ok := t.(*singleFlight); ok {
		return s
	}
	return &singleFlight{ticker: t}
}

// claim reserves the next tick; run must follow a successful claim.
func (s *singleFlight) claim() bool { return s.running.CompareAndSwap(false, true) }

func (s *singleFlight) run(ctx context.Context) any {
	got := func() any {
		defer s.running.Store(false)
		return s.ticker.Tick(ctx)
	}()
	if s.after != nil {
		s.after(got)
	}
	return got
}

func (s *singleFlight) Tick(ctx context.Context) any {
	if !s.claim() {
		return tickRunning
	}
	return s.run(ctx)
}

// GoogleTickVerifier checks a Google-signed ID token minted for the
// scheduler's service account, for this tick URL.
type GoogleTickVerifier struct {
	validator      *idtoken.Validator
	audience       string
	serviceAccount string
}

// NewGoogleTickVerifier verifies tokens for audience (the tick URL) and
// serviceAccount (the scheduler's account email).
func NewGoogleTickVerifier(ctx context.Context, audience, serviceAccount string) (*GoogleTickVerifier, error) {
	if !strings.HasPrefix(audience, "https://") || !strings.HasSuffix(audience, TickPath) || !strings.HasSuffix(strings.ToLower(serviceAccount), ".gserviceaccount.com") {
		return nil, errors.New("the tick audience must be the https tick URL and the account a service account")
	}
	v, err := idtoken.NewValidator(ctx, idtoken.WithHTTPClient(&http.Client{Timeout: 8 * time.Second}))
	if err != nil {
		return nil, err
	}
	return &GoogleTickVerifier{validator: v, audience: audience, serviceAccount: strings.ToLower(serviceAccount)}, nil
}

func (v *GoogleTickVerifier) VerifyTick(ctx context.Context, token string) error {
	p, err := v.validator.Validate(ctx, token, v.audience)
	if err != nil {
		return errors.New("tick token refused")
	}
	return tickClaims(p, v.audience, v.serviceAccount, time.Now())
}

// tickClaims admits the scheduler's token only: Google's issuer, this tick
// URL as audience, the scheduler's verified service account, unexpired.
func tickClaims(p *idtoken.Payload, audience, serviceAccount string, now time.Time) error {
	if p == nil || audience == "" || serviceAccount == "" || p.Audience != audience || p.Expires <= now.Unix() ||
		(p.Issuer != "https://accounts.google.com" && p.Issuer != "accounts.google.com") {
		return errors.New("tick token refused")
	}
	email, _ := p.Claims["email"].(string)
	verified, _ := p.Claims["email_verified"].(bool)
	if !verified || strings.ToLower(email) != serviceAccount {
		return errors.New("tick token refused")
	}
	return nil
}

// newTickHandler serves TickPath: POST with the scheduler's bearer token,
// at most ips' rate per address before the token is verified (G9-L7).
func newTickHandler(verifier TickVerifier, ticker Ticker, ips *preauthLimiter) http.Handler {
	flight := oneAtATime(ticker)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			fail(w, 405, "method_not_allowed", "POST required")
			return
		}
		if !ips.allow(addressKey(r)) {
			fail(w, 429, "rate_limited", "request limit reached")
			return
		}
		headers := r.Header.Values("Authorization")
		fields := []string{}
		if len(headers) == 1 {
			fields = strings.Fields(headers[0])
		}
		if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || len(fields[1]) > 16384 {
			fail(w, 401, "unauthenticated", "the scheduler's token is required")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		err := verifier.VerifyTick(ctx, fields[1])
		cancel()
		if err != nil {
			fail(w, 401, "unauthenticated", "the scheduler's token is required")
			return
		}
		// One tick at a time: while one runs, the scheduler is told so and
		// nothing starts.
		if !flight.claim() {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(tickRunning)
			return
		}
		// The tick outlives the request (the server's write timeout is
		// shorter than a tick), within its own deadline.
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), tickDeadline)
			defer cancel()
			flight.run(ctx)
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": true})
	})
}
