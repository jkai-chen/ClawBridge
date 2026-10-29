package bus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A Linear project's plan fields (priority, target date, start date) set
// through ClawBridge (FR13, live/FR13-PLAN.md §2 and r1). The client reads
// the project, previews current → planned, and asks the server who may
// confirm: project_plan_reserve without confirm checks everything and
// records nothing. A confirm is reserved here before anything reaches
// Linear (a linear_write_reservations row of kind project_plan: subject the
// Linear project, detail the planned field names) and settled after the
// read-back. The values are never stored; plan_sha256 binds them. The
// server decides who may confirm, from the stored sessions, roles, lane
// pins and binding, never from the client:
//
//   - without a session: the project owner (owner), a project-role lead
//     (project_lead), or a team-roles lead of a bound write team the
//     project holds (team_lead);
//   - with a session: the project's designated coordinator, when its
//     lane's write teams meet the project's teams (coordinator), or a
//     session whose owning principal leads such a team in the team roles,
//     inside a lane that holds that team (team_lead_session). A binding
//     without lanes takes neither (outside_lane).
//
// Nothing is reserved until the operator has set serve
// --project-plan-proven; the preview works without it.

const projectPlanKind = "project_plan"

// projectPlanFields are the fields a plan may set.
var projectPlanFields = []string{"priority", "startDate", "targetDate"}

// projectPlanChangeable are the field names a read-back may report as
// changed besides the planned fields: names only, never a value.
var projectPlanChangeable = []string{"labels", "lead", "members", "name", "status", "state", "teams_added", "teams_removed", "targetDateResolution", "startDateResolution"}

// projectPlanAsk is the refusal for anyone who may preview but not
// confirm.
const projectPlanAsk = "only the project owner, a project lead, a team's lead in the team roles, or the coordinator inside its lane sets a Linear project's priority and dates; preview it and ask the project owner or a lead"

func projectPlanOperation(op string) bool {
	return op == "project_plan_reserve" || op == "project_plan_settle"
}

func (s *Store) projectPlanOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor map[string]any, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	if r.Session == "" && str(a, "instance") != "" {
		return nil, invalid("instance requires session")
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := at(nowRaw)
	if projectRole == "reader" {
		return nil, scoped("project member permission required", "forbidden")
	}
	op := str(a, "operation_id")
	if !commentOperationID.MatchString(op) {
		return nil, invalid("operation_id must be 1–128 letters, digits, '.', '_', ':' or '-'")
	}
	if r.Operation == "project_plan_reserve" {
		return s.reserveProjectPlan(ctx, tx, wid, pid, projectRole, actor, a, op, now)
	}
	return s.settleProjectPlan(ctx, tx, wid, pid, actor, a, op, now)
}

// projectPlanConfirmer is who confirms the plan, the lane it was checked
// against, and the team that authorized it ("" for the owner and a project
// lead). team_ids is the client's read of the project's teams; every role,
// pin and lead row is read here, at each call.
func (s *Store) projectPlanConfirmer(ctx context.Context, tx *transaction, pid, projectRole string, actor map[string]any, b TaskboardBindingV2, teams []string, confirm bool, a map[string]any) (by, lane, team string, err error) {
	// held are the project's teams that are write teams of the binding.
	held := []string{}
	for _, t := range teams {
		if b.TeamAccess(t) == TeamAccessWrite {
			held = append(held, t)
		}
	}
	roles, _, err := readTeamRoles(ctx, tx, pid)
	if err != nil {
		return "", "", "", err
	}
	if actor == nil {
		switch projectRole {
		case "owner":
			return "owner", "", "", nil
		case "lead":
			return "project_lead", "", "", nil
		}
		// A team's lead by the stored team roles: a lead row the owner
		// linked to this principal, of a write team the project holds.
		for _, t := range ledTeams(roles, s.principal, b.LinearTeamIDs) {
			if slices.Contains(held, t) {
				return "team_lead", "", t, nil
			}
		}
		return "", "", "", scoped(projectPlanAsk, "forbidden")
	}
	coordinator, err := isProjectCoordinator(ctx, tx, pid, actor)
	if err != nil {
		return "", "", "", err
	}
	// The session's owning principal, as stored; never a claim.
	owner := str(actor, "owner_principal")
	led := []string{}
	if actor["status"] == "active" && actor["role"] != "system" {
		for _, t := range ledTeams(roles, owner, b.LinearTeamIDs) {
			if slices.Contains(held, t) {
				led = append(led, t)
			}
		}
	}
	// While the dispatcher is on, a domain's coordinator session plans too,
	// for a project whose teams are all write teams of its lane (#287's
	// bound for a project status set).
	domain := false
	if !coordinator && len(led) == 0 {
		if domain, err = isLinearCoordinator(ctx, tx, pid, actor); err != nil {
			return "", "", "", err
		}
	}
	if !coordinator && len(led) == 0 && !domain {
		return "", "", "", scoped(projectPlanAsk, "forbidden")
	}
	if len(b.Lanes) == 0 {
		return "", "", "", scoped("outside_lane: this binding has no lanes, so a session sets no project plan; the project owner, a lead or a team's lead does, in their own terminal", "forbidden")
	}
	name, _, _, err := s.sessionLane(ctx, tx, pid, actor)
	if err != nil {
		return "", "", "", err
	}
	l, exists := b.Lane(name)
	if !exists {
		return "", "", "", scoped("outside_lane: the session has no lane, so it sets no project plan; the project owner pins it with linear lanes pin", "forbidden")
	}
	if coordinator {
		for _, t := range held {
			if slices.Contains(l.WriteTeamIDs, t) {
				return "coordinator", l.Name, t, nil
			}
		}
	}
	for _, t := range led {
		if slices.Contains(l.WriteTeamIDs, t) {
			return "team_lead_session", l.Name, t, nil
		}
	}
	if domain {
		return s.domainPlanBound(l, teams, confirm, a)
	}
	return "", "", "", scoped(fmt.Sprintf("outside_lane: the project holds no team of lane %s that this session may plan for; the project owner or a lead sets another lane's project", l.Name), "forbidden")
}

// domainPlanBound is a domain coordinator's bound: every team the project
// carries is a write team of its lane. A confirm judges the server's own
// Linear read of the project's teams (project_teams, which the server sets
// and a client can't), refused when partial or empty, and waits on the
// operator's proof that the app sees a project's private teams
// (project_status_domain, as #287's status set). A preview judges the
// client's read, so it shows who would confirm and records nothing.
func (s *Store) domainPlanBound(l Lane, clientTeams []string, confirm bool, a map[string]any) (by, lane, team string, err error) {
	teams := clientTeams
	if confirm {
		if !s.projectStatusDomainProven {
			return "", "", "", scoped("project_status_domain_unproven: a domain coordinator's project plan waits for the operator to prove that the app sees a project's private teams (serve --project-status-domain-proven); the project owner or a lead sets it", "forbidden")
		}
		if teams, err = uuidList(a, "project_teams"); err != nil {
			return "", "", "", err
		}
		if more, _ := a["project_teams_more"].(bool); more || len(teams) == 0 {
			return "", "", "", scoped(fmt.Sprintf("outside_lane: the project's teams weren't read in full by the server, so a coordinator of lane %s can't tell they are all its own; the project owner or a lead sets its plan", l.Name), "forbidden")
		}
	}
	if len(teams) == 0 {
		return "", "", "", scoped(fmt.Sprintf("outside_lane: no team of the project is known to be in lane %s", l.Name), "forbidden")
	}
	for _, t := range teams {
		if !slices.Contains(l.WriteTeamIDs, strings.ToLower(t)) {
			return "", "", "", scoped(fmt.Sprintf("outside_lane: the project carries team %s, which isn't a write team of lane %s; the project owner or a lead sets the plan of a cross-lane project", t, l.Name), "forbidden")
		}
	}
	return "coordinator", l.Name, teams[0], nil
}

// reserveProjectPlan authorizes a plan, and with confirm records it before
// any Linear call. Without confirm it is the preview's check: the same
// refusals, nothing recorded, and no gate. team_ids and fields are the
// client's own read and plan, so they are policy, not proof: the write
// goes through the confirming person's own Linear profile, whose access
// Linear checks.
func (s *Store) reserveProjectPlan(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor, a map[string]any, op string, now time.Time) (any, error) {
	confirm := false
	switch v := a["confirm"].(type) {
	case nil:
	case bool:
		confirm = v
	default:
		return nil, invalid("confirm must be true or false")
	}
	if confirm && !s.projectPlanProven {
		return nil, scoped("project_plan_unproven: this server sets no project plan until its operator sets serve --project-plan-proven after the probe; the preview works", "forbidden")
	}
	// The agent guard: without a session the confirm is the person's own;
	// a client that says an agent runs it is refused.
	switch v := a["agent_marker"].(type) {
	case nil:
	case string:
		if len(v) > 64 {
			return nil, invalid("agent_marker must be a short string")
		}
		if v != "" && actor == nil {
			return nil, scoped("setting a project plan without a session is a person's, and "+v+" says an agent runs this; an agent confirms from its session, or previews and asks the project owner or a lead", "forbidden")
		}
	default:
		return nil, invalid("agent_marker must be a short string")
	}
	project, err := lowerUUID(a, "linear_project", true)
	if err != nil {
		return nil, err
	}
	teams, err := uuidList(a, "team_ids")
	if err != nil {
		return nil, err
	}
	if len(teams) == 0 {
		return nil, invalid("team_ids must list the project's teams, as read")
	}
	fields, err := fieldNames(a["fields"], "fields", projectPlanFields)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, invalid("fields must name at least one of " + strings.Join(projectPlanFields, ", "))
	}
	plan := str(a, "plan_sha256")
	if !sha256Pattern.MatchString(plan) {
		return nil, invalid("plan_sha256 must be a lower-case SHA-256 hex digest")
	}
	b, linked, err := storedBinding(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if !linked {
		return nil, invalid("the task board isn't linked; a plan is set only on a project of the binding")
	}
	if b.Mode != "project_set" {
		return nil, invalid("a team board has no projects; link a project set to plan one of its projects")
	}
	if !slices.Contains(b.LinearProjectIDs, project) {
		return nil, scoped(fmt.Sprintf("project %s isn't in this binding", project), "forbidden")
	}
	by, lane, team, err := s.projectPlanConfirmer(ctx, tx, pid, projectRole, actor, b, teams, confirm, a)
	if err != nil {
		return nil, err
	}
	if !confirm {
		return map[string]any{"preview": true, "operation_id": op, "linear_project": project, "fields": fields, "by": by, "lane": lane, "team_id": team, "proven": s.projectPlanProven}, nil
	}
	session := ""
	if actor != nil {
		session = str(actor, "id")
	}
	// A session's principal is its owner's: the store refuses any other
	// principal's session.
	row, replay, err := reserveLinearWrite(ctx, tx, pid, linearWrite{Kind: projectPlanKind, Operation: op, Session: session, Principal: s.principal, ByRole: by, Lane: lane, Team: team, Subject: project, Plan: project + "\n" + strings.Join(fields, ",") + "\n" + strings.Join(teams, ",") + "\n" + plan, Detail: map[string]any{"fields": fields}}, now)
	if err != nil {
		return nil, err
	}
	if replay {
		return projectPlanView(row, true), nil
	}
	// History by IDs and field names only: never a value or a name.
	if err = s.audit(ctx, tx, wid, pid, session, projectPlanKind, op, "reserve", projectPlanAudit(row, by, lane, team, nil)); err != nil {
		return nil, err
	}
	return projectPlanView(row, false), nil
}

// projectPlanAudit is a reserve's or settle's history: the project, who,
// the session and its owning principal for a session, the lane, the
// field names and, for a settle, the changed names.
func projectPlanAudit(row map[string]any, by, lane, team string, changed []string) map[string]any {
	d := map[string]any{"linear_project": str(row, "subject"), "by": by, "lane": lane, "team_id": team, "fields": changedOf(detailOf(row)["fields"])}
	if session := str(row, "session_id"); session != "" {
		d["session_id"], d["principal"] = session, str(row, "principal")
	}
	if changed != nil {
		d["changed"] = changed
	}
	return d
}

// settleProjectPlan closes the caller's own reservation with the
// read-back's outcome: accepted (with the names of any other fields that
// changed), uncertain or not_sent. A settle repeats safely; a different
// outcome for a settled plan is a conflict.
func (s *Store) settleProjectPlan(ctx context.Context, tx *transaction, wid, pid string, actor, a map[string]any, op string, now time.Time) (any, error) {
	session := ""
	if actor != nil {
		session = str(actor, "id")
	}
	row, err := linearWriteOwn(ctx, tx, pid, op, projectPlanKind, session, s.principal)
	if err != nil {
		return nil, err
	}
	outcome := str(a, "outcome")
	if outcome != "accepted" && outcome != "uncertain" && outcome != "not_sent" {
		return nil, invalid("outcome must be accepted, uncertain or not_sent")
	}
	changed, err := fieldNames(a["changed"], "changed", projectPlanChangeable)
	if err != nil {
		return nil, err
	}
	if outcome != "accepted" && len(changed) > 0 {
		return nil, invalid("only an accepted plan names fields Linear changed")
	}
	detail := map[string]any{}
	if len(changed) > 0 {
		detail["changed"] = changed
	}
	if str(row, "state") != "reserved" && !slices.Equal(changedOf(detailOf(row)["changed"]), changed) {
		return nil, scoped("this plan was already settled as "+str(row, "state"), "conflict")
	}
	row, replay, err := settleLinearWrite(ctx, tx, pid, row, outcome, "", detail, now)
	if err != nil {
		return nil, err
	}
	if replay {
		return projectPlanView(row, true), nil
	}
	if err = s.audit(ctx, tx, wid, pid, session, projectPlanKind, op, outcome, projectPlanAudit(row, str(row, "by_role"), str(row, "lane"), str(row, "team_id"), changed)); err != nil {
		return nil, err
	}
	return projectPlanView(row, false), nil
}

// projectPlanState is a row's outcome as shown: accepted_with_changes when
// Linear changed more than the planned fields.
func projectPlanState(row map[string]any) string {
	state := str(row, "state")
	if state == "accepted" && len(changedOf(detailOf(row)["changed"])) > 0 {
		return "accepted_with_changes"
	}
	return state
}

func projectPlanView(row map[string]any, replay bool) map[string]any {
	return map[string]any{"operation_id": row["operation_id"], "linear_project": row["subject"], "fields": changedOf(detailOf(row)["fields"]), "team_id": row["team_id"], "by": row["by_role"], "lane": row["lane"], "state": projectPlanState(row), "changed": changedOf(detailOf(row)["changed"]), "acknowledged_at": row["acknowledged_at"], "reserved_at": row["reserved_at"], "settled_at": row["settled_at"], "replay": replay}
}

// projectPlanShown bounds the plans the overview lists.
const projectPlanShown = projectTeamAddShown

// projectPlansView is the settled plans, newest first, and how many came
// back with other fields changed: doctor and the overview count them.
func projectPlansView(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT operation_id,subject,team_id,by_role,lane,state,detail,reserved_at,settled_at,acknowledged_at FROM linear_write_reservations WHERE project_id=? AND kind=? AND state<>'reserved' ORDER BY settled_at DESC,operation_id`, pid, projectPlanKind)
	if err != nil {
		return nil, err
	}
	planned, withChanges := 0, 0
	recent := []map[string]any{}
	for _, row := range rows {
		switch projectPlanState(row) {
		case "accepted":
			planned++
		case "accepted_with_changes":
			planned++
			// Counted until the person who may acknowledge it does
			// (linear writes --ack).
			if row["acknowledged_at"] == nil {
				withChanges++
			}
		}
		if len(recent) < projectPlanShown {
			recent = append(recent, projectPlanView(row, false))
		}
	}
	return map[string]any{"planned": planned, "with_changes": withChanges, "recent": recent, "keep_days": linearWriteKeepDays}, nil
}

// SetProjectPlanProven is serve --project-plan-proven.
func (s *Store) SetProjectPlanProven(proven bool) { s.projectPlanProven = proven }
