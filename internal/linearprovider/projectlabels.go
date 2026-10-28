package linearprovider

import (
	"context"
	"strings"
)

// Project labels over Linear's MCP, as the project kind seed reads and
// makes them (PROJECT-KIND-LABELS-PLAN P2). The shapes are the recorded
// ones: list_project_labels answers {hasNextPage, labels}; a row names its
// team and its parent group by name, never by ID; a team-scoped read lists
// the workspace's labels and that team's; save_project_label answers the
// new label's id.

// projectLabelPages bounds a read: 4 pages of 250. Past it the read is
// refused, never used in part.
const projectLabelPages = 4

// ProjectLabelRow is one project label as list_project_labels gives it.
type ProjectLabelRow struct {
	ID, Name string
	Group    bool
	// GroupType is a group's singleSelect or multiSelect.
	GroupType string
	// ParentName and ParentGroupType are a choice's group, by name.
	ParentName, ParentGroupType string
	// TeamName is the label's team, by name; "" for a workspace label.
	TeamName string
	// Archived is an archived or a retired label.
	Archived bool
}

// ProjectLabelsReason is why the project label tools can't be used, or "".
func (p *Provider) ProjectLabelsReason() string {
	sample := "00000000-0000-4000-8000-000000000001"
	checks := []struct {
		tool, field string
		args        []arg
	}{
		{"list_project_labels", "team, cursor, limit, includeArchived and includeGroups", []arg{a(sample, "team"), a("c", "cursor"), {[]string{"limit"}, 250, "integer"}, {[]string{"includeArchived"}, true, "boolean"}, {[]string{"includeGroups"}, true, "boolean"}}},
		{"save_project_label", "name, teamId, isGroup, groupType and parent", []arg{a("x", "name"), a(sample, "teamId"), {[]string{"isGroup"}, true, "boolean"}, a("singleSelect", "groupType")}},
		{"save_project_label", "parent", []arg{a("x", "name"), a(sample, "teamId"), a(sample, "parent")}},
	}
	for _, c := range checks {
		if _, _, e := p.v2Negotiation([]string{c.tool}, c.args); e != nil {
			return c.tool + " no longer accepts " + c.field
		}
	}
	return ""
}

// ProjectLabels reads every project label a read shows: with team, the
// workspace's and that team's; without, the workspace's only. Archived and
// retired labels and groups are included, marked.
func (p *Provider) ProjectLabels(ctx context.Context, team string) ([]ProjectLabelRow, error) {
	if team != "" && !ValidID(team) {
		return nil, fail("invalid")
	}
	if p.ProjectLabelsReason() != "" {
		return nil, fail("capability_unavailable")
	}
	var out []ProjectLabelRow
	cursor := ""
	for page := 0; page < projectLabelPages; page++ {
		args := []arg{{[]string{"limit"}, 250, "integer"}, {[]string{"includeArchived"}, true, "boolean"}, {[]string{"includeGroups"}, true, "boolean"}}
		if team != "" {
			args = append(args, a(team, "team"))
		}
		if cursor != "" {
			args = append(args, a(cursor, "cursor"))
		}
		m, e := p.v2Call(ctx, []string{"list_project_labels"}, args...)
		if e != nil {
			return nil, e
		}
		rows, next, complete, e := list(m, "labels")
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			l, e := projectLabelRow(row)
			if e != nil {
				return nil, e
			}
			out = append(out, l)
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

func projectLabelRow(row map[string]any) (ProjectLabelRow, error) {
	id := strings.ToLower(str(row, "id"))
	name := str(row, "name")
	if !ValidID(id) || name == "" {
		return ProjectLabelRow{}, fail("invalid_response")
	}
	l := ProjectLabelRow{ID: id, Name: name, GroupType: str(row, "groupType"), ParentName: str(row, "parent"), ParentGroupType: str(row, "parentGroupType"), TeamName: str(row, "team")}
	l.Group, _ = row["isGroup"].(bool)
	l.Archived = str(row, "archivedAt") != "" || str(row, "retiredAt") != ""
	return l, nil
}

// CreateProjectLabelGroup makes a single-select project-label group on team
// and answers its ID.
func (p *Provider) CreateProjectLabelGroup(ctx context.Context, team, name string) (string, error) {
	return p.saveProjectLabel(ctx, a(name, "name"), a(team, "teamId"), arg{[]string{"isGroup"}, true, "boolean"}, a("singleSelect", "groupType"))
}

// CreateProjectLabelChoice makes a choice of the group parent on team and
// answers its ID.
func (p *Provider) CreateProjectLabelChoice(ctx context.Context, team, parent, name string) (string, error) {
	if !ValidID(parent) {
		return "", fail("invalid")
	}
	return p.saveProjectLabel(ctx, a(name, "name"), a(team, "teamId"), a(parent, "parent"))
}

func (p *Provider) saveProjectLabel(ctx context.Context, args ...arg) (string, error) {
	team, _ := args[1].value.(string)
	name, _ := args[0].value.(string)
	if !ValidID(team) || strings.TrimSpace(name) == "" {
		return "", fail("invalid")
	}
	if p.ProjectLabelsReason() != "" {
		return "", fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"save_project_label"}, args...)
	if e != nil {
		return "", e
	}
	id := strings.ToLower(str(m, "id"))
	if !ValidID(id) {
		return "", fail("receipt_mismatch")
	}
	return id, nil
}
