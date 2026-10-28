package linearprovider

import (
	"context"
	"time"
)

// Project leads (live/TEAM-ROLE-MAPPING-DESIGN.md §6.1, §14, the §15 probe):
// save_project {id, lead} sets a project's lead, and lead null clears it.
// Linear also makes the lead a project member, and clearing or replacing
// the lead leaves them one (the probe's variant D); nothing here removes a
// member, and members are only ever counted.
// Linear's lead field also takes a name, an email or "me", which Linear
// would resolve outside ClawBridge's checks, so only a user UUID is ever
// sent (review); omitted fields are left as they are.

// SetProjectLead sets the project's lead to the user UUID lead, or clears
// it when lead is "". The caller reads the project back to learn whether
// it took.
func (p *Provider) SetProjectLead(ctx context.Context, project, lead string) error {
	if !ValidID(project) || lead != "" && !ValidID(lead) {
		return fail("invalid")
	}
	value := arg{[]string{"lead"}, any(lead), "string"}
	if lead == "" {
		value = arg{[]string{"lead"}, nil, "null"}
	}
	args := []arg{a(project, "id"), value}
	if _, _, e := p.v2Negotiation([]string{"save_project"}, args); e != nil {
		return fail("capability_unavailable")
	}
	_, e := p.v2Call(ctx, []string{"save_project"}, args...)
	return e
}

// ProjectLeadState is a project's lead as get_project gives it. LeadKnown
// is false when the answer leaves the lead out: unknown is never taken as
// no lead (an empty object or null is no lead, as the probe recorded).
// Created is Linear's own createdAt, zero when the answer has none.
type ProjectLeadState struct {
	ID        string    `json:"id"`
	TeamIDs   []string  `json:"team_ids"`
	Lead      string    `json:"lead,omitempty"`
	LeadKnown bool      `json:"lead_known"`
	Created   time.Time `json:"created_at,omitempty"`
	Trashed   bool      `json:"trashed,omitempty"`
	// Members is how many members the project has, when MembersKnown;
	// never who they are.
	Members      int  `json:"members"`
	MembersKnown bool `json:"members_known"`
}

// ProjectLead reads a project's lead, teams and creation time.
func (p *Provider) ProjectLead(ctx context.Context, project string) (ProjectLeadState, error) {
	if !ValidID(project) {
		return ProjectLeadState{}, fail("invalid")
	}
	args := []arg{a(project, "query", "id", "projectId")}
	withMembers := append(args, arg{[]string{"includeMembers"}, true, "boolean"})
	if _, _, e := p.negotiate([]string{"get_project"}, withMembers...); e == nil {
		args = withMembers
	}
	m, e := p.call(ctx, []string{"get_project"}, args...)
	if e != nil {
		return ProjectLeadState{}, e
	}
	m = unwrap(m, "project")
	pr, e := parseProject(m)
	if e != nil {
		return ProjectLeadState{}, e
	}
	if pr.ID != project {
		return ProjectLeadState{}, fail("scope_mismatch")
	}
	out := ProjectLeadState{ID: pr.ID, TeamIDs: pr.TeamIDs}
	if raw, present := m["lead"]; present {
		switch v := raw.(type) {
		case nil:
			out.LeadKnown = true
		case map[string]any:
			if len(v) == 0 {
				out.LeadKnown = true
				break
			}
			lead, ok := v["id"].(string)
			if !ok || !ValidID(lead) {
				return ProjectLeadState{}, fail("invalid_response")
			}
			out.Lead, out.LeadKnown = lead, true
		default:
			return ProjectLeadState{}, fail("invalid_response")
		}
	}
	if s, ok := m["createdAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			out.Created = t.UTC()
		}
	}
	out.Trashed, _ = m["trashed"].(bool)
	if list, ok := m["members"].([]any); ok {
		out.Members, out.MembersKnown = len(list), true
	} else if conn := obj(m, "members"); conn != nil {
		if nodes, ok := conn["nodes"].([]any); ok {
			out.Members, out.MembersKnown = len(nodes), true
		}
	}
	return out, nil
}
