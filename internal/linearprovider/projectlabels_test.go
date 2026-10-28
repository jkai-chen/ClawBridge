package linearprovider

import (
	"context"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	plGroup  = "e4000000-0000-4000-8000-000000000001"
	plChoice = "e4000000-0000-4000-8000-000000000002"
)

func projectLabelProvider(t *testing.T, answers map[string][]any) (*Provider, *scripted) {
	t.Helper()
	tools := []*mcp.Tool{fixtureTool(t, "list-project-labels-20261027.json"), fixtureTool(t, "save-project-label-20261027.json")}
	f := &scripted{fake: &fake{tools: tools}, answers: answers}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	if reason := p.ProjectLabelsReason(); reason != "" {
		t.Fatal("recorded project label schemas refused:", reason)
	}
	return p, f
}

// The recorded shapes: {hasNextPage, labels}; a row's team and parent are
// names; a retired label is marked archived.
func TestProjectLabelsReadsTheRecordedShape(t *testing.T) {
	page := map[string]any{"hasNextPage": false, "labels": []any{
		map[string]any{"id": plGroup, "name": "Type", "isGroup": true, "groupType": "singleSelect", "archivedAt": nil, "team": "Platform"},
		map[string]any{"id": plChoice, "name": "Review", "parent": "Type", "parentGroupType": "singleSelect", "team": "Platform", "retiredAt": "2026-10-27T00:00:00.000Z"},
	}}
	p, f := projectLabelProvider(t, map[string][]any{"list_project_labels": {page}})
	got, e := p.ProjectLabels(context.Background(), tid)
	want := []ProjectLabelRow{
		{ID: plGroup, Name: "Type", Group: true, GroupType: "singleSelect", TeamName: "Platform"},
		{ID: plChoice, Name: "Review", ParentName: "Type", ParentGroupType: "singleSelect", TeamName: "Platform", Archived: true},
	}
	if e != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, e)
	}
	sent := f.fake.args[len(f.fake.args)-1]
	if sent["team"] != tid || sent["includeGroups"] != true || sent["includeArchived"] != true {
		t.Fatal(sent)
	}
}

// More pages than the bound, or a page without a cursor, is refused.
func TestProjectLabelsRefusesAPartialRead(t *testing.T) {
	more := map[string]any{"hasNextPage": true, "cursor": "c", "labels": []any{}}
	p, _ := projectLabelProvider(t, map[string][]any{"list_project_labels": {more, more, more, more}})
	if _, e := p.ProjectLabels(context.Background(), ""); e == nil {
		t.Fatal("a read past 4 pages was used")
	}
	p, _ = projectLabelProvider(t, map[string][]any{"list_project_labels": {map[string]any{"hasNextPage": true, "labels": []any{}}}})
	if _, e := p.ProjectLabels(context.Background(), ""); e == nil {
		t.Fatal("a page without a cursor was used")
	}
}

// A group is made single-select on its team; a choice names its parent by
// ID; the answer's id is the new label's.
func TestProjectLabelCreates(t *testing.T) {
	p, f := projectLabelProvider(t, map[string][]any{"save_project_label": {map[string]any{"id": plGroup}, map[string]any{"id": plChoice}, map[string]any{"name": "x"}}})
	id, e := p.CreateProjectLabelGroup(context.Background(), tid, "Type")
	sent := f.fake.args[len(f.fake.args)-1]
	if e != nil || id != plGroup || sent["isGroup"] != true || sent["groupType"] != "singleSelect" || sent["teamId"] != tid || sent["name"] != "Type" {
		t.Fatal(id, e, sent)
	}
	id, e = p.CreateProjectLabelChoice(context.Background(), tid, plGroup, "Review")
	sent = f.fake.args[len(f.fake.args)-1]
	if e != nil || id != plChoice || sent["parent"] != plGroup || sent["isGroup"] != nil {
		t.Fatal(id, e, sent)
	}
	if _, e = p.CreateProjectLabelChoice(context.Background(), tid, plGroup, "Test"); e == nil {
		t.Fatal("an answer without the id was accepted")
	}
}
