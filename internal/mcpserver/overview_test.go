package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// project_overview is a read-only tool on the pinned project and session;
// it takes no project of its own.
func TestProjectOverviewToolReadsThePinnedProject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := &recordingStore{}
	server := NewServer(store, "workspace-A", "project-A", "reviewer", "reviewer-instance")
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "overview", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, x := range tools.Tools {
		if x.Name == "project_overview" {
			tool = x
		}
	}
	if tool == nil || !tool.Annotations.ReadOnlyHint || *tool.Annotations.DestructiveHint {
		t.Fatalf("tool: %+v", tool)
	}
	if r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "project_overview", Arguments: map[string]any{}}); e != nil || r.IsError {
		t.Fatal(r, e)
	}
	if r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "project_overview", Arguments: map[string]any{"project": "project-B"}}); e == nil && !r.IsError {
		t.Fatal("accepted another project")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.calls) != 1 {
		t.Fatalf("calls: %+v", store.calls)
	}
	r := store.calls[0]
	if r.Operation != "project_overview" || r.Workspace != "workspace-A" || r.Project != "project-A" || r.Session != "reviewer" || r.Args["instance"] != "reviewer-instance" {
		t.Fatalf("%+v", r)
	}
}
