package linearprovider

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	initiativeA = "e6000000-0000-4000-8000-000000000006"
	projectX    = "e7000000-0000-4000-8000-000000000007"
)

func initiativeProvider(t *testing.T, answers map[string][]any) (*Provider, *scripted) {
	t.Helper()
	tools := []*mcp.Tool{fixtureTool(t, "save-initiative-20261023.json"), fixtureTool(t, "list-initiatives-20261023.json"), fixtureTool(t, "get-initiative-20261023.json"), fixtureTool(t, "save-project-20261022.json")}
	f := &scripted{fake: &fake{tools: tools}, answers: answers}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	if reason := p.InitiativeReason(); reason != "" {
		t.Fatal("native initiative schemas refused:", reason)
	}
	return p, f
}

func TestInitiativeCreateSendsOnlyNameAndDescription(t *testing.T) {
	p, f := initiativeProvider(t, map[string][]any{"save_initiative": {map[string]any{"id": initiativeA, "name": "Product Review"}}})
	r, e := p.CreateInitiative(context.Background(), "Product Review", "Reviews")
	if e != nil || r.ID != initiativeA {
		t.Fatal(r, e)
	}
	if want := map[string]any{"name": "Product Review", "description": "Reviews"}; !reflect.DeepEqual(f.args[0], want) {
		t.Fatal("create arguments", f.args[0])
	}
	p, f = initiativeProvider(t, map[string][]any{"save_initiative": {map[string]any{"initiative": map[string]any{"id": initiativeA, "name": "Product Review"}}}})
	if _, e = p.CreateInitiative(context.Background(), "Product Review", ""); e != nil || len(f.args[0]) != 1 {
		t.Fatal("no description sent when empty", f.args[0], e)
	}
	p, _ = initiativeProvider(t, map[string][]any{"save_initiative": {map[string]any{"id": initiativeA, "name": "Other"}}})
	var pe *Error
	if _, e = p.CreateInitiative(context.Background(), "Product Review", ""); !errors.As(e, &pe) || pe.Kind != "receipt_mismatch" {
		t.Fatal(e)
	}
}

func TestInitiativeWalkAndMembership(t *testing.T) {
	p, f := initiativeProvider(t, map[string][]any{"list_initiatives": {
		map[string]any{"initiatives": []any{map[string]any{"id": projectX, "name": "Other"}}, "hasNextPage": true, "cursor": projectX},
		map[string]any{"initiatives": []any{map[string]any{"id": initiativeA, "name": "product review"}}, "hasNextPage": false},
	}})
	found, complete, e := p.InitiativesNamed(context.Background(), "Product Review")
	if e != nil || !complete || len(found) != 1 || found[0].ID != initiativeA || f.args[0]["includeArchived"] != true || f.args[1]["cursor"] != projectX {
		t.Fatal(found, complete, e, f.args)
	}
	p, f = initiativeProvider(t, map[string][]any{"get_initiative": {map[string]any{"id": initiativeA, "projects": []any{map[string]any{"id": projectX}}}}})
	if in, e := p.InitiativeHasProject(context.Background(), initiativeA, projectX); e != nil || !in || f.args[0]["query"] != initiativeA || f.args[0]["includeProjects"] != true {
		t.Fatal(in, e, f.args)
	}
	p, _ = initiativeProvider(t, map[string][]any{"get_initiative": {map[string]any{"id": "e8000000-0000-4000-8000-000000000008", "projects": []any{map[string]any{"id": projectX}}}}})
	if in, e := p.InitiativeHasProject(context.Background(), initiativeA, projectX); e == nil || in {
		t.Fatal("another initiative answered", in, e)
	}
	p, _ = initiativeProvider(t, map[string][]any{"get_initiative": {map[string]any{"id": initiativeA}}})
	if _, e := p.InitiativeHasProject(context.Background(), initiativeA, projectX); e == nil {
		t.Fatal("an initiative without a projects field answered no")
	}
	// Linear lists the projects inline without paging fields: that is the
	// whole list, so a project not in it is not a member.
	p, _ = initiativeProvider(t, map[string][]any{"get_initiative": {map[string]any{"id": initiativeA, "projects": []any{}}}})
	if in, e := p.InitiativeHasProject(context.Background(), initiativeA, projectX); e != nil || in {
		t.Fatal("an inline empty list did not answer no", in, e)
	}
	p, _ = initiativeProvider(t, map[string][]any{"get_initiative": {map[string]any{"id": initiativeA, "projects": []any{map[string]any{"id": "e8000000-0000-4000-8000-000000000009"}}}}})
	if in, e := p.InitiativeHasProject(context.Background(), initiativeA, projectX); e != nil || in {
		t.Fatal("an inline list of other projects did not answer no", in, e)
	}
	// A list that says it continues cannot answer no.
	for _, paged := range []map[string]any{
		{"id": initiativeA, "projects": []any{}, "hasNextPage": true, "cursor": "c1"},
		{"id": initiativeA, "projects": []any{}, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": "c1"}},
	} {
		p, _ = initiativeProvider(t, map[string][]any{"get_initiative": {paged}})
		var pe *Error
		if _, e := p.InitiativeHasProject(context.Background(), initiativeA, projectX); !errors.As(e, &pe) || pe.Kind != "pagination_unavailable" {
			t.Fatal("a continued list answered", paged, e)
		}
	}
}

func TestInitiativeAddProjectIsVerifiedByReadback(t *testing.T) {
	p, f := initiativeProvider(t, map[string][]any{
		"save_project":   {map[string]any{"id": projectX, "name": "Review · Search"}},
		"get_initiative": {map[string]any{"id": initiativeA, "projects": []any{map[string]any{"id": projectX}}}},
	})
	if r, e := p.AddProjectToInitiative(context.Background(), initiativeA, projectX); e != nil || r.ID != initiativeA {
		t.Fatal(r, e)
	}
	if want := map[string]any{"id": projectX, "addInitiatives": []string{initiativeA}}; !reflect.DeepEqual(f.args[0], want) {
		t.Fatal("add-project arguments", f.args[0])
	}
	p, _ = initiativeProvider(t, map[string][]any{
		"save_project":   {map[string]any{"id": projectX}},
		"get_initiative": {map[string]any{"id": initiativeA, "projects": []any{}}},
	})
	var pe *Error
	if _, e := p.AddProjectToInitiative(context.Background(), initiativeA, projectX); !errors.As(e, &pe) || pe.Kind != "receipt_unverified" {
		t.Fatal("a membership the readback cannot see was accepted", e)
	}
}

func TestInitiativeReasonPinsTheSchemas(t *testing.T) {
	for name, drop := range map[string][2]string{"no addInitiatives": {"save_project", "addInitiatives"}, "no includeProjects": {"get_initiative", "includeProjects"}, "no description": {"save_initiative", "description"}} {
		tools := []*mcp.Tool{fixtureTool(t, "save-initiative-20261023.json"), fixtureTool(t, "list-initiatives-20261023.json"), fixtureTool(t, "get-initiative-20261023.json"), fixtureTool(t, "save-project-20261022.json")}
		for _, tool := range tools {
			if tool.Name == drop[0] {
				delete(tool.InputSchema.(map[string]any)["properties"].(map[string]any), drop[1])
			}
		}
		f := &fake{tools: tools}
		p, _ := NewWithCaller(context.Background(), f)
		var pe *Error
		if _, e := p.CreateInitiative(context.Background(), "N", ""); p.InitiativeReason() == "" || !errors.As(e, &pe) || pe.Kind != "capability_unavailable" || len(f.calls) != 0 {
			t.Fatal(name, e)
		}
	}
}
