package mcpserver

import (
	"context"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recordingStore struct {
	mu    sync.Mutex
	calls []bus.Request
}

func (s *recordingStore) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r)
	if r.Operation == "register_session" {
		return bus.Result{Source: "local-authority", Data: map[string]any{"instance": "registered-instance"}}, nil
	}
	return bus.Result{Source: "local-authority", Data: map[string]any{"ok": true}}, nil
}

func TestPinnedScopeRegistrationAndOverrideRejection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := &recordingStore{}
	server := NewServer(store, "workspace-A", "project-A", "reviewer", "")
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "integration", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 17 {
		t.Fatalf("got %d tools", len(tools.Tools))
	}
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	if r := call("register_session", map[string]any{"name": "reviewer", "role": "reviewer"}); r.IsError {
		t.Fatalf("registration failed: %+v", r)
	}
	if r := call("send", map[string]any{"to": "lead", "body": "ready", "operation_id": "attempt-1"}); r.IsError {
		t.Fatalf("send failed: %+v", r)
	}
	store.mu.Lock()
	if len(store.calls) != 2 {
		t.Fatalf("calls: %d", len(store.calls))
	}
	r := store.calls[1]
	store.mu.Unlock()
	if r.Workspace != "workspace-A" || r.Project != "project-A" || r.Session != "reviewer" || r.Args["instance"] != "registered-instance" {
		t.Fatalf("lost pinned identity: %+v", r)
	}
	for _, bad := range []map[string]any{
		{"to": "lead", "body": "ready", "operation_id": "2", "project": "project-B"},
		{"to": "lead", "body": "ready", "operation_id": "3", "instance": "someone-else"},
	} {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "send", Arguments: bad})
		if err == nil && !r.IsError {
			t.Fatal("accepted caller scope/instance override")
		}
	}
	if r := call("register_session", map[string]any{"name": "other"}); !r.IsError {
		t.Fatal("accepted different registration name")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.calls) != 2 {
		t.Fatal("rejected scope override reached application")
	}
}

func TestCoordinationMCPRejectsExplicitNullAndEmptyOptionalMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := &recordingStore{}
	server := NewServer(store, "workspace", "project", "actor", "instance")
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "coord-test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	valid := map[string]any{"issue_id": "11111111-2222-3333-4444-555555555555", "assignment_revision": "revision", "intent": "request"}
	for _, invalid := range []any{nil, map[string]any{}, map[string]any{"issue_id": valid["issue_id"], "assignment_revision": "revision", "intent": "request", "request_id": nil}, map[string]any{"issue_id": valid["issue_id"], "assignment_revision": "revision", "intent": "request", "source_commit": ""}} {
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "send", Arguments: map[string]any{"to": "peer", "body": "request", "operation_id": "bad", "coordination": invalid}})
		if err == nil && !result.IsError {
			t.Fatal("malformed metadata accepted", invalid)
		}
	}
	store.mu.Lock()
	calls := len(store.calls)
	store.mu.Unlock()
	if calls != 0 {
		t.Fatal("malformed metadata reached store", calls)
	}
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "send", Arguments: map[string]any{"to": "peer", "body": "request", "operation_id": "valid", "coordination": valid}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.calls) != 1 || store.calls[0].Args["coordination"] == nil {
		t.Fatal(store.calls)
	}
}
