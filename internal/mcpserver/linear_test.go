package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type boardCall struct {
	scope     bus.Request
	operation string
	args      map[string]any
	bounded   bool
}
type recordingBoard struct {
	mu    sync.Mutex
	calls []boardCall
	data  any
	err   error
}

func (p *recordingBoard) Execute(ctx context.Context, r bus.Request, op string, args map[string]any) (any, error) {
	deadline, ok := ctx.Deadline()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Reads get 45 seconds; a V2 write gets the app's write budget.
	bounded := ok && time.Until(deadline) <= 45*time.Second
	switch op {
	case "v2/create", "v2/update", "v2/comment", "v2/relation_add", "v2/relation_remove":
		bounded = ok && time.Until(deadline) > 45*time.Second && time.Until(deadline) <= linearboard.WriteTimeout
	}
	p.calls = append(p.calls, boardCall{r, op, args, bounded})
	return p.data, p.err
}
func (p *recordingBoard) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.calls) }
func boardSession(t *testing.T, p TaskboardTools) (context.Context, *mcp.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	server := NewServerWithOptions(&recordingStore{}, "pinned-workspace", "pinned-project", "pinned-actor", "pinned-instance", Options{Context: ctx, Taskboard: p})
	st, ct := mcp.NewInMemoryTransports()
	ss, e := server.Connect(ctx, st, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ss.Close() })
	cs, e := mcp.NewClient(&mcp.Implementation{Name: "native-linear-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cs.Close() })
	return ctx, cs
}
func TestLinearMCPAdditiveClosedPinnedTools(t *testing.T) {
	p := &recordingBoard{data: map[string]any{"ok": true}}
	ctx, cs := boardSession(t, p)
	listed, e := cs.ListTools(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if p.count() != 0 {
		t.Fatal("listing performed provider IO")
	}
	tools := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		tools[tool.Name] = tool
	}
	if len(tools) != 25 {
		t.Fatalf("expected legacy18 plus7, got%d", len(tools))
	}
	cases := []struct {
		op   string
		args map[string]any
		read bool
	}{
		{"binding", map[string]any{}, true}, {"context", map[string]any{}, true}, {"issues", map[string]any{"limit": 2, "cursor": "opaque"}, true}, {"issue", map[string]any{"id": "issue-id"}, true},
		{"create", map[string]any{"title": "new", "description": "body", "operation_id": "create-1"}, false}, {"update", map[string]any{"id": "issue-id", "title": "updated", "operation_id": "update-1"}, false}, {"comment", map[string]any{"id": "issue-id", "body": "comment", "operation_id": "comment-1"}, false},
	}
	for _, tc := range cases {
		tool := tools["taskboard_"+tc.op]
		if tool == nil {
			t.Fatal("missing tool", tc.op)
		}
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != tc.read || tool.Annotations.IdempotentHint != tc.read {
			t.Fatal("incorrect annotations", tc.op)
		}
		result, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: tc.args})
		if e != nil || result.IsError {
			t.Fatalf("%s: %+v %v", tc.op, result, e)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 7 {
		t.Fatal(len(p.calls))
	}
	for i, call := range p.calls {
		if call.operation != cases[i].op || call.scope.Workspace != "pinned-workspace" || call.scope.Project != "pinned-project" || call.scope.Session != "pinned-actor" || call.scope.Args["instance"] != "pinned-instance" || !call.bounded {
			t.Fatalf("scope/deadline lost %+v", call)
		}
	}
}
func TestLinearMCPRejectsOverrideNullBeforeProvider(t *testing.T) {
	p := &recordingBoard{}
	ctx, cs := boardSession(t, p)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"context", map[string]any{"profile": "foreign"}}, {"binding", map[string]any{"project": "foreign"}}, {"issues", map[string]any{"team_id": "foreign"}}, {"issue", map[string]any{"id": "x", "instance": "foreign"}},
		{"issues", map[string]any{"limit": nil}}, {"issues", map[string]any{"cursor": nil}}, {"create", map[string]any{"title": "x", "description": nil, "operation_id": "op"}}, {"update", map[string]any{"id": "x", "state_id": nil, "operation_id": "op"}}, {"comment", map[string]any{"id": "x", "body": "x", "operation_id": nil}}, {"create", map[string]any{"title": "x"}},
	} {
		t.Run(tc.name+"/"+stringJSON(tc.args), func(t *testing.T) {
			r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_" + tc.name, Arguments: tc.args})
			if e == nil && !r.IsError {
				t.Fatalf("invalid arguments accepted: %+v", tc)
			}
			if p.count() != 0 {
				t.Fatal("invalid arguments reached provider")
			}
		})
	}
}
func stringJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func TestLinearMCPKnownReceiptPreservedOnError(t *testing.T) {
	p := &recordingBoard{data: map[string]any{"id": "accepted-receipt-id", "outcome": "accepted_context_unverified"}, err: &linearboard.ReceiptError{Outcome: "accepted_context_unverified"}}
	ctx, cs := boardSession(t, p)
	r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_comment", Arguments: map[string]any{"id": "issue-id", "body": "body", "operation_id": "op"}})
	if e != nil {
		t.Fatal(e)
	}
	if !r.IsError {
		t.Fatal("uncertain context reported success")
	}
	encoded := stringJSON(r.StructuredContent)
	if !strings.Contains(encoded, "accepted-receipt-id") || !strings.Contains(encoded, "linear") {
		t.Fatalf("known receipt dropped: %s", encoded)
	}
}
func TestLinearMCPNoInventedReceiptOnOrdinaryError(t *testing.T) {
	p := &recordingBoard{err: errors.New("provider unavailable")}
	ctx, cs := boardSession(t, p)
	r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_context", Arguments: map[string]any{}})
	if e == nil && !r.IsError {
		t.Fatal("provider error reported success")
	}
	if r != nil && strings.Contains(stringJSON(r.StructuredContent), "accepted-receipt-id") {
		t.Fatal("invented receipt")
	}
}

func TestLinearMCPInitializationSuppliesOptInWorkflowWithoutProviderIO(t *testing.T) {
	_, legacy := boardSession(t, nil)
	legacyText := legacy.InitializeResult().Instructions
	if strings.Contains(legacyText, "Native Linear workflow") {
		t.Fatal("legacy server gained opt-in instructions")
	}
	p := &recordingBoard{}
	ctx, native := boardSession(t, p)
	text := native.InitializeResult().Instructions
	if !strings.HasPrefix(text, legacyText+"\n\n") {
		t.Fatal("legacy instructions changed")
	}
	for _, required := range []string{"coordinator-confirmed issue/session assignment", "assignment revision", "repository scope", "list_sessions", "immutable session IDs", "ClawBridge send/reply", "not human approval", "never automatically claims, approves or completes", "operation_id", "identical retry", "read-back", "never automatically replay"} {
		if !strings.Contains(text, required) {
			t.Fatalf("initialization missing workflow boundary %q", required)
		}
	}
	if _, err := native.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if p.count() != 0 {
		t.Fatal("initialization/listing performed native provider IO")
	}
}
