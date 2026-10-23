package linearprovider

import (
	"context"
	"strings"
)

// The coordinator-only admin writes: plain team labels and team projects.
// Each sends only the fields named here, never an id on create (Linear's
// save_* tools update when given one), and checks the receipt.

// AdminLabel is a label row as the admin walk reads it. TeamKnown is false
// when the row names no team: Linear's team filter returns team and
// workspace labels alike, so such a row is available to the team but its
// own scope is unknown.
type AdminLabel struct {
	ID, Name, TeamID          string
	TeamKnown, Group, Retired bool
}

// AdminReceipt is a created or renamed label, or a created project.
// Unverified names fields Linear's response left out (team, teams).
type AdminReceipt struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	TeamID     string   `json:"team_id"`
	Unverified []string `json:"unverified,omitempty"`
}

// adminLabelPages bounds the walk of one team's labels: 20 pages of 100.
const adminLabelPages = 20

// AdminReason is why the admin writes are unavailable, or "". It pins the
// fields they send. Negotiation refuses a tool whose required fields we do
// not send, so a save_issue_label or save_project that starts requiring id
// (an update) switches these writes off.
func (p *Provider) AdminReason() string {
	sample := "00000000-0000-4000-8000-000000000001"
	checks := []struct {
		tool, field string
		args        []arg
	}{
		{"save_issue_label", "name and teamId", []arg{a("x", "name"), a(sample, "teamId")}},
		{"save_project", "name and addTeams", []arg{a("x", "name"), {[]string{"addTeams"}, []string{sample}, "array"}}},
		{"list_issue_labels", "team, cursor, includeArchived and includeGroups", []arg{a(sample, "team"), a("c", "cursor"), {[]string{"limit"}, 100, "integer"}, {[]string{"includeArchived"}, true, "boolean"}, {[]string{"includeGroups"}, true, "boolean"}}},
	}
	for _, c := range checks {
		if _, _, e := p.v2Negotiation([]string{c.tool}, c.args); e != nil {
			return c.tool + " no longer accepts " + c.field
		}
	}
	if !p.teamProjectsSupported() || p.tools["get_project"] == nil {
		return "list_projects or get_project changed"
	}
	return ""
}

// TeamLabelsNamed walks every label available to team, archived and groups
// included, and returns those whose name equals name ignoring case.
// complete is false when the walk hit its bound or an unusable cursor.
func (p *Provider) TeamLabelsNamed(ctx context.Context, team, name string) ([]AdminLabel, bool, error) {
	if !ValidID(team) || name == "" {
		return nil, false, fail("invalid")
	}
	if p.AdminReason() != "" {
		return nil, false, fail("capability_unavailable")
	}
	var found []AdminLabel
	cursor := ""
	for page := 0; page < adminLabelPages; page++ {
		args := []arg{a(team, "team"), {[]string{"limit"}, 100, "integer"}, {[]string{"includeArchived"}, true, "boolean"}, {[]string{"includeGroups"}, true, "boolean"}}
		if cursor != "" {
			args = append(args, a(cursor, "cursor"))
		}
		m, e := p.v2Call(ctx, []string{"list_issue_labels"}, args...)
		if e != nil {
			return nil, false, e
		}
		rows, next, complete, e := list(m, "labels")
		if e != nil {
			return nil, false, e
		}
		for _, row := range rows {
			label, e := adminLabel(row)
			if e != nil {
				return nil, false, e
			}
			if label.TeamKnown && label.TeamID != "" && label.TeamID != team {
				return nil, false, fail("scope_mismatch")
			}
			if strings.EqualFold(label.Name, name) {
				found = append(found, label)
			}
		}
		if complete {
			return found, true, nil
		}
		if next == "" || next == cursor {
			return found, false, nil
		}
		cursor = next
	}
	return found, false, nil
}

func adminLabel(row map[string]any) (AdminLabel, error) {
	i, e := entityID(row, "labelId")
	if e != nil {
		return AdminLabel{}, e
	}
	label := AdminLabel{ID: i, Name: str(row, "name")}
	_, flat := row["teamId"]
	_, nested := row["team"]
	if flat || nested {
		if label.TeamID, e = scopedID(row, "teamId", "team", true); e != nil {
			return AdminLabel{}, e
		}
		label.TeamKnown = true
	}
	if v, ok := row["isGroup"].(bool); ok {
		label.Group = v
	}
	for _, k := range []string{"archivedAt", "retiredAt"} {
		if v, present := row[k]; present && v != nil {
			label.Retired = true
		}
	}
	if v, ok := row["archived"].(bool); ok && v {
		label.Retired = true
	}
	return label, nil
}

// CreateTeamLabel creates a plain label in team: it sends name and teamId
// only. The receipt must be a non-group label without a parent, named name,
// in team; a receipt that leaves the team out lists team as unverified.
func (p *Provider) CreateTeamLabel(ctx context.Context, team, name string) (AdminReceipt, error) {
	if !ValidID(team) || name == "" || !textOK(name, 256) {
		return AdminReceipt{}, fail("invalid")
	}
	if p.AdminReason() != "" {
		return AdminReceipt{}, fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"save_issue_label"}, a(name, "name"), a(team, "teamId"))
	if e != nil {
		return AdminReceipt{}, e
	}
	return labelReceipt(m, "", team, name)
}

// RenameTeamLabel renames the label id, which the caller proved is a plain
// label of team, to name: it sends id and name only. The receipt must name
// the same label, the new name and no other team.
func (p *Provider) RenameTeamLabel(ctx context.Context, team, id, name string) (AdminReceipt, error) {
	if !ValidID(team) || !ValidID(id) || name == "" || !textOK(name, 256) {
		return AdminReceipt{}, fail("invalid")
	}
	if p.AdminReason() != "" {
		return AdminReceipt{}, fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"save_issue_label"}, a(id, "id"), a(name, "name"))
	if e != nil {
		return AdminReceipt{}, e
	}
	return labelReceipt(m, id, team, name)
}

func labelReceipt(m map[string]any, wantID, team, name string) (AdminReceipt, error) {
	for _, key := range []string{"issueLabel", "label"} {
		if nested := obj(m, key); nested != nil {
			m = nested
			break
		}
	}
	label, e := adminLabel(m)
	if e != nil {
		return AdminReceipt{}, e
	}
	if (wantID != "" && label.ID != wantID) || label.Name != name || label.Group || label.Retired {
		return AdminReceipt{}, fail("receipt_mismatch")
	}
	if parent, present := m["parent"]; present && parent != nil {
		return AdminReceipt{}, fail("receipt_mismatch")
	}
	if v, present := m["parentId"]; present && v != nil {
		return AdminReceipt{}, fail("receipt_mismatch")
	}
	r := AdminReceipt{ID: label.ID, Name: label.Name, TeamID: team}
	switch {
	case !label.TeamKnown:
		r.Unverified = []string{"team"}
	case label.TeamID != team:
		return AdminReceipt{}, fail("scope_mismatch")
	}
	return r, nil
}

// TeamProjectsNamed walks team's projects, trashed ones included, and
// returns those named name ignoring case.
func (p *Provider) TeamProjectsNamed(ctx context.Context, team, name string) ([]ProjectListing, bool, error) {
	var found []ProjectListing
	cursor := ""
	for page := 0; page < adminLabelPages; page++ {
		got, e := p.TeamProjects(ctx, team, PageOptions{Limit: 50, Cursor: cursor})
		if e != nil {
			return nil, false, e
		}
		for _, pr := range got.Items {
			if strings.EqualFold(pr.Name, name) {
				found = append(found, pr)
			}
		}
		if got.Complete {
			return found, true, nil
		}
		if got.NextCursor == "" || got.NextCursor == cursor {
			return found, false, nil
		}
		cursor = got.NextCursor
	}
	return found, false, nil
}

// CreateTeamProject creates a project in team: it sends name and addTeams
// only. The receipt must name the project; its teams, when Linear leaves
// them out, are read back with get_project.
func (p *Provider) CreateTeamProject(ctx context.Context, team, name string) (AdminReceipt, error) {
	if !ValidID(team) || name == "" || !textOK(name, 256) {
		return AdminReceipt{}, fail("invalid")
	}
	if p.AdminReason() != "" {
		return AdminReceipt{}, fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"save_project"}, a(name, "name"), arg{[]string{"addTeams"}, []string{team}, "array"})
	if e != nil {
		return AdminReceipt{}, e
	}
	if nested := obj(m, "project"); nested != nil {
		m = nested
	}
	_, teamsShown := m["teams"]
	_, idsShown := m["teamIds"]
	var pr Project
	if teamsShown || idsShown {
		pr, e = parseProject(m)
	} else {
		pr.ID, e = entityID(m, "projectId")
		pr.Name = str(m, "name")
	}
	if e != nil {
		return AdminReceipt{}, e
	}
	if pr.Name != name {
		return AdminReceipt{}, fail("receipt_mismatch")
	}
	r := AdminReceipt{ID: pr.ID, Name: pr.Name, TeamID: team}
	if !teamsShown && !idsShown {
		read, e := p.GetProject(ctx, pr.ID)
		if e != nil || read.ID != pr.ID {
			r.Unverified = []string{"teams"}
			return r, nil
		}
		pr = read
	}
	for _, t := range pr.TeamIDs {
		if t == team {
			return r, nil
		}
	}
	return AdminReceipt{}, fail("scope_mismatch")
}
