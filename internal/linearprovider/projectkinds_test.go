package linearprovider

import (
	"context"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The apply's tools, hand-written in the shape the tester recorded:
// list_projects takes a label filter and fields; get_project a query.
func kindApplyTools(t *testing.T) []*mcp.Tool {
	listProjects := &mcp.Tool{Name: "list_projects", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"label": map[string]any{"type": "string"}, "team": map[string]any{"type": "string"}, "fields": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"limit": map[string]any{"type": "number", "maximum": 50}, "cursor": map[string]any{"type": "string"}, "includeArchived": map[string]any{"type": "boolean"}, "query": map[string]any{"type": "string"},
	}}}
	return []*mcp.Tool{listProjects, tool("get_project", map[string]string{"query": "string"}), fixtureTool(t, "save-project-20261022.json")}
}

func kindApplyProvider(t *testing.T, answers map[string][]any) (*Provider, *scripted) {
	t.Helper()
	f := &scripted{fake: &fake{tools: kindApplyTools(t)}, answers: answers}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	if reason := p.ProjectKindApplyReason(); reason != "" {
		t.Fatal("apply schemas refused:", reason)
	}
	return p, f
}

const (
	pkProjectA = "c0c0c0c0-0000-4000-8000-00000000000a"
	pkProjectB = "c0c0c0c0-0000-4000-8000-00000000000b"
	pkLabel    = "c0c0c0c0-0000-4000-8000-0000000000c1"
)

// A label's carriers come back by UUID across pages, the filter by ID with
// archived projects included; a page without a cursor, or a stuck one, is
// refused.
func TestProjectsWithLabel(t *testing.T) {
	page1 := map[string]any{"hasNextPage": true, "cursor": "c2", "projects": []any{map[string]any{"uuid": pkProjectA}}}
	page2 := map[string]any{"hasNextPage": false, "projects": []any{map[string]any{"uuid": pkProjectB}}}
	p, f := kindApplyProvider(t, map[string][]any{"list_projects": {page1, page2}})
	got, e := p.ProjectsWithLabel(context.Background(), pkLabel)
	if e != nil || !reflect.DeepEqual(got, []string{pkProjectA, pkProjectB}) {
		t.Fatal(got, e)
	}
	first := f.fake.args[0]
	if first["label"] != pkLabel || first["includeArchived"] != true || !reflect.DeepEqual(first["fields"], []any{"uuid"}) && !reflect.DeepEqual(first["fields"], []string{"uuid"}) {
		t.Fatal(first)
	}
	if f.fake.args[1]["cursor"] != "c2" {
		t.Fatal(f.fake.args[1])
	}
	stuck := map[string]any{"hasNextPage": true, "cursor": "c", "projects": []any{}}
	p, _ = kindApplyProvider(t, map[string][]any{"list_projects": {stuck, stuck}})
	if _, e = p.ProjectsWithLabel(context.Background(), pkLabel); e == nil {
		t.Fatal("a stuck cursor was used")
	}
	p, _ = kindApplyProvider(t, map[string][]any{"list_projects": {map[string]any{"hasNextPage": false, "projects": []any{map[string]any{"id": "P-TEAM-1"}}}}})
	if _, e = p.ProjectsWithLabel(context.Background(), pkLabel); e == nil {
		t.Fatal("a row without its UUID was used")
	}
	if _, e = p.ProjectsWithLabel(context.Background(), "Test"); e == nil {
		t.Fatal("a label name was sent as a filter")
	}
}

// get_project names the labels; its uuid must be the project asked for.
func TestProjectLabelNames(t *testing.T) {
	project := map[string]any{"id": "P-TEAM-1", "uuid": pkProjectA, "name": "Alpha", "labels": []any{"Review", "Customer"}}
	p, _ := kindApplyProvider(t, map[string][]any{"get_project": {project, map[string]any{"uuid": pkProjectB, "labels": []any{}}}})
	got, e := p.ProjectLabelNames(context.Background(), pkProjectA)
	if e != nil || !reflect.DeepEqual(got, []string{"Review", "Customer"}) {
		t.Fatal(got, e)
	}
	if _, e = p.ProjectLabelNames(context.Background(), pkProjectA); e == nil {
		t.Fatal("another project's answer was used")
	}
}

// save_project sends the whole set by ID.
func TestSetProjectLabels(t *testing.T) {
	p, f := kindApplyProvider(t, map[string][]any{"save_project": {map[string]any{"id": "P-TEAM-1"}}})
	if e := p.SetProjectLabels(context.Background(), pkProjectA, []string{pkLabel}); e != nil {
		t.Fatal(e)
	}
	sent := f.fake.args[0]
	if sent["id"] != pkProjectA || len(sent) != 2 {
		t.Fatal(sent)
	}
	if e := p.SetProjectLabels(context.Background(), pkProjectA, []string{"Review"}); e == nil {
		t.Fatal("a label name was sent")
	}
}
