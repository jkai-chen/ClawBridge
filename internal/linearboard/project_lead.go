package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/docpublish"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// Linear project leads from the team roles (live/TEAM-ROLE-MAPPING-DESIGN.md
// §6.1, §14, step 6.1a): the project owner, in their own terminal and
// through their own Linear profile, sets a project's lead to its team's one
// mapped lead. A project that has a lead is never changed unless the owner
// names it with --replace. The lead is sent only as the mapping's stored
// user UUID (review), and each write is read back.

// maxLeadProjects bounds one apply-leads run: each project is one read.
const maxLeadProjects = 50

// projectLeader is the provider support project leads need.
type projectLeader interface {
	ProjectLead(ctx context.Context, project string) (linearprovider.ProjectLeadState, error)
	SetProjectLead(ctx context.Context, project, lead string) error
}

// ApplyLeadsInput is one apply-leads run. Revision is the team roles'
// revision the owner read; Replace names the one project whose different
// lead may be replaced. Without Confirm it previews.
type ApplyLeadsInput struct {
	Revision int
	Replace  string
	Confirm  bool
}

// ApplyLeadsEntry is one bound project. Action is set, replace, none,
// differs (has another lead; --replace changes it) or skip (Reason says
// why); Outcome is what a confirmed run did: accepted, uncertain, drifted
// or failed.
type ApplyLeadsEntry struct {
	Project string `json:"project"`
	Lead    string `json:"lead,omitempty"`
	Current string `json:"current_lead,omitempty"`
	Action  string `json:"action"`
	Reason  string `json:"reason,omitempty"`
	// Member says what Linear does to the project's members (review).
	Member  string `json:"member_note,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// ApplyLeadsResult is what apply-leads found and, with Confirm, did.
type ApplyLeadsResult struct {
	Revision  int               `json:"revision"`
	Projects  []ApplyLeadsEntry `json:"projects"`
	Notice    string            `json:"notice"`
	Confirmed bool              `json:"confirmed"`
}

// memberNote is a set's or replace's word on the members Linear changes.
func memberNote(name string, replace bool) string {
	note := "Linear also adds " + name + " as a project member; clearing or replacing the lead doesn't remove them."
	if replace {
		note += " The current lead stays a member; ClawBridge never removes members."
	}
	return note
}

// leadNotice is the preview's word on what a set does in Linear.
// The probe (live/PROJECT-LEAD-PROBE.md, variant D) found that Linear
// makes the lead a project member, and clearing or replacing the lead
// leaves them one (no MCP tool removes a member); and that Linear sent the
// person no notification either way.
const leadNotice = "Linear makes the lead a member of the project, and replacing a lead leaves the earlier lead a member; ClawBridge can't remove members, so remove them in Linear if that's not wanted. Linear doesn't notify the person: ClawBridge sends their sessions a notice when the owner linked their lead row to their account (--link); otherwise tell them yourself."

// leadAccess opens the person's connection (for writing when confirm) and
// checks what every lead write needs: the owner without a session, the
// team roles at the revision read, and the provider's lead tools.
func (s *Service) leadAccess(ctx context.Context, scope bus.Request, revision int, write bool) (bus.TaskboardBindingV2, teamRolesMapping, map[string]string, projectLeader, func(), error) {
	var none teamRolesMapping
	if scope.Session != "" {
		return bus.TaskboardBindingV2{}, none, nil, nil, nil, errors.New("a session can't set project leads; the project owner runs clawbridge linear roles in their own terminal")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return b, none, nil, nil, nil, e
	}
	if !b.Configured {
		return b, none, nil, nil, nil, errors.New("task-board is not linked")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return b, none, nil, nil, nil, e
	}
	if access.ProjectRole != "owner" || access.SessionID != "" {
		return b, none, nil, nil, nil, errors.New("only the project owner, without a session, sets project leads")
	}
	mapping, e := s.teamRoles(ctx, scope)
	if e != nil {
		return b, none, nil, nil, nil, e
	}
	if mapping.Revision == 0 {
		return b, none, nil, nil, nil, errors.New("no team roles are set yet; set the teams' leads with clawbridge linear roles first")
	}
	if revision != mapping.Revision {
		return b, none, nil, nil, nil, fmt.Errorf("team roles changed: current revision is %d; read them again", mapping.Revision)
	}
	// Each consumer checks the lead is still an active, non-guest user of
	// the workspace when it uses the role (review L1).
	users, e := s.WorkspaceUsers(ctx, scope)
	if e != nil {
		return b, none, nil, nil, nil, e
	}
	// eligible holds each active, non-guest user's display name, for the
	// preview's member note only; it is never stored or sent.
	eligible := map[string]string{}
	for _, u := range users {
		if u.Eligible() {
			name := oneLine(u.Name)
			if name == "" {
				name = "the lead"
			}
			eligible[strings.ToLower(u.ID)] = name
		}
	}
	var p Provider
	var done func()
	if write {
		var profile linearconn.Profile
		profile, p, done, e = s.openForWrite(ctx)
		if e == nil && profile.Access != linearconn.Write {
			done()
			e = errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
		}
	} else {
		_, p, done, e = s.open(ctx)
	}
	if e != nil {
		return b, none, nil, nil, nil, e
	}
	leader, ok := p.(projectLeader)
	if !ok {
		done()
		return b, none, nil, nil, nil, errors.New("this Linear connection can't read or set project leads")
	}
	if _, e = s.verifyReadBindingV2(ctx, p, b); e != nil {
		done()
		return b, none, nil, nil, nil, e
	}
	return b, mapping, eligible, leader, done, nil
}

// teamLead is the one mapped lead of a bound team, or why there's none.
func teamLead(m teamRolesMapping, team string) (string, string) {
	for _, t := range m.Teams {
		if t.TeamID != team {
			continue
		}
		switch {
		case !t.TeamBound:
			return "", "the team left the binding"
		case len(t.Leads) == 1:
			return t.Leads[0], ""
		case len(t.Leads) > 1:
			return "", "the team has several leads"
		}
	}
	return "", "the team has no mapped lead"
}

// projectTeamLead is the lead a project's bound teams agree on.
func projectTeamLead(m teamRolesMapping, bound, teams []string) (string, string) {
	lead, why := "", "the project is in no bound team"
	for _, t := range teams {
		if !slices.Contains(bound, t) {
			continue
		}
		l, reason := teamLead(m, t)
		switch {
		case reason != "":
			return "", reason
		case lead != "" && l != lead:
			return "", "its bound teams have different leads"
		}
		lead, why = l, ""
	}
	return lead, why
}

// ApplyLeads previews, or with Confirm carries out, setting each bound
// project's lead from the team roles.
func (s *Service) ApplyLeads(ctx context.Context, scope bus.Request, in ApplyLeadsInput) (ApplyLeadsResult, error) {
	out := ApplyLeadsResult{Revision: in.Revision, Projects: []ApplyLeadsEntry{}, Notice: leadNotice}
	if in.Replace != "" && !linearprovider.ValidID(in.Replace) {
		return out, errors.New("--replace names one Linear project by UUID")
	}
	b, mapping, eligible, leader, done, e := s.leadAccess(ctx, scope, in.Revision, in.Confirm)
	if e != nil {
		return out, e
	}
	defer done()
	projects, e := s.leadCandidates(ctx, leader, b)
	if e != nil {
		return out, e
	}
	replaceFound := false
	for _, project := range projects {
		entry := ApplyLeadsEntry{Project: project}
		st, err := leader.ProjectLead(ctx, project)
		lead, why := "", ""
		if err == nil {
			lead, why = projectTeamLead(mapping, b.LinearTeamIDs, st.TeamIDs)
		}
		entry.Lead, entry.Current = lead, st.Lead
		switch {
		case err != nil:
			entry.Action, entry.Reason = "skip", "could not read the project ("+linearCode(err)+")"
		case st.Trashed:
			entry.Action, entry.Reason = "skip", "the project is in the trash"
		case !st.LeadKnown:
			entry.Action, entry.Reason = "skip", "Linear's answer didn't say who leads it"
		case why != "":
			entry.Action, entry.Reason = "skip", why
		case eligible[strings.ToLower(lead)] == "":
			entry.Action, entry.Reason = "skip", "the mapped lead isn't an active, non-guest user of the workspace"
		case st.Lead == "":
			entry.Action = "set"
		case strings.EqualFold(st.Lead, lead):
			entry.Action = "none"
		case project == in.Replace:
			entry.Action, replaceFound = "replace", true
		default:
			entry.Action, entry.Reason = "differs", "it has another lead; only --replace "+project+" changes it"
		}
		out.Projects = append(out.Projects, entry)
	}
	for i := range out.Projects {
		entry := &out.Projects[i]
		if entry.Action == "set" || entry.Action == "replace" {
			entry.Member = memberNote(eligible[strings.ToLower(entry.Lead)], entry.Action == "replace")
		}
	}
	if in.Replace != "" && !replaceFound {
		return out, errors.New("--replace names a bound project whose lead differs from its team's mapped lead")
	}
	if !in.Confirm {
		return out, nil
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return out, e
	}
	for i := range out.Projects {
		entry := &out.Projects[i]
		if entry.Action != "set" && entry.Action != "replace" {
			continue
		}
		entry.Outcome = s.writeLead(ctx, scope, leader, entry.Project, entry.Current, entry.Lead, "apply")
	}
	out.Confirmed = true
	return out, nil
}

// leadCandidates are the binding's projects: the project set's, or on a
// team board the bound teams' projects, at most maxLeadProjects.
func (s *Service) leadCandidates(ctx context.Context, p projectLeader, b bus.TaskboardBindingV2) ([]string, error) {
	if b.Mode == "project_set" {
		if len(b.LinearProjectIDs) > maxLeadProjects {
			return nil, fmt.Errorf("the binding has more than %d projects; apply-leads reads at most %d", maxLeadProjects, maxLeadProjects)
		}
		return b.LinearProjectIDs, nil
	}
	lister, ok := p.(teamProjectLister)
	if !ok {
		return nil, errors.New("this Linear connection can't list a team's projects")
	}
	seen := map[string]bool{}
	var out []string
	for _, team := range b.LinearTeamIDs {
		cursor := ""
		for page := 0; ; page++ {
			if page == maxProjectListPages {
				return nil, errors.New("a bound team has more projects than one run reads")
			}
			got, e := lister.TeamProjects(ctx, team, linearprovider.PageOptions{Limit: 50, Cursor: cursor})
			if e != nil {
				return nil, e
			}
			for _, pr := range got.Items {
				if !seen[pr.ID] {
					seen[pr.ID] = true
					out = append(out, pr.ID)
				}
			}
			if len(out) > maxLeadProjects {
				return nil, fmt.Errorf("the bound teams have more than %d projects; apply-leads reads at most %d", maxLeadProjects, maxLeadProjects)
			}
			if got.NextCursor == "" || got.NextCursor == cursor {
				break
			}
			cursor = got.NextCursor
		}
	}
	return out, nil
}

// writeLead sets one project's lead: read again first (anything changed
// since the plan isn't written), send the UUID, read back, and record the
// outcome by class. It never retries.
func (s *Service) writeLead(ctx context.Context, scope bus.Request, leader projectLeader, project, current, lead, source string) string {
	if !linearprovider.ValidID(lead) {
		return "failed"
	}
	now, e := leader.ProjectLead(ctx, project)
	if e != nil || !now.LeadKnown || !strings.EqualFold(now.Lead, current) {
		return "drifted"
	}
	outcome, after := "accepted", linearprovider.ProjectLeadState{}
	if e = leader.SetProjectLead(ctx, project, lead); e != nil {
		outcome = "uncertain"
	} else if after, e = leader.ProjectLead(ctx, project); e != nil || !strings.EqualFold(after.Lead, lead) {
		outcome = "uncertain"
	}
	s.recordLead(ctx, scope, project, lead, source, outcome, current != "", now, after)
	return outcome
}

// recordLead records a write by outcome, with the project's member counts
// before and after when Linear gave them (Linear adds the lead as a
// member), never who.
//
// The lead goes with it so the server can tell them: the server picks the
// recipient from its own mapping, never from this UUID alone. One
// operation ID per write keeps a retried record from telling them twice.
func (s *Service) recordLead(ctx context.Context, scope bus.Request, project, lead, source, outcome string, replaced bool, before, after linearprovider.ProjectLeadState) {
	scope.Operation = "project_lead_record"
	scope.Args = instanceArgs(scope.Args)
	op, e := docpublish.NewToken()
	if e != nil {
		op = "x"
	}
	for k, v := range map[string]any{"linear_project": project, "lead": lead, "operation_id": "lead-" + project + "-" + op, "source": source, "outcome": outcome, "replaced": replaced} {
		scope.Args[k] = v
	}
	if before.MembersKnown && after.MembersKnown {
		scope.Args["members_before"], scope.Args["members_after"] = before.Members, after.Members
	}
	_, _ = s.cfg.Executor.Execute(ctx, scope)
}

// ProjectLeadSetting is project_lead_get's answer.
type ProjectLeadSetting struct {
	Revision int    `json:"revision"`
	Mode     string `json:"mode"`
	OnAt     string `json:"on_at,omitempty"`
}

// ProjectLeadSettingGet reads whether project leads are on.
func (s *Service) ProjectLeadSettingGet(ctx context.Context, scope bus.Request) (ProjectLeadSetting, error) {
	scope.Operation = "project_lead_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return ProjectLeadSetting{}, e
	}
	var out ProjectLeadSetting
	raw, _ := json.Marshal(r.Data)
	if json.Unmarshal(raw, &out) != nil {
		return out, errors.New("the project lead setting is unreadable")
	}
	return out, nil
}

// CreateLead is what project create does about the new project's lead:
// with project leads on, the team's one mapped lead, or the one --lead
// names among several; with none, nothing (Note says why).
type CreateLead struct {
	Lead     string `json:"lead,omitempty"`
	Revision int    `json:"revision,omitempty"`
	Note     string `json:"note,omitempty"`
}

// PlanCreateLead decides a new project's lead before it is created, so a
// missing --lead refuses the create rather than following it. pick is the
// --lead user's ID, or "".
func (s *Service) PlanCreateLead(ctx context.Context, scope bus.Request, team, pick string) (CreateLead, error) {
	setting, e := s.ProjectLeadSettingGet(ctx, scope)
	if e != nil && strings.Contains(e.Error(), `unsupported operation "project_lead_get"`) && pick == "" {
		// An older server has no project leads: create as before.
		return CreateLead{}, nil
	}
	if e != nil {
		return CreateLead{}, e
	}
	if setting.Mode != "on" {
		if pick != "" {
			return CreateLead{}, errors.New("--lead needs project leads on (clawbridge linear roles project-lead on)")
		}
		return CreateLead{Note: "project leads are off; the project is created without a lead"}, nil
	}
	mapping, e := s.teamRoles(ctx, scope)
	if e != nil {
		return CreateLead{}, e
	}
	var leads []string
	for _, t := range mapping.Teams {
		if t.TeamID == strings.ToLower(team) && t.TeamBound {
			leads = t.Leads
		}
	}
	out := CreateLead{Revision: mapping.Revision}
	switch {
	case pick != "":
		if !slices.Contains(leads, strings.ToLower(pick)) {
			return CreateLead{}, errors.New("--lead must be one of the team's mapped leads (clawbridge linear roles show)")
		}
		out.Lead = strings.ToLower(pick)
	case len(leads) == 1:
		out.Lead = leads[0]
	case len(leads) > 1:
		return CreateLead{}, errors.New("the team has several mapped leads; name one with --lead")
	default:
		out.Note = "the team has no mapped lead; the project is created without one"
	}
	return out, nil
}

// SetCreatedProjectLead sets a project ClawBridge just created to the lead
// PlanCreateLead chose, at the same team roles revision, and reads it back.
func (s *Service) SetCreatedProjectLead(ctx context.Context, scope bus.Request, project string, plan CreateLead) (string, error) {
	if plan.Lead == "" {
		return "", nil
	}
	_, _, eligible, leader, done, e := s.leadAccess(ctx, scope, plan.Revision, true)
	if e != nil {
		return "", e
	}
	defer done()
	if eligible[plan.Lead] == "" {
		return "", errors.New("the mapped lead isn't an active, non-guest user of the workspace; the project has no lead")
	}
	return s.writeLead(ctx, scope, leader, project, "", plan.Lead, "create"), nil
}

// CreateLeadResult is the lead part of a project create's answer.
type CreateLeadResult struct {
	Lead    string `json:"lead,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Note    string `json:"note,omitempty"`
	Member  string `json:"member_note,omitempty"`
}

// LeadForCreatedProject finishes a create: only a project this run made
// (accepted) gets the planned lead, never one that was already there.
func (s *Service) LeadForCreatedProject(ctx context.Context, scope bus.Request, r *AdminResult, plan CreateLead) {
	if plan.Lead == "" {
		if plan.Note != "" && !strings.Contains(plan.Note, "off") {
			r.Lead = &CreateLeadResult{Note: plan.Note}
		}
		return
	}
	out := &CreateLeadResult{Lead: plan.Lead}
	r.Lead = out
	if r.Outcome != "accepted" || !linearprovider.ValidID(r.ID) {
		out.Note = "the project wasn't created by this run, so its lead isn't changed"
		return
	}
	outcome, e := s.SetCreatedProjectLead(ctx, scope, r.ID, plan)
	if e != nil {
		out.Outcome, out.Note = "failed", e.Error()
		return
	}
	out.Outcome = outcome
	out.Member = "Linear also adds the lead as a project member; clearing or replacing the lead doesn't remove them."
}
