package bus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A bound team added to a Linear project in the binding (FR10,
// live/FR10-PLAN.md). The client plans and previews it; a confirm is
// reserved here before anything reaches Linear (a linear_write_reservations
// row of kind project_team_add: team_id the team added, subject the Linear
// project) and settled after the read-back. The server decides who may
// confirm: the project owner or a lead without a session, or the project's
// designated coordinator session when the team is a write team of its lane
// and the project already holds one. Nothing is reserved until the operator
// has set serve --project-team-add-proven.

const projectTeamAddKind = "project_team_add"

// projectTeamAddShown bounds the adds the list shows.
const projectTeamAddShown = 20

// projectTeamAddFields are the field names a read-back may report as
// changed besides the team added: names only, never a value.
var projectTeamAddFields = []string{"labels", "lead", "members", "name", "priority", "startDate", "state", "status", "targetDate", "teams_added", "teams_removed"}

// projectTeamAddUnverifiable are the fields a read may leave out, so an add
// couldn't prove them unchanged (F2).
var projectTeamAddUnverifiable = []string{"lead", "members"}

func projectTeamAddOperation(op string) bool {
	return op == "project_team_add_reserve" || op == "project_team_add_settle" || op == "project_team_add_list"
}

func (s *Store) projectTeamAddOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor map[string]any, r Request) (any, error) {
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
	if r.Operation == "project_team_add_list" {
		return projectTeamAddsView(ctx, tx, pid)
	}
	if projectRole == "reader" {
		return nil, scoped("project member permission required", "forbidden")
	}
	op := str(a, "operation_id")
	if !commentOperationID.MatchString(op) {
		return nil, invalid("operation_id must be 1–128 letters, digits, '.', '_', ':' or '-'")
	}
	if r.Operation == "project_team_add_reserve" {
		return s.reserveProjectTeamAdd(ctx, tx, wid, pid, projectRole, actor, a, op, now)
	}
	return s.settleProjectTeamAdd(ctx, tx, wid, pid, actor, a, op, now)
}

// projectTeamAdder is who confirms the add, and the lane it was checked
// against: the owner or a lead in their own terminal, or the designated
// coordinator when team is a write team of its lane and the project's
// current teams include one too (R2: a coordinator never pulls another
// lane's project into its own).
func (s *Store) projectTeamAdder(ctx context.Context, tx *transaction, pid, projectRole string, actor map[string]any, b TaskboardBindingV2, team string, current []string) (string, string, error) {
	if actor == nil {
		if projectRole == "owner" || projectRole == "lead" {
			return projectRole, "", nil
		}
		// R3: the added team's own lead, by the stored team roles: a lead
		// row of that team the owner linked to this principal. The person
		// being active and not a guest is Linear's to say; the client checks
		// it before reserving, and Linear refuses a deactivated account.
		roles, _, err := readTeamRoles(ctx, tx, pid)
		if err != nil {
			return "", "", err
		}
		if slices.Contains(ledTeams(roles, s.principal, b.LinearTeamIDs), team) {
			return "team_lead", "", nil
		}
		return "", "", scoped("only the project owner, a lead, or the team's own lead in the team roles adds a team to a Linear project, from their own terminal; preview it and ask them", "forbidden")
	}
	ok, err := isProjectCoordinator(ctx, tx, pid, actor)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", scoped("only the project's designated coordinator session adds a team to a Linear project; other sessions preview it and ask the project owner or a lead", "forbidden")
	}
	if len(b.Lanes) == 0 {
		return "", "", scoped("outside_lane: this binding has no lanes, so a coordinator adds no team to a project; the project owner or a lead does", "forbidden")
	}
	lane, _, _, err := s.sessionLane(ctx, tx, pid, actor)
	if err != nil {
		return "", "", err
	}
	l, exists := b.Lane(lane)
	if !exists {
		return "", "", scoped("outside_lane: the coordinator session has no lane, so it adds no team to a project; the project owner pins it with linear lanes pin", "forbidden")
	}
	if !slices.Contains(l.WriteTeamIDs, team) {
		return "", "", scoped(fmt.Sprintf("outside_lane: team %s isn't a write team of lane %s; a coordinator adds only its lane's teams", team, l.Name), "forbidden")
	}
	if !slices.ContainsFunc(current, func(t string) bool { return slices.Contains(l.WriteTeamIDs, t) }) {
		return "", "", scoped(fmt.Sprintf("outside_lane: the project holds no write team of lane %s; the project owner or a lead adds a team to another lane's project", l.Name), "forbidden")
	}
	return "coordinator", l.Name, nil
}

// reserveProjectTeamAdd records a confirmed add before any Linear call.
// project_teams is the client's own read of the project's teams, so it is
// policy, not proof: the server decides who may add which team to which
// project of the binding, and the write itself goes through the confirming
// person's own Linear profile, whose access Linear checks.
func (s *Store) reserveProjectTeamAdd(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor, a map[string]any, op string, now time.Time) (any, error) {
	if !s.projectTeamAddProven {
		return nil, scoped("project_team_add_unproven: this server adds no team to a project until its operator sets serve --project-team-add-proven after the probe; the preview works", "forbidden")
	}
	project, err := lowerUUID(a, "linear_project", true)
	if err != nil {
		return nil, err
	}
	team, err := lowerUUID(a, "team_id", true)
	if err != nil {
		return nil, err
	}
	current, err := uuidList(a, "project_teams")
	if err != nil {
		return nil, err
	}
	plan := str(a, "plan_sha256")
	if !sha256Pattern.MatchString(plan) {
		return nil, invalid("plan_sha256 must be a lower-case SHA-256 hex digest")
	}
	if slices.Contains(current, team) {
		return nil, invalid("the team is already in the project; nothing to add")
	}
	b, linked, err := storedBinding(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if !linked {
		return nil, invalid("the task board isn't linked; a team is added only to a project of the binding")
	}
	if b.Mode != "project_set" {
		return nil, invalid("a team board has no projects; link a project set to add a team to one of its projects")
	}
	if !slices.Contains(b.LinearProjectIDs, project) {
		return nil, scoped(fmt.Sprintf("project %s isn't in this binding", project), "forbidden")
	}
	if b.TeamAccess(team) != TeamAccessWrite {
		return nil, scoped(fmt.Sprintf("team %s isn't a write team of this binding", team), "forbidden")
	}
	by, lane, err := s.projectTeamAdder(ctx, tx, pid, projectRole, actor, b, team, current)
	if err != nil {
		return nil, err
	}
	session := ""
	if actor != nil {
		session = str(actor, "id")
	}
	row, replay, err := reserveLinearWrite(ctx, tx, pid, linearWrite{Kind: projectTeamAddKind, Operation: op, Session: session, Principal: s.principal, ByRole: by, Lane: lane, Team: team, Subject: project, Plan: project + "\n" + team + "\n" + strings.Join(current, ",") + "\n" + plan}, now)
	if err != nil {
		return nil, err
	}
	if replay {
		return projectTeamAddView(row, true), nil
	}
	// History by IDs only: no names.
	if err = s.audit(ctx, tx, wid, pid, session, projectTeamAddKind, op, "reserve", map[string]any{"linear_project": project, "team_id": team, "by": by, "lane": lane}); err != nil {
		return nil, err
	}
	return projectTeamAddView(row, false), nil
}

// settleProjectTeamAdd closes the caller's own reservation with the
// read-back's outcome: accepted (with the names of any other fields that
// changed), uncertain or not_sent. A settle repeats safely; a different
// outcome for a settled add is a conflict.
func (s *Store) settleProjectTeamAdd(ctx context.Context, tx *transaction, wid, pid string, actor, a map[string]any, op string, now time.Time) (any, error) {
	session := ""
	if actor != nil {
		session = str(actor, "id")
	}
	row, err := linearWriteOwn(ctx, tx, pid, op, projectTeamAddKind, session, s.principal)
	if err != nil {
		return nil, err
	}
	outcome := str(a, "outcome")
	if outcome != "accepted" && outcome != "uncertain" && outcome != "not_sent" {
		return nil, invalid("outcome must be accepted, uncertain or not_sent")
	}
	changed, err := fieldNames(a["changed"], "changed", projectTeamAddFields)
	if err != nil {
		return nil, err
	}
	unverified, err := fieldNames(a["unverified"], "unverified", projectTeamAddUnverifiable)
	if err != nil {
		return nil, err
	}
	if outcome != "accepted" && len(changed)+len(unverified) > 0 {
		return nil, invalid("only an accepted add names fields Linear changed or that couldn't be read")
	}
	detail := map[string]any{}
	if len(changed) > 0 {
		detail["changed"] = changed
	}
	if len(unverified) > 0 {
		detail["unverified"] = unverified
	}
	if str(row, "state") != "reserved" && (!slices.Equal(changedOf(detailOf(row)["changed"]), changed) || !slices.Equal(changedOf(detailOf(row)["unverified"]), unverified)) {
		return nil, scoped("this add was already settled as "+str(row, "state"), "conflict")
	}
	row, replay, err := settleLinearWrite(ctx, tx, pid, row, outcome, "", detail, now)
	if err != nil {
		return nil, err
	}
	if replay {
		return projectTeamAddView(row, true), nil
	}
	if err = s.audit(ctx, tx, wid, pid, session, projectTeamAddKind, op, outcome, map[string]any{"linear_project": str(row, "subject"), "team_id": str(row, "team_id"), "by": str(row, "by_role"), "lane": str(row, "lane"), "changed": changed, "unverified": unverified}); err != nil {
		return nil, err
	}
	return projectTeamAddView(row, false), nil
}

// fieldNames is a settle's list of field names under key, sorted and
// checked against allowed.
func fieldNames(v any, key string, allowed []string) ([]string, error) {
	names := []string{}
	if v == nil {
		return names, nil
	}
	list, ok := v.([]any)
	if !ok {
		if s, ok := v.([]string); ok {
			for _, x := range s {
				list = append(list, x)
			}
		} else {
			return nil, invalid(key + " must be a list of field names")
		}
	}
	for _, x := range list {
		name, _ := x.(string)
		if !slices.Contains(allowed, name) {
			return nil, invalid(key + " names only fields of " + strings.Join(allowed, ", "))
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// uuidList reads a list of UUIDs, lower-cased.
func uuidList(a map[string]any, key string) ([]string, error) {
	var raw []any
	switch v := a[key].(type) {
	case nil:
	case []any:
		raw = v
	case []string:
		for _, x := range v {
			raw = append(raw, x)
		}
	default:
		return nil, invalid(key + " must be a list of UUIDs")
	}
	out := []string{}
	for _, x := range raw {
		id, err := lowerUUID(map[string]any{"id": x}, "id", true)
		if err != nil {
			return nil, invalid(key + " must be a list of UUIDs")
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

func changedOf(v any) []string {
	out := []string{}
	list, _ := v.([]any)
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// projectTeamAddState is a row's outcome as shown: accepted_with_changes
// when Linear changed more than the teams.
func projectTeamAddState(row map[string]any) string {
	state := str(row, "state")
	if state == "accepted" && len(changedOf(detailOf(row)["changed"])) > 0 {
		return "accepted_with_changes"
	}
	return state
}

func projectTeamAddView(row map[string]any, replay bool) map[string]any {
	return map[string]any{"operation_id": row["operation_id"], "linear_project": row["subject"], "team_id": row["team_id"], "by": row["by_role"], "lane": row["lane"], "state": projectTeamAddState(row), "changed": changedOf(detailOf(row)["changed"]), "unverified": changedOf(detailOf(row)["unverified"]), "acknowledged_at": row["acknowledged_at"], "reserved_at": row["reserved_at"], "settled_at": row["settled_at"], "replay": replay}
}

// projectTeamAddsView is the settled adds, newest first, and how many
// came back with other fields changed: doctor and the overview count them.
func projectTeamAddsView(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT operation_id,subject,team_id,by_role,lane,state,detail,reserved_at,settled_at,acknowledged_at FROM linear_write_reservations WHERE project_id=? AND kind=? AND state<>'reserved' ORDER BY settled_at DESC,operation_id`, pid, projectTeamAddKind)
	if err != nil {
		return nil, err
	}
	added, withChanges := 0, 0
	recent := []map[string]any{}
	for _, row := range rows {
		state := projectTeamAddState(row)
		switch state {
		case "accepted":
			added++
		case "accepted_with_changes":
			added++
			// Counted until the owner acknowledges it (linear writes --ack).
			if row["acknowledged_at"] == nil {
				withChanges++
			}
		}
		if len(recent) < projectTeamAddShown {
			recent = append(recent, projectTeamAddView(row, false))
		}
	}
	return map[string]any{"added": added, "with_changes": withChanges, "recent": recent, "keep_days": linearWriteKeepDays}, nil
}

// SetProjectTeamAddProven is serve --project-team-add-proven.
func (s *Store) SetProjectTeamAddProven(proven bool) { s.projectTeamAddProven = proven }
