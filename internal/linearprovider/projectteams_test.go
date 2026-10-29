package linearprovider

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	ptTeamA  = "a1000000-0000-4000-8000-0000000000a1"
	ptTeamB  = "a2000000-0000-4000-8000-0000000000a2"
	ptLead   = "d1000000-0000-4000-8000-0000000000d1"
	ptMember = "d2000000-0000-4000-8000-0000000000d2"
)

// saveProjectWith is save_project with addTeams described as text, beside
// the recorded catalog's other team fields.
func saveProjectWith(addTeams map[string]any) *mcp.Tool {
	s := map[string]any{"type": "string"}
	props := map[string]any{"id": s, "name": s, "lead": s,
		"setTeams":    map[string]any{"type": "array", "items": s, "description": "Replace all teams with these. Cannot combine with addTeams/removeTeams"},
		"removeTeams": map[string]any{"type": "array", "items": s, "description": "Team name or ID to remove"}}
	if addTeams != nil {
		props["addTeams"] = addTeams
	}
	return &mcp.Tool{Name: "save_project", InputSchema: map[string]any{"type": "object", "properties": props}}
}

// An add sends save_project exactly the project's ID and the team's ID in
// addTeams, against the recorded catalog: never setTeams, removeTeams or
// any other field.
func TestAddProjectTeamSendsIDAndAddTeamsOnly(t *testing.T) {
	p, f := statusProvider(t, []*mcp.Tool{fixtureTool(t, "save-project-20261022.json")}, map[string][]any{
		"save_project": {map[string]any{"id": suProject}},
	})
	if reason := p.ProjectTeamAddReason(); reason != "" {
		t.Fatal("the recorded catalog refused:", reason)
	}
	if e := p.AddProjectTeam(context.Background(), suProject, ptTeamB); e != nil {
		t.Fatal(e)
	}
	if want := map[string]any{"id": suProject, "addTeams": []any{ptTeamB}}; len(f.args) != 1 || !reflect.DeepEqual(normal(f.args[0]), want) {
		t.Fatal(f.args)
	}
	if !reflect.DeepEqual(ProjectTeamAddFields, []string{"id", "addTeams"}) {
		t.Fatal(ProjectTeamAddFields)
	}
	// A team by name, or a project by name, is refused before any call.
	for _, c := range [][2]string{{suProject, "Platform"}, {"P-ENG-1", ptTeamB}} {
		if e := p.AddProjectTeam(context.Background(), c[0], c[1]); e == nil || len(f.args) != 1 {
			t.Fatal("sent a name:", c, e)
		}
	}
}

// normal turns a []string argument into []any, as JSON would.
func normal(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if list, ok := v.([]string); ok {
			items := []any{}
			for _, s := range list {
				items = append(items, s)
			}
			v = items
		}
		out[k] = v
	}
	return out
}

// A schema with no addTeams, only setTeams, a non-array addTeams, or an
// addTeams documented as replacing the teams, sends nothing.
func TestAddProjectTeamRefusesChangedSchemas(t *testing.T) {
	s := map[string]any{"type": "string"}
	for name, add := range map[string]map[string]any{
		"missing":         nil,
		"replacing":       {"type": "array", "items": s, "description": "Replace all teams with these"},
		"add and replace": {"type": "array", "items": s, "description": "Team name or ID to add; replaces the current teams"},
		"not an add":      {"type": "array", "items": s, "description": "Team name or ID"},
		"not a list":      {"type": "string", "description": "Team name or ID to add"},
		"not strings":     {"type": "array", "items": map[string]any{"type": "integer"}, "description": "Team name or ID to add"},
	} {
		p, f := statusProvider(t, []*mcp.Tool{saveProjectWith(add)}, nil)
		var refused *Error
		if p.ProjectTeamAddReason() == "" {
			t.Fatal(name, "accepted")
		}
		if e := p.AddProjectTeam(context.Background(), suProject, ptTeamB); !errors.As(e, &refused) || refused.Kind != "capability_unavailable" || len(f.args) != 0 {
			t.Fatal(name, e, f.args)
		}
	}
	p, _ := statusProvider(t, nil, nil)
	if p.ProjectTeamAddReason() == "" {
		t.Fatal("no save_project accepted")
	}
	// The same description as the catalog, rebuilt: accepted.
	p, _ = statusProvider(t, []*mcp.Tool{saveProjectWith(map[string]any{"type": "array", "items": s, "description": "Team name or ID to add"})}, nil)
	if reason := p.ProjectTeamAddReason(); reason != "" {
		t.Fatal(reason)
	}
}

func snapshotProject(teams []string, lead any, members []string, extra map[string]any) map[string]any {
	rows := []any{}
	for _, t := range teams {
		rows = append(rows, map[string]any{"id": t, "name": "synthetic"})
	}
	people := []any{}
	for _, m := range members {
		people = append(people, map[string]any{"id": m, "name": "synthetic"})
	}
	m := map[string]any{"id": suProject, "name": "Synthetic", "teams": rows, "lead": lead, "members": people,
		"status": map[string]any{"id": "e1000000-0000-4000-8000-000000000001", "name": "Planned"}, "startDate": "2026-11-01", "labels": []any{"x"}}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// The snapshot asks for members when get_project takes includeMembers,
// and the comparison names only what changed other than the added team.
func TestProjectSnapshotAndTeamAddChanges(t *testing.T) {
	get := tool("get_project", map[string]string{"query": "string", "includeMembers": "boolean"})
	lead := map[string]any{"id": ptLead}
	before := snapshotProject([]string{ptTeamA}, lead, []string{ptLead, ptMember}, nil)
	cases := map[string]struct {
		after map[string]any
		want  []string
	}{
		"accepted":      {snapshotProject([]string{ptTeamB, ptTeamA}, lead, []string{ptMember, ptLead}, nil), []string{}},
		"not added":     {snapshotProject([]string{ptTeamA}, lead, []string{ptLead, ptMember}, nil), []string{}},
		"lead":          {snapshotProject([]string{ptTeamA, ptTeamB}, nil, []string{ptLead, ptMember}, nil), []string{"lead"}},
		"members":       {snapshotProject([]string{ptTeamA, ptTeamB}, lead, []string{ptLead}, nil), []string{"members"}},
		"teams_removed": {snapshotProject([]string{ptTeamB}, lead, []string{ptLead, ptMember}, nil), []string{"teams_removed"}},
		"teams_added":   {snapshotProject([]string{ptTeamA, ptTeamB, "a3000000-0000-4000-8000-0000000000a3"}, lead, []string{ptLead, ptMember}, nil), []string{"teams_added"}},
		"fields":        {snapshotProject([]string{ptTeamA, ptTeamB}, lead, []string{ptLead, ptMember}, map[string]any{"startDate": nil, "targetDate": "2026-12-01", "labels": []any{"x", "y"}}), []string{"labels", "startDate", "targetDate"}},
	}
	for name, c := range cases {
		p, f := statusProvider(t, []*mcp.Tool{get}, map[string][]any{"get_project": {map[string]any{"project": before}, c.after}})
		b, e := p.ProjectSnapshot(context.Background(), suProject)
		if e != nil || f.args[0]["includeMembers"] != true || !b.MembersKnown || !b.LeadKnown || b.Lead != ptLead || b.HasTeam(ptTeamB) {
			t.Fatal(name, b, e, f.args)
		}
		a, e := p.ProjectSnapshot(context.Background(), suProject)
		if e != nil {
			t.Fatal(name, e)
		}
		if got := TeamAddChanges(b, a, ptTeamB); !reflect.DeepEqual(got, c.want) {
			t.Fatal(name, got)
		}
		if added := a.HasTeam(ptTeamB); added == (name == "not added") {
			t.Fatal(name, "HasTeam", added)
		}
	}
	// Without includeMembers the read sends only the query, and members
	// stay unknown: not a change, but unverified (F2), as is a lead one
	// read leaves out.
	p, f := statusProvider(t, []*mcp.Tool{tool("get_project", map[string]string{"query": "string"})}, map[string][]any{"get_project": {snapshotProject([]string{ptTeamA}, nil, nil, map[string]any{"members": nil})}})
	s, e := p.ProjectSnapshot(context.Background(), suProject)
	if e != nil || s.MembersKnown || !s.LeadKnown || s.Lead != "" || !reflect.DeepEqual(f.args[0], map[string]any{"query": suProject}) {
		t.Fatal(s, e, f.args)
	}
	if got := TeamAddChanges(s, s, ptTeamB); len(got) != 0 {
		t.Fatal(got)
	}
	if got := TeamAddUnverified(s, s); !reflect.DeepEqual(got, []string{"members"}) {
		t.Fatal(got)
	}
	noLead := s
	noLead.LeadKnown = false
	if got, un := TeamAddChanges(s, noLead, ptTeamB), TeamAddUnverified(s, noLead); len(got) != 0 || !reflect.DeepEqual(un, []string{"lead", "members"}) {
		t.Fatal(got, un)
	}
	// Another project's answer, a bad lead or a bad member is refused.
	for name, bad := range map[string]map[string]any{
		"scope":  snapshotProject(nil, nil, nil, map[string]any{"id": "f9000000-0000-4000-8000-000000000009"}),
		"lead":   snapshotProject(nil, map[string]any{"id": "someone"}, nil, nil),
		"member": snapshotProject(nil, nil, []string{"someone"}, nil),
	} {
		p, _ := statusProvider(t, []*mcp.Tool{get}, map[string][]any{"get_project": {bad}})
		if _, e := p.ProjectSnapshot(context.Background(), suProject); e == nil {
			t.Fatal(name, "accepted")
		}
	}
}
