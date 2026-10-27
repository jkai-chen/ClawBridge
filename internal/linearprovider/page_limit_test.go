package linearprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A projects page above 50 is refused before anything is sent: Linear's
// list_projects refuses 51 (provider_rejected). Teams still take 100.
func TestProjectsPageIsAtMostFifty(t *testing.T) {
	tool := &mcp.Tool{Name: "list_projects", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"limit": map[string]any{"type": "number"}, "cursor": map[string]any{"type": "string"},
	}}}
	f := &fake{tools: []*mcp.Tool{tool}, result: map[string]any{"projects": []any{}, "hasNextPage": false}}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	var pe *Error
	if _, e = p.Projects(context.Background(), PageOptions{Limit: MaxProjectsPage + 1}); !errors.As(e, &pe) || pe.Kind != "invalid" || len(f.args) != 0 {
		t.Fatal("a 51-project page:", e, f.args)
	}
	if _, e = p.Projects(context.Background(), PageOptions{Limit: MaxProjectsPage}); e != nil || len(f.args) != 1 || f.args[0]["limit"] != MaxProjectsPage {
		t.Fatal(e, f.args)
	}
}

// A number outside the bounds the tool's own schema declares is refused
// locally and never sent.
func TestSchemaBoundsAreKeptBeforeSending(t *testing.T) {
	tool := &mcp.Tool{Name: "list_teams", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"limit": map[string]any{"type": "number", "minimum": float64(1), "maximum": float64(25)}, "cursor": map[string]any{"type": "string"},
	}}}
	f := &fake{tools: []*mcp.Tool{tool}, result: map[string]any{"teams": []any{}, "hasNextPage": false}}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	var pe *Error
	if _, e = p.Teams(context.Background(), PageOptions{Limit: 26}); !errors.As(e, &pe) || pe.Kind != "invalid" || !pe.NotSent || len(f.args) != 0 {
		t.Fatal("past the schema's maximum:", e, f.args)
	}
	if _, e = p.Teams(context.Background(), PageOptions{Limit: 25}); e != nil || len(f.args) != 1 {
		t.Fatal(e, f.args)
	}
}

func TestWithinSchemaBounds(t *testing.T) {
	prop := map[string]any{"type": "number", "minimum": float64(5), "maximum": float64(25)}
	for n, want := range map[int]bool{4: false, 5: true, 25: true, 26: false} {
		if withinSchemaBounds(prop, n) != want {
			t.Fatal(n)
		}
	}
	if !withinSchemaBounds(map[string]any{"type": "number"}, 1000) || !withinSchemaBounds(nil, 1) {
		t.Fatal("no bounds declared")
	}
}
