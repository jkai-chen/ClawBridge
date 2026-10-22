package linearprovider

import (
	"context"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	liveProject    = "33333333-3333-4333-8333-333333333333"
	trashedProject = "44444444-4444-4444-8444-444444444444"
)

// listProjectsTool has the shape of Linear's live list_projects inputs.
func listProjectsTool(fieldEnum ...any) *mcp.Tool {
	fields := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	if fieldEnum != nil {
		fields["items"] = map[string]any{"type": "string", "enum": fieldEnum}
	}
	return &mcp.Tool{Name: "list_projects", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"team": map[string]any{"type": "string"}, "fields": fields, "limit": map[string]any{"type": "number", "maximum": 50},
		"cursor": map[string]any{"type": "string"}, "includeArchived": map[string]any{"type": "boolean"}, "query": map[string]any{"type": "string"},
	}}}
}

// projectRow is a list_projects row as Linear returns it: display id, UUID,
// a plain teams array and trashed null or true.
func projectRow(display, id, name string, trashed any) map[string]any {
	return map[string]any{"id": display, "uuid": id, "name": name, "trashed": trashed, "teams": []any{map[string]any{"id": tid, "name": "Synthetic", "key": "SYN"}}}
}

func TestTeamProjectsListsTrashedRowsMarkedAndSendsTheProbedArguments(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{listProjectsTool("id", "uuid", "name", "teams", "trashed", "status")}, result: map[string]any{"projects": []any{
		projectRow("P-SYN-2", liveProject, "Same name", nil),
		projectRow("P-SYN-1", trashedProject, "Same name", true),
	}, "hasNextPage": false}}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	page, e := p.TeamProjects(context.Background(), tid, PageOptions{})
	if e != nil || !page.Complete || len(page.Items) != 2 {
		t.Fatalf("%+v %v", page, e)
	}
	if got := page.Items[0]; got.ID != liveProject || got.Trashed || !reflect.DeepEqual(got.TeamIDs, []string{tid}) {
		t.Fatalf("live row %+v", got)
	}
	if got := page.Items[1]; got.ID != trashedProject || !got.Trashed {
		t.Fatalf("trashed row %+v", got)
	}
	want := map[string]any{"team": tid, "includeArchived": true, "fields": []string{"id", "uuid", "name", "teams", "trashed"}, "limit": 50}
	if !reflect.DeepEqual(f.args[0], want) {
		t.Fatalf("arguments %#v", f.args[0])
	}
}

func TestTeamProjectsPagesByTheReturnedCursor(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{listProjectsTool()}, result: map[string]any{"projects": []any{projectRow("P-SYN-2", liveProject, "A", nil)}, "hasNextPage": true, "cursor": liveProject}}
	p, _ := NewWithCaller(context.Background(), f)
	page, e := p.TeamProjects(context.Background(), tid, PageOptions{Limit: 1, Cursor: trashedProject})
	if e != nil || page.Complete || page.NextCursor != liveProject {
		t.Fatalf("%+v %v", page, e)
	}
	if f.args[0]["cursor"] != trashedProject || f.args[0]["limit"] != 1 {
		t.Fatalf("arguments %#v", f.args[0])
	}
}

func TestTeamProjectsRefusesUnexpectedShapes(t *testing.T) {
	for name, result := range map[string]map[string]any{
		"trashed not a boolean": {"projects": []any{projectRow("P-SYN-2", liveProject, "A", "yes")}, "hasNextPage": false},
		"more without a cursor": {"projects": []any{}, "hasNextPage": true},
		"no page flag":          {"projects": []any{}},
		"teams missing":         {"projects": []any{map[string]any{"id": "P-SYN-2", "uuid": liveProject, "trashed": nil}}, "hasNextPage": false},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := NewWithCaller(context.Background(), &fake{tools: []*mcp.Tool{listProjectsTool()}, result: result})
			if _, e := p.TeamProjects(context.Background(), tid, PageOptions{}); e == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestTeamProjectsNeedsEveryProbedInputAndKeepsTheConnection(t *testing.T) {
	partial := tool("list_projects", map[string]string{"team": "string", "limit": "number", "cursor": "string", "fields": "array"})
	for name, lp := range map[string]*mcp.Tool{
		"no includeArchived": partial,
		"no trashed field":   listProjectsTool("id", "uuid", "name", "teams"),
		"required query":     {Name: "list_projects", InputSchema: map[string]any{"type": "object", "properties": listProjectsTool().InputSchema.(map[string]any)["properties"], "required": []any{"query"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{tools: []*mcp.Tool{lp}}
			p, _ := NewWithCaller(context.Background(), f)
			_, e := p.TeamProjects(context.Background(), tid, PageOptions{})
			if pe, ok := e.(*Error); !ok || pe.Kind != "capability_unavailable" || len(f.calls) != 0 || p.broken.Load() {
				t.Fatalf("%v calls=%v broken=%v", e, f.calls, p.broken.Load())
			}
		})
	}
}
