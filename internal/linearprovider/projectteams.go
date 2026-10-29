package linearprovider

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
)

// Adding a team to a project (FR10, live/FR10-PLAN.md): save_project
// {id, addTeams: [team]}, nothing else. addTeams appends, so a concurrent
// edit can't lose a team ClawBridge didn't read; setTeams and removeTeams
// are never sent. Linear's addTeams also takes a team's name, which Linear
// would resolve outside ClawBridge's checks, so only a team UUID is sent.

// ProjectTeamAddFields is everything an add sends: the preview's "nothing
// else changes" is this list, not free text.
var ProjectTeamAddFields = []string{"id", "addTeams"}

// ProjectTeamAddReason is why a team add is unavailable, or "": save_project
// must take id and addTeams as an array of strings documented as an add.
// A schema that renames addTeams, drops it, or documents it as replacing
// the teams switches adds off.
func (p *Provider) ProjectTeamAddReason() string {
	t := p.tools["save_project"]
	if t == nil {
		return "no save_project tool"
	}
	props, _, e := schema(t)
	if e != nil {
		return "save_project schema unreadable"
	}
	add, ok := props["addTeams"].(map[string]any)
	text, _ := add["description"].(string)
	lower := strings.ToLower(text)
	if !ok || add["type"] != "array" || !allowsArrayStrings(add) || !strings.Contains(lower, "to add") || strings.Contains(lower, "replace") {
		return "save_project schema changed: addTeams missing or no longer documented as adding teams"
	}
	sample := "00000000-0000-4000-8000-000000000001"
	if _, _, e := p.v2Negotiation([]string{"save_project"}, []arg{a(sample, "id"), {[]string{"addTeams"}, []string{sample}, "array"}}); e != nil {
		return "save_project no longer accepts id and addTeams"
	}
	return ""
}

// AddProjectTeam adds team to project by their UUIDs. The caller reads the
// project back to learn whether it took.
func (p *Provider) AddProjectTeam(ctx context.Context, project, team string) error {
	if !ValidID(project) || !ValidID(team) {
		return fail("invalid")
	}
	if p.ProjectTeamAddReason() != "" {
		return fail("capability_unavailable")
	}
	_, e := p.v2Call(ctx, []string{"save_project"}, a(project, "id"), arg{[]string{"addTeams"}, []string{team}, "array"})
	return e
}

// projectSnapshotFields are the project fields a team add must leave as
// they were, each compared as Linear returned it. Teams, the lead and the
// members are compared by ID.
var projectSnapshotFields = []string{"status", "state", "startDate", "targetDate", "labels", "priority", "name"}

// ProjectSnapshot is a project as get_project gives it, for comparing a
// team add's before and after. Member IDs are only ever compared, never
// stored or shown.
type ProjectSnapshot struct {
	ID           string
	TeamIDs      []string
	Lead         string
	LeadKnown    bool
	members      []string
	MembersKnown bool
	fields       map[string]string
}

// ProjectSnapshot reads project with its members when get_project offers
// them.
func (p *Provider) ProjectSnapshot(ctx context.Context, project string) (ProjectSnapshot, error) {
	if !ValidID(project) {
		return ProjectSnapshot{}, fail("invalid")
	}
	args := []arg{a(project, "query", "id", "projectId")}
	withMembers := append(slices.Clone(args), arg{[]string{"includeMembers"}, true, "boolean"})
	if _, _, e := p.negotiate([]string{"get_project"}, withMembers...); e == nil {
		args = withMembers
	}
	m, e := p.call(ctx, []string{"get_project"}, args...)
	if e != nil {
		return ProjectSnapshot{}, e
	}
	m = unwrap(m, "project")
	pr, e := parseProject(m)
	if e != nil {
		return ProjectSnapshot{}, e
	}
	if pr.ID != project {
		return ProjectSnapshot{}, fail("scope_mismatch")
	}
	out := ProjectSnapshot{ID: pr.ID, TeamIDs: pr.TeamIDs, fields: map[string]string{}}
	if raw, present := m["lead"]; present {
		switch v := raw.(type) {
		case nil:
			out.LeadKnown = true
		case map[string]any:
			if len(v) > 0 {
				lead, ok := v["id"].(string)
				if !ok || !ValidID(lead) {
					return ProjectSnapshot{}, fail("invalid_response")
				}
				out.Lead = lead
			}
			out.LeadKnown = true
		default:
			return ProjectSnapshot{}, fail("invalid_response")
		}
	}
	list, ok := m["members"].([]any)
	if !ok {
		if conn := obj(m, "members"); conn != nil {
			list, ok = conn["nodes"].([]any)
		}
	}
	if ok {
		for _, x := range list {
			row, _ := x.(map[string]any)
			id, _ := row["id"].(string)
			if !ValidID(id) {
				return ProjectSnapshot{}, fail("invalid_response")
			}
			out.members = append(out.members, id)
		}
		slices.Sort(out.members)
		out.MembersKnown = true
	}
	for _, key := range projectSnapshotFields {
		if raw, present := m[key]; present {
			b, _ := json.Marshal(raw)
			out.fields[key] = string(b)
		}
	}
	return out, nil
}

// HasTeam reports whether team is among the project's teams.
func (s ProjectSnapshot) HasTeam(team string) bool { return slices.Contains(s.TeamIDs, team) }

// TeamAddChanges names what differs between before and after other than
// team joining the teams: "teams_removed", "teams_added" (a team other
// than team), "lead", "members" and each changed field of
// projectSnapshotFields, sorted. Names only, never values. A field one
// read has and the other lacks is changed; one neither has is not. The
// lead and the members are compared only when both reads state them;
// otherwise TeamAddUnverified names them.
func TeamAddChanges(before, after ProjectSnapshot, team string) []string {
	changed := []string{}
	for _, t := range before.TeamIDs {
		if !after.HasTeam(t) {
			changed = append(changed, "teams_removed")
			break
		}
	}
	for _, t := range after.TeamIDs {
		if t != team && !before.HasTeam(t) {
			changed = append(changed, "teams_added")
			break
		}
	}
	if before.LeadKnown && after.LeadKnown && before.Lead != after.Lead {
		changed = append(changed, "lead")
	}
	if before.MembersKnown && after.MembersKnown && !slices.Equal(before.members, after.members) {
		changed = append(changed, "members")
	}
	for _, key := range projectSnapshotFields {
		b, bOK := before.fields[key]
		a, aOK := after.fields[key]
		if bOK != aOK || b != a {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	return changed
}

// TeamAddUnverified names the fields a team add couldn't compare because a
// read left them out: "lead" and "members", sorted. An add with any is
// never reported as leaving them unchanged.
func TeamAddUnverified(before, after ProjectSnapshot) []string {
	out := []string{}
	if !before.LeadKnown || !after.LeadKnown {
		out = append(out, "lead")
	}
	if !before.MembersKnown || !after.MembersKnown {
		out = append(out, "members")
	}
	return out
}
