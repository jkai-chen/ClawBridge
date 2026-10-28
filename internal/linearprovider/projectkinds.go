package linearprovider

import (
	"context"
	"strings"
)

// Applying a project kind (PROJECT-KIND-LABELS-PLAN P3) over Linear's MCP.
// get_project names a project's labels, never their IDs, so a label is
// proven on a project only by list_projects filtered by that label's ID
// (the tester's read-only recording, 28 Oct): exactly its carriers, an
// unknown ID answering none. save_project's labels replace the set.

// labelCarrierPages bounds one label's carrier read: 10 pages of 50.
const labelCarrierPages = 10

// ProjectKindApplyReason is why the apply's tools can't be used, or "".
func (p *Provider) ProjectKindApplyReason() string {
	sample := "00000000-0000-4000-8000-000000000001"
	checks := []struct {
		tool, field string
		args        []arg
	}{
		{"list_projects", "label, fields, includeArchived, limit and cursor", []arg{a(sample, "label"), {[]string{"fields"}, []string{"uuid"}, "array"}, {[]string{"includeArchived"}, true, "boolean"}, {[]string{"limit"}, 50, "integer"}, a("c", "cursor")}},
		{"get_project", "query", []arg{a(sample, "query")}},
		{"save_project", "id and labels", []arg{a(sample, "id"), {[]string{"labels"}, []string{sample}, "array"}}},
	}
	for _, c := range checks {
		if _, _, e := p.v2Negotiation([]string{c.tool}, c.args); e != nil {
			return c.tool + " no longer accepts " + c.field
		}
	}
	return ""
}

// ProjectsWithLabel lists every project carrying label, by UUID, archived
// projects included. A read past the bound, or a cursor that doesn't move,
// is refused: never a partial set.
func (p *Provider) ProjectsWithLabel(ctx context.Context, label string) ([]string, error) {
	if !ValidID(label) {
		return nil, fail("invalid")
	}
	if p.ProjectKindApplyReason() != "" {
		return nil, fail("capability_unavailable")
	}
	var out []string
	cursor := ""
	for page := 0; page < labelCarrierPages; page++ {
		args := []arg{a(label, "label"), {[]string{"fields"}, []string{"uuid"}, "array"}, {[]string{"includeArchived"}, true, "boolean"}, {[]string{"limit"}, 50, "integer"}}
		if cursor != "" {
			args = append(args, a(cursor, "cursor"))
		}
		m, e := p.v2Call(ctx, []string{"list_projects"}, args...)
		if e != nil {
			return nil, e
		}
		rows, next, complete, e := list(m, "projects")
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			id := strings.ToLower(str(row, "uuid"))
			if !ValidID(id) {
				return nil, fail("invalid_response")
			}
			out = append(out, id)
		}
		if complete {
			return out, nil
		}
		if next == "" || next == cursor {
			return nil, fail("pagination_unavailable")
		}
		cursor = next
	}
	return nil, fail("result_limit")
}

// ProjectLabelNames is the names of project's labels, as get_project gives
// them.
func (p *Provider) ProjectLabelNames(ctx context.Context, project string) ([]string, error) {
	if !ValidID(project) {
		return nil, fail("invalid")
	}
	if p.ProjectKindApplyReason() != "" {
		return nil, fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"get_project"}, a(project, "query"))
	if e != nil {
		return nil, e
	}
	if !strings.EqualFold(str(m, "uuid"), project) {
		return nil, fail("scope_mismatch")
	}
	raw, ok := m["labels"].([]any)
	if !ok {
		return nil, fail("invalid_response")
	}
	names := []string{}
	for _, v := range raw {
		name, ok := v.(string)
		if !ok || name == "" {
			return nil, fail("invalid_response")
		}
		names = append(names, name)
	}
	return names, nil
}

// SetProjectLabels replaces project's labels with labels, by ID.
func (p *Provider) SetProjectLabels(ctx context.Context, project string, labels []string) error {
	if !ValidID(project) {
		return fail("invalid")
	}
	for _, l := range labels {
		if !ValidID(l) {
			return fail("invalid")
		}
	}
	if p.ProjectKindApplyReason() != "" {
		return fail("capability_unavailable")
	}
	_, e := p.v2Call(ctx, []string{"save_project"}, a(project, "id"), arg{[]string{"labels"}, labels, "array"})
	return e
}
