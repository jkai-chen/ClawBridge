package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/bus"
)

// An older server refuses the agent_marker argument; a person-only call is
// then retried once without it. Nothing else is retried: not another
// argument, not another operation, not another refusal.
func TestOlderServerRefusingTheMarkerIsRetriedWithoutIt(t *testing.T) {
	var mu sync.Mutex
	var seen []bus.Request
	refuse := func(w http.ResponseWriter, message string) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "invalid", "message": message}})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req bus.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		_, marked := req.Args["agent_marker"]
		switch {
		case req.Args["bad"] != nil:
			refuse(w, `unsupported argument "bad" for dispatcher_enable`)
		case marked && req.Operation == "dispatcher_enable":
			refuse(w, `unsupported argument "agent_marker" for dispatcher_enable`)
		case marked && req.Operation == "lane_pin_set":
			refuse(w, `unsupported argument "something_else"`)
		case marked:
			refuse(w, `unsupported argument "agent_marker" for `+req.Operation)
		default:
			_ = json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: map[string]any{"ok": true}})
		}
	}))
	defer server.Close()
	client, err := NewWithTokenSource(Config{URL: server.URL, Audience: "audience", Principal: "alice@example.test"}, TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	run := func(op string, args map[string]any) error {
		mu.Lock()
		seen = nil
		mu.Unlock()
		_, err := client.Execute(context.Background(), bus.Request{Operation: op, Workspace: "ws_1", Project: "prj_1", Args: args})
		return err
	}
	// The refusal of the marker alone: retried once, without it, and it works.
	args := map[string]any{"expected_revision": 0, "agent_marker": ""}
	if err = run("dispatcher_enable", args); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0].Args["agent_marker"] == nil || seen[1].Args["agent_marker"] != nil || seen[1].Args["expected_revision"] == nil {
		t.Fatalf("requests: %+v", seen)
	}
	if _, still := args["agent_marker"]; !still {
		t.Fatal("the caller's args were changed")
	}
	// A non-empty marker is refused locally, whatever the server: nothing
	// is sent, for an older server (which refuses the argument) and a newer
	// one (which would refuse the operation).
	for _, op := range []string{"dispatcher_enable", "lane_pin_set"} {
		err = run(op, map[string]any{"expected_revision": 0, "agent_marker": "CODEX_SANDBOX"})
		if err == nil || len(seen) != 0 || !strings.Contains(err.Error(), "the agent marker CODEX_* is set") {
			t.Fatalf("%s with a marker: %v, %d requests", op, err, len(seen))
		}
	}
	// Another argument's refusal: not retried.
	if err = run("dispatcher_enable", map[string]any{"agent_marker": "", "bad": 1}); err == nil || len(seen) != 1 {
		t.Fatalf("another argument: %v, %d requests", err, len(seen))
	}
	// Another refusal of a person-only op: not retried.
	if err = run("lane_pin_set", map[string]any{"agent_marker": ""}); err == nil || len(seen) != 1 {
		t.Fatalf("another refusal: %v, %d requests", err, len(seen))
	}
	// An operation that isn't person-only: not retried.
	if err = run("whoami", map[string]any{"agent_marker": ""}); err == nil || len(seen) != 1 || !strings.Contains(err.Error(), "agent_marker") {
		t.Fatalf("not person-only: %v, %d requests", err, len(seen))
	}
}

// A newer server would refuse a marked person-only operation itself; the
// client refuses first, so it never sees the request, and an empty marker
// goes through untouched.
func TestNewerServerNeverSeesAMarkedPersonOnlyRequest(t *testing.T) {
	var mu sync.Mutex
	var seen []bus.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req bus.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: map[string]any{"ok": true}})
	}))
	defer server.Close()
	client, err := NewWithTokenSource(Config{URL: server.URL, Audience: "audience", Principal: "alice@example.test"}, TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	req := bus.Request{Operation: "project_kind_set", Workspace: "ws_1", Project: "prj_1", Args: map[string]any{"expected_revision": 0, "agent_marker": "CLAUDECODE"}}
	if _, err = client.Execute(context.Background(), req); err == nil || !strings.Contains(err.Error(), "the agent marker CLAUDECODE is set") || strings.Contains(err.Error(), "=") || len(seen) != 0 {
		t.Fatalf("%v, %d requests", err, len(seen))
	}
	req.Args["agent_marker"] = ""
	if _, err = client.Execute(context.Background(), req); err != nil || len(seen) != 1 {
		t.Fatalf("%v, %d requests", err, len(seen))
	}
}
