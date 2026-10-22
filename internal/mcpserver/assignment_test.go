package mcpserver

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAssignmentToolsReachTheStoreAndFindOwners(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	exec := func(op, session string, a map[string]any) map[string]any {
		t.Helper()
		r, e := store.Execute(ctx, bus.Request{Operation: op, Workspace: "w", Project: "p", Session: session, Args: a})
		if e != nil {
			t.Fatalf("%s: %v", op, e)
		}
		m, _ := r.Data.(map[string]any)
		return m
	}
	exec("workspace_init", "", map[string]any{"mode": "local"})
	exec("project_create", "", map[string]any{"slug": "p"})
	exec("register_session", "eng", map[string]any{"instance": "eng-i"})
	exec("register_session", "coord", map[string]any{"instance": "coord-i"})
	exec("coordinator_set", "coord", map[string]any{"instance": "coord-i", "target_session": "coord"})

	server := NewServer(store, "w", "p", "eng", "eng-i")
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "assign-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil {
			t.Fatal(e)
		}
		out, _ := r.StructuredContent.(map[string]any)
		data, _ := out["data"].(map[string]any)
		return data, r.IsError
	}
	proposed, failed := call("assignment_propose", map[string]any{"issue_uuid": "0c0c0c0c-0c0c-4c0c-8c0c-0c0c0c0c0c0c", "task_role": "engineer", "scope_paths": []any{"internal/bus/**"}})
	if failed {
		t.Fatal("propose failed")
	}
	revision := proposed["assignment"].(map[string]any)["revision"].(string)
	// The MCP session is not the coordinator, so it cannot confirm itself.
	if _, failed = call("assignment_confirm", map[string]any{"revision": revision}); !failed {
		t.Fatal("non-coordinator confirmed through MCP")
	}
	exec("assignment_confirm", "coord", map[string]any{"instance": "coord-i", "target_session": "eng", "revision": revision})
	listed, failed := call("list_sessions", map[string]any{"scope_path": "internal/bus/store.go", "assignment_state": "confirmed"})
	sessions, _ := listed["sessions"].([]any)
	if failed || len(sessions) != 1 || sessions[0].(map[string]any)["name"] != "eng" {
		t.Fatal(listed)
	}
	current := sessions[0].(map[string]any)["assignment"].(map[string]any)["revision"]
	if _, failed = call("assignment_release", map[string]any{"revision": current}); failed {
		t.Fatal("own release failed")
	}
	if listed, _ = call("list_sessions", map[string]any{"scope_path": "internal/bus/store.go"}); len(listed["sessions"].([]any)) != 0 {
		t.Fatal(listed)
	}
}
