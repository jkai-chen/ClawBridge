package linearprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	adminLabelA   = "e1000000-0000-4000-8000-000000000001"
	adminLabelB   = "e2000000-0000-4000-8000-000000000002"
	adminProjectA = "e3000000-0000-4000-8000-000000000003"
)

// fixtureTool loads a tool object saved verbatim from the 2026-10-22
// catalog captures.
func fixtureTool(t *testing.T, file string) *mcp.Tool {
	t.Helper()
	raw, e := os.ReadFile("testdata/" + file)
	if e != nil {
		t.Fatal(e)
	}
	var tool mcp.Tool
	if e = json.Unmarshal(raw, &tool); e != nil {
		t.Fatal(e)
	}
	return &tool
}

func adminTools(t *testing.T) []*mcp.Tool {
	return []*mcp.Tool{fixtureTool(t, "save-issue-label-20261022.json"), fixtureTool(t, "save-project-20261022.json"), nativeLabelList(), listProjectsTool(), tool("get_project", map[string]string{"query": "string"})}
}

// scripted answers each call by tool name from a queue.
type scripted struct {
	*fake
	answers map[string][]any
}

func (f *scripted) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	queue := f.answers[p.Name]
	if len(queue) == 0 {
		return nil, errors.New("unexpected call to " + p.Name)
	}
	f.fake.result, f.answers[p.Name] = queue[0], queue[1:]
	return f.fake.CallTool(ctx, p)
}

func adminProvider(t *testing.T, answers map[string][]any) (*Provider, *scripted) {
	t.Helper()
	f := &scripted{fake: &fake{tools: adminTools(t)}, answers: answers}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	if reason := p.AdminReason(); reason != "" {
		t.Fatal("native admin schemas refused:", reason)
	}
	return p, f
}

func labelRow(id, name string, extra map[string]any) map[string]any {
	row := map[string]any{"id": id, "name": name, "color": "#000000"}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

func TestAdminLabelCreateSendsOnlyNameAndTeam(t *testing.T) {
	p, f := adminProvider(t, map[string][]any{"save_issue_label": {labelRow(adminLabelA, "Bug", map[string]any{"team": map[string]any{"id": tid}, "isGroup": false, "parent": nil})}})
	r, e := p.CreateTeamLabel(context.Background(), tid, "Bug")
	if e != nil || r.ID != adminLabelA || r.TeamID != tid || r.Unverified != nil {
		t.Fatal(r, e)
	}
	if want := map[string]any{"name": "Bug", "teamId": tid}; !reflect.DeepEqual(f.args[0], want) {
		t.Fatal("create sent more than name and teamId", f.args[0])
	}
	// A receipt without a team leaves the team unverified.
	p, _ = adminProvider(t, map[string][]any{"save_issue_label": {labelRow(adminLabelA, "Bug", nil)}})
	if r, e = p.CreateTeamLabel(context.Background(), tid, "Bug"); e != nil || !reflect.DeepEqual(r.Unverified, []string{"team"}) {
		t.Fatal("teamless receipt", r, e)
	}
	for name, c := range map[string]struct {
		row  map[string]any
		kind string
	}{
		"another team": {labelRow(adminLabelA, "Bug", map[string]any{"teamId": "99999999-9999-4999-8999-999999999999"}), "scope_mismatch"},
		"another name": {labelRow(adminLabelA, "Bugs", nil), "receipt_mismatch"},
		"a group":      {labelRow(adminLabelA, "Bug", map[string]any{"isGroup": true}), "receipt_mismatch"},
		"a parent":     {labelRow(adminLabelA, "Bug", map[string]any{"parent": map[string]any{"id": adminLabelB}}), "receipt_mismatch"},
		"retired":      {labelRow(adminLabelA, "Bug", map[string]any{"retiredAt": "2026-10-23T00:00:00Z"}), "receipt_mismatch"},
	} {
		p, _ := adminProvider(t, map[string][]any{"save_issue_label": {c.row}})
		var pe *Error
		if _, e := p.CreateTeamLabel(context.Background(), tid, "Bug"); !errors.As(e, &pe) || pe.Kind != c.kind {
			t.Fatal(name, e)
		}
	}
}

func TestAdminLabelRenameSendsTheUUIDAndName(t *testing.T) {
	p, f := adminProvider(t, map[string][]any{"save_issue_label": {labelRow(adminLabelA, "Bug old", map[string]any{"teamId": tid})}})
	if r, e := p.RenameTeamLabel(context.Background(), tid, adminLabelA, "Bug old"); e != nil || r.ID != adminLabelA {
		t.Fatal(r, e)
	}
	if want := map[string]any{"id": adminLabelA, "name": "Bug old"}; !reflect.DeepEqual(f.args[0], want) {
		t.Fatal("rename arguments", f.args[0])
	}
	p, _ = adminProvider(t, map[string][]any{"save_issue_label": {labelRow(adminLabelB, "Bug old", map[string]any{"teamId": tid})}})
	var pe *Error
	if _, e := p.RenameTeamLabel(context.Background(), tid, adminLabelA, "Bug old"); !errors.As(e, &pe) || pe.Kind != "receipt_mismatch" {
		t.Fatal("a receipt for another label accepted", e)
	}
}

// The walk reads every page, archived labels and groups included, and
// matches names ignoring case.
func TestAdminLabelWalkIsCompleteAndCaseBlind(t *testing.T) {
	p, f := adminProvider(t, map[string][]any{"list_issue_labels": {
		map[string]any{"labels": []any{labelRow(adminLabelA, "bug", nil), labelRow(adminLabelB, "Other", nil)}, "hasNextPage": true, "cursor": adminLabelB},
		map[string]any{"labels": []any{labelRow(adminProjectA, "BUG", map[string]any{"isGroup": true, "teamId": tid})}, "hasNextPage": false},
	}})
	found, complete, e := p.TeamLabelsNamed(context.Background(), tid, "Bug")
	if e != nil || !complete || len(found) != 2 || found[0].TeamKnown || !found[1].Group || !found[1].TeamKnown {
		t.Fatal(found, complete, e)
	}
	if f.args[0]["includeArchived"] != true || f.args[0]["includeGroups"] != true || f.args[0]["team"] != tid || f.args[1]["cursor"] != adminLabelB {
		t.Fatal("walk arguments", f.args)
	}
	p, _ = adminProvider(t, map[string][]any{"list_issue_labels": {map[string]any{"labels": []any{}, "hasNextPage": true}}})
	if _, complete, e = p.TeamLabelsNamed(context.Background(), tid, "Bug"); e == nil && complete {
		t.Fatal("a walk without a cursor counted as complete", complete, e)
	}
}

func TestAdminProjectCreateSendsOnlyNameAndTeam(t *testing.T) {
	p, f := adminProvider(t, map[string][]any{"save_project": {map[string]any{"id": adminProjectA, "name": "Q4", "teams": []any{map[string]any{"id": tid}}}}})
	r, e := p.CreateTeamProject(context.Background(), tid, "Q4")
	if e != nil || r.ID != adminProjectA || r.Unverified != nil {
		t.Fatal(r, e)
	}
	if want := map[string]any{"name": "Q4", "addTeams": []string{tid}}; !reflect.DeepEqual(f.args[0], want) {
		t.Fatal("project create arguments", f.args[0])
	}
	// No teams in the receipt: read back with get_project.
	p, f = adminProvider(t, map[string][]any{"save_project": {map[string]any{"id": adminProjectA, "name": "Q4"}}, "get_project": {map[string]any{"id": adminProjectA, "name": "Q4", "teams": []any{map[string]any{"id": tid}}}}})
	if r, e = p.CreateTeamProject(context.Background(), tid, "Q4"); e != nil || r.Unverified != nil || f.calls[1] != "get_project" {
		t.Fatal("readback", r, e, f.calls)
	}
	p, _ = adminProvider(t, map[string][]any{"save_project": {map[string]any{"id": adminProjectA, "name": "Q4", "teams": []any{map[string]any{"id": "99999999-9999-4999-8999-999999999999"}}}}})
	var pe *Error
	if _, e = p.CreateTeamProject(context.Background(), tid, "Q4"); !errors.As(e, &pe) || pe.Kind != "scope_mismatch" {
		t.Fatal("a project in another team accepted", e)
	}
}

// The admin writes are off when a tool stops accepting the fields they send
// or starts requiring id (an update).
func TestAdminReasonPinsTheNativeSchemas(t *testing.T) {
	for name, change := range map[string]func([]*mcp.Tool){
		"id required on labels": func(tools []*mcp.Tool) {
			tools[0].InputSchema.(map[string]any)["required"] = []any{"id"}
		},
		"no teamId": func(tools []*mcp.Tool) {
			delete(tools[0].InputSchema.(map[string]any)["properties"].(map[string]any), "teamId")
		},
		"no addTeams": func(tools []*mcp.Tool) {
			delete(tools[1].InputSchema.(map[string]any)["properties"].(map[string]any), "addTeams")
		},
		"id required on projects": func(tools []*mcp.Tool) {
			tools[1].InputSchema.(map[string]any)["required"] = []any{"name", "id"}
		},
		"no includeGroups": func(tools []*mcp.Tool) {
			delete(tools[2].InputSchema.(map[string]any)["properties"].(map[string]any), "includeGroups")
		},
	} {
		tools := adminTools(t)
		change(tools)
		f := &fake{tools: tools}
		p, _ := NewWithCaller(context.Background(), f)
		var pe *Error
		if p.AdminReason() == "" {
			t.Fatal(name, "accepted")
		}
		if _, e := p.CreateTeamLabel(context.Background(), tid, "Bug"); !errors.As(e, &pe) || pe.Kind != "capability_unavailable" || len(f.calls) != 0 {
			t.Fatal(name, "label create ran", e)
		}
	}
}
