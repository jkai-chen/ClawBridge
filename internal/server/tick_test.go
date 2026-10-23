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

type countTicks struct{ n int }

func (c *countTicks) Tick(context.Context) any { c.n++; return []string{"p1"} }

func TestTickEndpoint(t *testing.T) {
	ticker := &countTicks{}
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
	if do(http.MethodPost, "") != 401 || do(http.MethodPost, "a-users-token") != 401 || do(http.MethodGet, "scheduler-token") != 405 || ticker.n != 0 {
		t.Fatal("a refused caller ticked", ticker.n)
	}
	if do(http.MethodPost, "scheduler-token") != 200 || ticker.n != 1 {
		t.Fatal("the scheduler did not tick", ticker.n)
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
