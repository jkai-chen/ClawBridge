package server

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

// panicStore panics with a value built from the request, as a careless
// handler path might.
type panicStore struct{ value any }

func (s panicStore) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	panic(s.value)
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prior := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prior) })
	return &buf
}

// A panic in a handler answers the standard 500, and neither the log nor the
// response carries the panic value or anything the request carried (G9-3).
func TestHandlerPanicNeverLogsOrReturnsASecret(t *testing.T) {
	const tokenCanary, queryCanary, bodyCanary = "CANARY-SECRET-token", "CANARY-SECRET-query", "CANARY-SECRET-body"
	verifier := VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{Principal: "ada@example.com", Subject: "s"}, nil
	})
	buf := captureLog(t)
	host := httptest.NewServer(NewHandlerWithOptions(panicStore{value: "CANARY-SECRET-xyz " + tokenCanary + queryCanary + bodyCanary}, verifier, HandlerOptions{}))
	defer host.Close()
	body := `{"operation":"whoami","workspace":"w","project":"p","session":"s","args":{"k":"` + bodyCanary + `"}}`
	req, _ := http.NewRequest(http.MethodPost, host.URL+"/v1/execute?key="+queryCanary, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenCanary)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 500 {
		t.Fatal("status", res.StatusCode, string(got))
	}
	want := `{"error":{"code":"internal","message":"` + publicMessage("internal") + `"}}`
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("body %s, want %s", got, want)
	}
	logged := buf.String()
	if !strings.Contains(logged, `panic serving POST "/v1/execute": string`) {
		t.Fatal("the log names no panic:", logged)
	}
	for _, canary := range []string{"CANARY-SECRET", tokenCanary, queryCanary, bodyCanary} {
		if strings.Contains(logged, canary) || strings.Contains(string(got), canary) {
			t.Fatalf("%q leaked:\nlog: %s\nbody: %s", canary, logged, got)
		}
	}
}

// net/http's own abort is not a crash: it passes through unchanged.
func TestHandlerPanicKeepsErrAbortHandler(t *testing.T) {
	verifier := VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{Principal: "ada@example.com", Subject: "s"}, nil
	})
	h := NewHandlerWithOptions(panicStore{value: http.ErrAbortHandler}, verifier, HandlerOptions{})
	req := httptest.NewRequest(http.MethodPost, "/v1/execute", strings.NewReader(`{"operation":"whoami","workspace":"w","project":"p","session":"s","args":{}}`))
	req.Header.Set("Authorization", "Bearer t")
	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Fatal("recovered:", r)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), req)
	t.Fatal("no panic")
}
