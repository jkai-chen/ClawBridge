package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// olderServer answers marker_status as a server without the read does.
type olderServer struct{}

func (olderServer) Execute(context.Context, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New(`invalid: unsupported operation "marker_status"`)
}

func markersClient(t *testing.T, ctx context.Context, store Executor) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	server, err := NewServer(store, "workspace-A", "project-A", "eng", "eng-instance").Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "markers", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// assignment_markers is a read-only tool that takes an issue UUID, an
// issue identifier or an assignment ID and calls marker_status on the pinned project and session.
func TestAssignmentMarkersTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := &recordingStore{}
	cs := markersClient(t, ctx, store)
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, x := range tools.Tools {
		if x.Name == "assignment_markers" {
			tool = x
		}
	}
	if tool == nil || !tool.Annotations.ReadOnlyHint || *tool.Annotations.DestructiveHint || !tool.Annotations.IdempotentHint || *tool.Annotations.OpenWorldHint {
		t.Fatalf("tool: %+v", tool)
	}
	for _, want := range []string{"typed fields only", "never Linear text", "issue_identifier", "no Linear read"} {
		if !strings.Contains(tool.Description, want) {
			t.Fatalf("description lacks %q: %s", want, tool.Description)
		}
	}
	raw, _ := json.Marshal(tool.InputSchema)
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if json.Unmarshal(raw, &schema) != nil || len(schema.Properties) != 3 || schema.Properties["issue_identifier"] == nil || schema.Properties["issue_uuid"] == nil || schema.Properties["assignment_id"] == nil || len(schema.Required) != 0 {
		t.Fatalf("schema: %s", raw)
	}
	issue := "00000000-0000-4000-8000-000000000001"
	for _, args := range []map[string]any{{"issue_uuid": issue}, {"assignment_id": "asg_1"}, {"issue_identifier": "ENG-1"}} {
		if r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "assignment_markers", Arguments: args}); e != nil || r.IsError {
			t.Fatal(r, e)
		}
	}
	if r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "assignment_markers", Arguments: map[string]any{"issue": "ENG-1"}}); e == nil && !r.IsError {
		t.Fatal("accepted an identifier argument")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.calls) != 3 {
		t.Fatalf("calls: %+v", store.calls)
	}
	for i, r := range store.calls {
		if r.Operation != "marker_status" || r.Workspace != "workspace-A" || r.Project != "project-A" || r.Session != "eng" || r.Args["instance"] != "eng-instance" {
			t.Fatalf("%+v", r)
		}
		if (i == 0) != (r.Args["issue_uuid"] == issue) || (i == 1) != (r.Args["assignment_id"] == "asg_1") || (i == 2) != (r.Args["issue_identifier"] == "ENG-1") {
			t.Fatalf("args: %+v", r.Args)
		}
	}
}

// Against a server without the read the tool says to upgrade the server.
func TestAssignmentMarkersToolOlderServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs := markersClient(t, ctx, olderServer{})
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "assignment_markers", Arguments: map[string]any{"issue_uuid": "00000000-0000-4000-8000-000000000001"}})
	if err != nil || !r.IsError {
		t.Fatal(r, err)
	}
	text := ""
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text += t.Text
		}
	}
	if !strings.Contains(text, "the ClawBridge server is older than this client and has no marker read; upgrade the server") {
		t.Fatal(text)
	}
}

// noIdentifierServer has marker_status without the issue_identifier arg.
type noIdentifierServer struct{}

func (noIdentifierServer) Execute(context.Context, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New(`invalid: unsupported argument "issue_identifier" for marker_status`)
}

// Such a server is asked for the issue UUID instead.
func TestAssignmentMarkersToolServerWithoutIdentifier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs := markersClient(t, ctx, noIdentifierServer{})
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "assignment_markers", Arguments: map[string]any{"issue_identifier": "ENG-1"}})
	if err != nil || !r.IsError {
		t.Fatal(r, err)
	}
	text := ""
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text += t.Text
		}
	}
	if !strings.Contains(text, "this server reads markers by issue UUID or assignment only; pass the issue UUID") {
		t.Fatal(text)
	}
}
