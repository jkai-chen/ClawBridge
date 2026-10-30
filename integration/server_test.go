package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/server"
)

type executorAsFunc func(context.Context, string, bus.Request) (bus.Result, error)

func (f executorAsFunc) ExecuteAs(c context.Context, p string, r bus.Request) (bus.Result, error) {
	return f(c, p, r)
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestServerAuthenticationAndRequestBoundary(t *testing.T) {
	var executed atomic.Int64
	verifier := server.VerifierFunc(func(ctx context.Context, token string) (server.Identity, error) {
		if token != "synthetic-valid" {
			return server.Identity{}, errors.New("synthetic verifier rejection")
		}
		return server.Identity{Principal: pgPrincipal("alice"), Subject: pgSubject(pgPrincipal("alice"))}, nil
	})
	executor := executorAsFunc(func(ctx context.Context, principal string, r bus.Request) (bus.Result, error) {
		executed.Add(1)
		if principal != pgPrincipal("alice") {
			t.Errorf("body/header principal trusted: %q", principal)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 16*time.Second {
			t.Error("missing finite execution deadline")
		}
		if r.Operation == "secret-error" {
			return bus.Result{}, errors.New("SQL secret SQLSTATE test-password test-bearer postgres://private.invalid")
		}
		return bus.Result{Source: "server", Data: map[string]any{"principal": principal}}, nil
	})
	handler := server.NewHandler(executor, verifier)
	cases := []struct {
		name, auth, body string
		want             int
	}{
		{"missing-auth", "", `{"operation":"whoami"}`, 401},
		{"wrong-scheme", "Basic synthetic-valid", `{"operation":"whoami"}`, 401},
		{"bad-token", "Bearer bad-synthetic-token", `{"operation":"whoami"}`, 401},
		{"principal-injection", "Bearer synthetic-valid", `{"operation":"whoami","principal":"owner@example.test"}`, 400},
		{"unknown-field", "Bearer synthetic-valid", `{"operation":"whoami","skip_auth":true}`, 400},
		{"trailing-json", "Bearer synthetic-valid", `{"operation":"whoami"}{}`, 400},
		{"oversized", "Bearer synthetic-valid", `{"operation":"send","args":{"body":"` + strings.Repeat("x", 129<<10) + `"}}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := executed.Load()
			req := httptest.NewRequest("POST", "/v1/execute", strings.NewReader(tc.body))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			req.Header.Set("X-Principal", pgOwner)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if executed.Load() != before {
				t.Fatal("rejected request reached application")
			}
			if strings.Contains(rec.Body.String(), "bad-synthetic-token") {
				t.Fatal("reflected bearer token")
			}
		})
	}
	for _, op := range []string{"whoami", "secret-error"} {
		req := httptest.NewRequest("POST", "/v1/execute", strings.NewReader(`{"operation":"`+op+`"}`))
		req.Header.Set("Authorization", "Bearer synthetic-valid")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if op == "whoami" {
			if rec.Code != 200 {
				t.Fatalf("valid identity rejected: %d %s", rec.Code, rec.Body.String())
			}
			var result bus.Result
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.Source != "server" {
				t.Fatalf("missing server source: %s", rec.Body.String())
			}
		} else {
			if rec.Code < 400 {
				t.Fatal("service failure reported success")
			}
			for _, sensitive := range []string{"SQLSTATE", "test-password", "test-bearer", "postgres://"} {
				if strings.Contains(rec.Body.String(), sensitive) {
					t.Errorf("reflected private error %q", sensitive)
				}
			}
		}
	}
}

func TestPostgresHTTPRoundtripAndDenial(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p")
	verifier := server.VerifierFunc(func(ctx context.Context, token string) (server.Identity, error) {
		for _, name := range []string{"owner", "alice", "bob", "reader", "stranger"} {
			if token == "synthetic-"+name {
				return server.Identity{Principal: pgPrincipal(name), Subject: pgSubject(pgPrincipal(name))}, nil
			}
		}
		return server.Identity{}, errors.New("not a synthetic fixture token")
	})
	h := httptest.NewServer(server.NewHandler(f.store, verifier))
	defer h.Close()
	execute := func(name string, r bus.Request) (int, bus.Result, []byte) {
		t.Helper()
		raw, _ := json.Marshal(r)
		req, err := http.NewRequest("POST", h.URL+"/v1/execute", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer synthetic-"+name)
		req.Header.Set("Content-Type", "application/json")
		response, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		var result bus.Result
		if response.StatusCode == 200 {
			if err = json.Unmarshal(body, &result); err != nil {
				t.Fatal(err)
			}
			if result.Source != "server" {
				t.Fatalf("source %q", result.Source)
			}
		}
		return response.StatusCode, result, body
	}
	code, sent, body := execute("alice", f.request("send", "p", "alice", actor("alice", map[string]any{"to": "bob", "body": "real HTTP real Postgres", "operation_id": "http-send"})))
	if code != 200 {
		t.Fatalf("HTTP send code=%d: %s", code, body)
	}
	id := stringField(t, jsonData(t, sent.Data), "id")
	code, inbox, body := execute("bob", f.request("inbox", "p", "bob", actor("bob", nil)))
	if code != 200 {
		t.Fatalf("HTTP inbox code=%d: %s", code, body)
	}
	messages := pageItems(t, "messages", jsonData(t, inbox.Data))
	if len(messages) != 1 || stringField(t, messages[0], "id") != id {
		t.Fatalf("HTTP lost delivery: %v", messages)
	}
	before := f.call(pgOwner, "history", "p", "", nil)
	for _, name := range []string{"reader", "stranger"} {
		code, _, body = execute(name, f.request("send", "p", name, actor(name, map[string]any{"to": "alice", "body": "forbidden", "operation_id": "denied-" + name})))
		if code != 403 {
			t.Errorf("denied %s HTTP status=%d want403 body=%s", name, code, body)
		}
	}
	after := f.call(pgOwner, "history", "p", "", nil)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("HTTP role denial changed audit")
	}
}
