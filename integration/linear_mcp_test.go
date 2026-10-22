package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"clawbridge/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func localNotificationAuthority(t *testing.T, f *fixture) *notificationAuthority {
	t.Helper()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := &notificationAuthority{t: t, db: db, workspace: "w"}
	if err := db.QueryRow("SELECT id FROM workspaces WHERE name=?", a.workspace).Scan(&a.workspaceID); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestL1MCPNativeToolsRealStorePermissionAndPinnedRoute(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	f.call("taskboard_set", "w", "p", "", l1MappingArgs(0))
	p := new(l1SyntheticProvider)
	service := l1ServiceFor(t, f.store, p)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := mcpserver.NewServerWithOptions(f.store, "w", "p", "worker", "worker-instance", mcpserver.Options{Context: ctx, Taskboard: l2LegacyOnlyTools{service}})
	client := notificationMCPConnect(t, ctx, server, make(chan string, 1))
	observer := localNotificationAuthority(t, f)
	before := l1BindingSnapshot(t, observer)
	listed, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	native := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		if len(tool.Name) > 10 && tool.Name[:10] == "taskboard_" {
			native[tool.Name] = tool
		}
	}
	for _, op := range []string{"binding", "context", "issues", "issue", "create", "update", "comment"} {
		tool := native["taskboard_"+op]
		if tool == nil {
			t.Fatalf("missing native tool %s", op)
		}
		read := op == "binding" || op == "context" || op == "issues" || op == "issue"
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != read || tool.Annotations.IdempotentHint != read {
			t.Fatalf("wrong native mutation annotation: %s", op)
		}
	}
	if len(native) != 7 {
		t.Fatalf("unexpected administrative/passthrough native tool set: count=%d", len(native))
	}
	if p.writes.Load() != 0 || !reflect.DeepEqual(before, l1BindingSnapshot(t, observer)) {
		t.Fatal("tool listing touched authority/provider mutation")
	}
	for _, field := range []string{"profile", "workspace", "project", "session", "instance", "team_id", "server", "principal"} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_create", Arguments: map[string]any{"title": "synthetic task", "operation_id": "invalid-override-" + field, field: "foreign"}})
		if err == nil && !result.IsError {
			t.Fatalf("caller routing override accepted: %s", field)
		}
		if p.writes.Load() != 0 {
			t.Fatal("invalid tool arguments dispatched mutation")
		}
	}
	args := map[string]any{"title": "synthetic task", "operation_id": "mcp-durable-once"}
	first, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_create", Arguments: args})
	if err != nil || first.IsError {
		t.Fatalf("healthy pinned MCP write failed: result=%v err=%v", first, err)
	}
	second, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_create", Arguments: args})
	if err != nil || second.IsError {
		t.Fatalf("MCP identical retry failed: %v %v", second, err)
	}
	encode := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if p.writes.Load() != 1 || encode(first.StructuredContent) != encode(second.StructuredContent) {
		t.Fatal("actual MCP retry did not return the same durable receipt exactly once")
	}
	var envelope struct {
		Source string
		Data   map[string]any
	}
	if json.Unmarshal([]byte(encode(first.StructuredContent)), &envelope) != nil || envelope.Source != "linear" || envelope.Data["id"] == nil {
		t.Fatal("MCP receipt lost Linear source/identity")
	}
	if !reflect.DeepEqual(before, l1BindingSnapshot(t, observer)) {
		t.Fatal("native tool reads/write-access checks stamped local authority")
	}
	f.call("retire_session", "w", "p", "worker", actor("worker", nil))
	for _, name := range []string{"create", "update", "comment"} {
		input := map[string]any{"operation_id": "retired-mcp-" + name, "id": l1IssueID}
		if name == "create" {
			delete(input, "id")
			input["title"] = "denied"
		} else if name == "update" {
			input["description"] = "denied"
		} else {
			input["body"] = "denied"
		}
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_" + name, Arguments: input})
		if err == nil && !result.IsError {
			t.Errorf("retired real actor accepted native %s", name)
		}
		if p.writes.Load() != 1 {
			t.Fatal("MCP retirement guard failed before provider write")
		}
	}
}
