package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/idtoken"
)

const (
	tickURL     = "https://bus.example.test" + TickPath
	schedulerSA = "watch-tick@example-project.iam.gserviceaccount.com"
)

// Only the scheduler's token for this tick URL passes (IN-M1): a person's
// verified token, an owner's included, does not.
func TestTickClaimsAdmitOnlyTheScheduler(t *testing.T) {
	now := time.Now()
	good := func() *idtoken.Payload {
		return &idtoken.Payload{Issuer: "https://accounts.google.com", Audience: tickURL, Expires: now.Add(time.Minute).Unix(), Subject: "1", Claims: map[string]any{"email": schedulerSA, "email_verified": true}}
	}
	if err := tickClaims(good(), tickURL, schedulerSA, now); err != nil {
		t.Fatal("the scheduler was refused:", err)
	}
	for name, mutate := range map[string]func(*idtoken.Payload){
		"a person's token":     func(p *idtoken.Payload) { p.Claims["email"] = "owner@example.test" },
		"unverified email":     func(p *idtoken.Payload) { p.Claims["email_verified"] = false },
		"another audience":     func(p *idtoken.Payload) { p.Audience = "https://bus.example.test/v1/execute" },
		"another issuer":       func(p *idtoken.Payload) { p.Issuer = "https://evil.example.test" },
		"expired":              func(p *idtoken.Payload) { p.Expires = now.Add(-time.Second).Unix() },
		"another service acct": func(p *idtoken.Payload) { p.Claims["email"] = "other@example-project.iam.gserviceaccount.com" },
	} {
		p := good()
		mutate(p)
		if err := tickClaims(p, tickURL, schedulerSA, now); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	if _, err := NewGoogleTickVerifier(context.Background(), "https://bus.example.test/v1/execute", schedulerSA); err == nil {
		t.Fatal("a non-tick audience was configured")
	}
	if _, err := NewGoogleTickVerifier(context.Background(), tickURL, "owner@example.test"); err == nil {
		t.Fatal("a person was configured as the scheduler")
	}
}

type tickCheck struct{ good string }

func (c tickCheck) VerifyTick(_ context.Context, token string) error {
	if token != c.good {
		return errors.New("refused")
	}
	return nil
}

// countTicks reports each tick on its channel.
type countTicks struct{ ticks chan struct{} }

func (c *countTicks) Tick(context.Context) any { c.ticks <- struct{}{}; return []string{"p1"} }

func (c *countTicks) count(wait time.Duration) int {
	n := 0
	deadline := time.After(wait)
	for {
		select {
		case <-c.ticks:
			n++
		case <-deadline:
			return n
		}
	}
}

func TestTickEndpoint(t *testing.T) {
	ticker := &countTicks{ticks: make(chan struct{}, 16)}
	host := httptest.NewServer(NewHandlerWithOptions(&fakeIntake{}, nil, HandlerOptions{TickVerifier: tickCheck{"scheduler-token"}, Ticker: ticker}))
	defer host.Close()
	do := func(method, token string) int {
		req, _ := http.NewRequest(method, host.URL+TickPath, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if do(http.MethodPost, "") != 401 || do(http.MethodPost, "a-users-token") != 401 || do(http.MethodGet, "scheduler-token") != 405 {
		t.Fatal("a refused caller was answered")
	}
	if n := ticker.count(100 * time.Millisecond); n != 0 {
		t.Fatal("a refused caller ticked", n)
	}
	// The tick runs after the answer: the server's write timeout is shorter
	// than a tick.
	if do(http.MethodPost, "scheduler-token") != http.StatusAccepted {
		t.Fatal("the scheduler was not accepted")
	}
	if n := ticker.count(time.Second); n != 1 {
		t.Fatal("the scheduler did not tick", n)
	}
	// Without a verifier and a ticker there is no tick endpoint.
	plain := httptest.NewServer(NewHandlerWithOptions(&fakeIntake{}, nil, HandlerOptions{Ticker: ticker}))
	defer plain.Close()
	res, err := http.Post(plain.URL+TickPath, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatal("an unconfigured tick endpoint answered", res.StatusCode)
	}
}

func TestWatchSettings(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	if w, err := readWatchSettings(env()); err != nil || w.wanted() {
		t.Fatal("no settings should leave the watcher off:", w, err)
	}
	w, err := readWatchSettings(env("CLAWBRIDGE_WATCH_INTERVAL", "2m", "CLAWBRIDGE_WATCH_LEDGER_DIR", "/var/lib/clawbridge-watcher"))
	if err != nil || w.interval != 2*time.Minute || !w.wanted() || w.tickAudience != "" {
		t.Fatal(w, err)
	}
	for name, e := range map[string]func(string) string{
		"interval too short":      env("CLAWBRIDGE_WATCH_INTERVAL", "10s", "CLAWBRIDGE_WATCH_LEDGER_DIR", "/x"),
		"interval not a duration": env("CLAWBRIDGE_WATCH_INTERVAL", "2", "CLAWBRIDGE_WATCH_LEDGER_DIR", "/x"),
		"no ledger directory":     env("CLAWBRIDGE_WATCH_INTERVAL", "2m"),
		"half the tick settings":  env("CLAWBRIDGE_TICK_AUDIENCE", tickURL, "CLAWBRIDGE_WATCH_LEDGER_DIR", "/x"),
	} {
		if _, err := readWatchSettings(e); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The in-process clock ticks until its context ends, never overlapping.
func TestWatchLoopTicksUntilStopped(t *testing.T) {
	ticker := &countTicks{ticks: make(chan struct{}, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runWatchLoop(ctx, ticker, 10*time.Millisecond); close(done) }()
	if n := ticker.count(200 * time.Millisecond); n < 3 {
		t.Fatal("the clock did not tick:", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the clock did not stop")
	}
}

// Without the app wiring the watcher stays off, and the server still serves.
func TestWatcherStaysOffWithoutTheApp(t *testing.T) {
	saved := NewWatcher
	NewWatcher = nil
	defer func() { NewWatcher = saved }()
	opts := startWatcher(context.Background(), nil, watchSettings{interval: time.Minute, ledgerDirectory: t.TempDir()}, HandlerOptions{})
	if opts.Ticker != nil || opts.TickVerifier != nil {
		t.Fatal("a watcher started without the app")
	}
}
