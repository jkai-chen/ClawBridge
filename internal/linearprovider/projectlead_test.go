package linearprovider

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const leadUser = "a1000000-0000-4000-8000-00000000000a"

// A lead is sent only as a user UUID with the project's ID, and a clear as
// null; a name, an email or "me" is refused before anything is sent.
func TestSetProjectLeadSendsTheUUIDOnly(t *testing.T) {
	p, f := statusProvider(t, []*mcp.Tool{fixtureTool(t, "save-project-20261022.json")}, map[string][]any{
		"save_project": {map[string]any{"id": suProject}, map[string]any{"id": suProject}, map[string]any{"id": suProject}, map[string]any{"id": suProject}, map[string]any{"id": suProject}, map[string]any{"id": suProject}},
	})
	if e := p.SetProjectLead(context.Background(), suProject, leadUser); e != nil {
		t.Fatal(e)
	}
	if e := p.SetProjectLead(context.Background(), suProject, ""); e != nil {
		t.Fatal(e)
	}
	if len(f.args) != 2 || !reflect.DeepEqual(f.args[0], map[string]any{"id": suProject, "lead": leadUser}) || !reflect.DeepEqual(f.args[1], map[string]any{"id": suProject, "lead": nil}) {
		t.Fatal(f.args)
	}
	for _, who := range []string{"me", "Ada Lovelace", "ada@example.test", "A1000000-0000-4000-8000-00000000000A "} {
		if e := p.SetProjectLead(context.Background(), suProject, who); e == nil {
			t.Fatal("sent", who)
		}
	}
	if len(f.args) != 2 {
		t.Fatal("a refused lead was sent", f.args)
	}
	p, f = statusProvider(t, fakeStatusTools([]any{"project"}, []any{"onTrack"}), nil)
	var refused *Error
	if e := p.SetProjectLead(context.Background(), suProject, leadUser); !errors.As(e, &refused) || refused.Kind != "capability_unavailable" || len(f.args) != 0 {
		t.Fatal("set without save_project:", e, f.args)
	}
}

// get_project's lead, as the probe recorded it: {} is no lead, {id} a
// lead, null no lead; left out is unknown, never no lead.
func TestProjectLeadReadsTheShapes(t *testing.T) {
	project := func(lead any, withLead bool) map[string]any {
		m := map[string]any{"id": "P-ABC-1", "uuid": suProject, "teams": []any{map[string]any{"id": "b1000000-0000-4000-8000-00000000000b", "key": "ABC", "name": "x"}}, "createdAt": "2026-10-26T16:10:16.843Z"}
		if withLead {
			m["lead"] = lead
		}
		return m
	}
	tools := []*mcp.Tool{tool("get_project", map[string]string{"query": "string"})}
	for name, c := range map[string]struct {
		answer    map[string]any
		lead      string
		known, ok bool
	}{
		"empty object": {project(map[string]any{}, true), "", true, true},
		"null":         {project(nil, true), "", true, true},
		"a lead":       {project(map[string]any{"id": leadUser, "name": "Ada"}, true), leadUser, true, true},
		"left out":     {project(nil, false), "", false, true},
		"a name":       {project(map[string]any{"id": "Ada"}, true), "", false, false},
		"a string":     {project(leadUser, true), "", false, false},
	} {
		p, _ := statusProvider(t, tools, map[string][]any{"get_project": {c.answer}})
		got, e := p.ProjectLead(context.Background(), suProject)
		if c.ok != (e == nil) || got.Lead != c.lead || got.LeadKnown != c.known {
			t.Fatal(name, got, e)
		}
		if c.ok && (!got.Created.Equal(time.Date(2026, 10, 26, 16, 10, 16, 843e6, time.UTC)) || len(got.TeamIDs) != 1) {
			t.Fatal(name, got)
		}
	}
	// With includeMembers in the tool, members are asked for and counted.
	withMembers := []*mcp.Tool{tool("get_project", map[string]string{"query": "string", "includeMembers": "boolean"})}
	answer := project(map[string]any{}, true)
	answer["members"] = []any{map[string]any{"id": leadUser, "name": "Ada", "email": "ada@example.test"}}
	p, f := statusProvider(t, withMembers, map[string][]any{"get_project": {answer}})
	got, e := p.ProjectLead(context.Background(), suProject)
	if e != nil || got.Members != 1 || !got.MembersKnown || f.args[0]["includeMembers"] != true {
		t.Fatal(got, e, f.args)
	}
	p, f = statusProvider(t, tools, map[string][]any{"get_project": {project(map[string]any{}, true)}})
	if got, e = p.ProjectLead(context.Background(), suProject); e != nil || got.MembersKnown || len(f.args[0]) != 1 {
		t.Fatal("without includeMembers", got, e, f.args)
	}
}
