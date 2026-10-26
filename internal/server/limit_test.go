package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Heartbeats spend their own budget: a principal at its request limit still
// renews its sessions, and heartbeats never spend the request limit. A
// malformed request spends the principal's.
func TestHeartbeatsHaveTheirOwnBudget(t *testing.T) {
	verifier := VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{Principal: "ada@example.com", Subject: "s"}, nil
	})
	post := func(host *httptest.Server, body string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, host.URL+"/v1/execute", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	const whoami = `{"operation":"whoami","workspace":"w","project":"p","session":"s","args":{}}`
	const heartbeat = `{"operation":"heartbeat","workspace":"w","project":"p","session":"s","args":{}}`
	host := httptest.NewServer(NewHandlerWithOptions(okStore{}, verifier, HandlerOptions{}))
	defer host.Close()
	for i := 0; i < 1200; i++ {
		if code := post(host, whoami); code != 200 {
			t.Fatal(i, code)
		}
	}
	if code := post(host, whoami); code != 429 {
		t.Fatal("the request limit did not hold:", code)
	}
	for i := 0; i < 240; i++ {
		if code := post(host, heartbeat); code != 200 {
			t.Fatal("heartbeat", i, code)
		}
	}
	if code := post(host, heartbeat); code != 429 {
		t.Fatal("the heartbeat limit did not hold:", code)
	}

	malformed := httptest.NewServer(NewHandlerWithOptions(okStore{}, verifier, HandlerOptions{}))
	defer malformed.Close()
	for i := 0; i < 1200; i++ {
		if code := post(malformed, `{"operation":"heartbeat",`); code != 400 {
			t.Fatal(i, code)
		}
	}
	if code := post(malformed, whoami); code != 429 {
		t.Fatal("malformed requests spent nothing:", code)
	}
}
