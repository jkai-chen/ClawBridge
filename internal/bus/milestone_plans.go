package bus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A Linear project's milestones, created and updated through ClawBridge
// (FR55 PR 2, live/FR55-PLAN.md §2 and its r1). It is FR13's machinery with
// another subject: the client reads the project's milestones, previews
// current → planned, and asks the server who may confirm
// (milestone_plan_reserve without confirm checks everything and records
// nothing). A confirm is reserved here before anything reaches Linear (a
// linear_write_reservations row of kind milestone_plan) and settled after
// the read-back. Neither a name nor a date is stored; plan_sha256 binds
// them. Who may confirm is FR13's rule, read from stored rows at every call
// (planConfirmer): the project owner, a project lead, a team's lead in the
// team roles, the coordinator inside its lane, a session a team's lead owns
// inside a lane holding the team; while the dispatcher is on, a domain's
// coordinator session too, for a project whose teams are all write teams of
// its lane, as the server's own read shows them. Nothing is reserved until the operator has set serve
// --milestone-plan-proven.
//
// The row's subject is "<project>:<what>": for an update the milestone's
// UUID; for a create the SHA-256 of the case-folded, NFC name (N1), so two
// creates of one name serialise on it and no name is ever stored. A create
// that is reserved or uncertain, and not acknowledged, blocks another
// create of that name; the settled result is the new milestone's ID.

const milestonePlanKind = "milestone_plan"

// milestonePlanFields are the fields a milestone plan may set.
var milestonePlanFields = []string{"name", "targetDate"}

// milestonePlanChangeable are the names a read-back may report as changed
// besides the planned fields: names only, never a value.
var milestonePlanChangeable = []string{"name", "sortOrder", "targetDate"}

// milestonePlanAsk is the refusal for anyone who may preview but not
// confirm.
const milestonePlanAsk = "only the project owner, a project lead, a team's lead in the team roles, or the coordinator inside its lane creates or updates a Linear project's milestones; preview it and ask the project owner or a lead"

var milestonePlanWords = planWords{noun: "milestone plan", ask: milestonePlanAsk, domain: true}

func milestonePlanOperation(op string) bool {
	return op == "milestone_plan_reserve" || op == "milestone_plan_settle"
}

func (s *Store) milestonePlanOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor map[string]any, r Request) (any, error) {
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
	if r.Operation == "milestone_plan_reserve" {
		return s.reserveMilestonePlan(ctx, tx, wid, pid, projectRole, actor, a, op, now)
	}
	return s.settleMilestonePlan(ctx, tx, wid, pid, actor, a, op, now)
}

// milestoneSubject is a milestone plan row's subject, and its parts.
func milestoneSubject(project, what string) string { return project + ":" + what }

func splitMilestoneSubject(subject string) (project, what string) {
	project, what, _ = strings.Cut(subject, ":")
	return project, what
}

// reserveMilestonePlan authorizes a plan, and with confirm records it before
// any Linear call. Without confirm it is the preview's check: the same
// refusals, nothing recorded, and no gate. team_ids and fields are the
// client's own read and plan, so they are policy, not proof: the write goes
// through the confirming person's own Linear profile, whose access Linear
// checks.
func (s *Store) reserveMilestonePlan(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor, a map[string]any, op string, now time.Time) (any, error) {
	confirm := false
	switch v := a["confirm"].(type) {
	case nil:
	case bool:
		confirm = v
	default:
		return nil, invalid("confirm must be true or false")
	}
	if confirm && !s.milestonePlanProven {
		return nil, scoped("milestone_plan_unproven: this server creates and updates no milestone until its operator sets serve --milestone-plan-proven after the live sitting; the preview works", "forbidden")
	}
	// The agent guard, as FR13's: without a session the confirm is a
	// person's own.
	switch v := a["agent_marker"].(type) {
	case nil:
	case string:
		if len(v) > 64 {
			return nil, invalid("agent_marker must be a short string")
		}
		if v != "" && actor == nil {
			return nil, scoped("changing a project's milestones without a session is a person's, and "+v+" says an agent runs this; an agent confirms from its session, or previews and asks the project owner or a lead", "forbidden")
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
	kind := str(a, "op")
	if kind != "create" && kind != "update" {
		return nil, invalid("op must be create or update")
	}
	milestone, err := lowerUUID(a, "milestone", kind == "update")
	if err != nil {
		return nil, err
	}
	if kind == "create" && milestone != "" {
		return nil, invalid("a create has no milestone yet")
	}
	fields, err := fieldNames(a["fields"], "fields", milestonePlanFields)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, invalid("fields must name at least one of " + strings.Join(milestonePlanFields, ", "))
	}
	what := milestone
	if kind == "create" {
		if !slices.Contains(fields, "name") {
			return nil, invalid("a create sets the name")
		}
		what = str(a, "name_sha256")
		if !sha256Pattern.MatchString(what) {
			return nil, invalid("name_sha256 must be a lower-case SHA-256 hex digest of the folded name")
		}
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
		return nil, invalid("the task board isn't linked; a milestone is changed only on a project of the binding")
	}
	if b.Mode != "project_set" {
		return nil, invalid("a team board has no projects; link a project set to change one of its milestones")
	}
	if !slices.Contains(b.LinearProjectIDs, project) {
		return nil, scoped(fmt.Sprintf("project %s isn't in this binding", project), "forbidden")
	}
	by, lane, team, err := s.planConfirmer(ctx, tx, pid, projectRole, actor, b, teams, confirm, a, milestonePlanWords)
	if err != nil {
		return nil, err
	}
	if !confirm {
		return map[string]any{"preview": true, "operation_id": op, "linear_project": project, "op": kind, "fields": fields, "by": by, "lane": lane, "team_id": team, "proven": s.milestonePlanProven}, nil
	}
	session := ""
	if actor != nil {
		session = str(actor, "id")
	}
	subject := milestoneSubject(project, what)
	if kind == "create" {
		// N1: one create of a name at a time. Another create of it that is
		// reserved or uncertain, and not acknowledged, may have made the
		// milestone; this one waits for the owner to look.
		rows, err := query(ctx, tx, `SELECT operation_id FROM linear_write_reservations WHERE project_id=? AND kind=? AND subject=? AND operation_id<>? AND acknowledged_at IS NULL AND state IN ('reserved','uncertain')`, pid, milestonePlanKind, subject, op)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			return nil, scoped("another create of a milestone with this name is unsettled or uncertain; look in Linear, then the project owner acknowledges it with linear writes --ack", "conflict")
		}
	}
	// A session's principal is its owner's: the store refuses any other
	// principal's session.
	row, replay, err := reserveLinearWrite(ctx, tx, pid, linearWrite{Kind: milestonePlanKind, Operation: op, Session: session, Principal: s.principal, ByRole: by, Lane: lane, Team: team, Subject: subject, Plan: subject + "\n" + kind + "\n" + strings.Join(fields, ",") + "\n" + strings.Join(teams, ",") + "\n" + plan, Detail: map[string]any{"op": kind, "fields": fields}}, now)
	if err != nil {
		return nil, err
	}
	if replay {
		return milestonePlanView(row, true), nil
	}
	// History by IDs and field names only: never a value or a name.
	if err = s.audit(ctx, tx, wid, pid, session, milestonePlanKind, op, "reserve", milestonePlanAudit(row, by, lane, team, nil)); err != nil {
		return nil, err
	}
	return milestonePlanView(row, false), nil
}

// milestonePlanAudit is a reserve's or settle's history: the project, the
// milestone (an update's, or a settled create's result), who, the session
// and its owning principal for a session, the lane, the operation, the
// field names and, for a settle, the changed names.
func milestonePlanAudit(row map[string]any, by, lane, team string, changed []string) map[string]any {
	project, _ := splitMilestoneSubject(str(row, "subject"))
	d := map[string]any{"linear_project": project, "op": str(detailOf(row), "op"), "by": by, "lane": lane, "team_id": team, "fields": changedOf(detailOf(row)["fields"])}
	if m := milestoneOf(row); m != "" {
		d["milestone"] = m
	}
	if session := str(row, "session_id"); session != "" {
		d["session_id"], d["principal"] = session, str(row, "principal")
	}
	if changed != nil {
		d["changed"] = changed
	}
	return d
}

// milestoneOf is the milestone a row is about: an update's, or the ID a
// settled create ended with ("" before).
func milestoneOf(row map[string]any) string {
	_, what := splitMilestoneSubject(str(row, "subject"))
	if str(detailOf(row), "op") == "create" {
		return str(row, "result")
	}
	return what
}

// settleMilestonePlan closes the caller's own reservation with the
// read-back's outcome: accepted (with the names of any other fields that
// changed, and a create's milestone ID), uncertain or not_sent. A settle
// repeats safely; a different outcome for a settled plan is a conflict.
func (s *Store) settleMilestonePlan(ctx context.Context, tx *transaction, wid, pid string, actor, a map[string]any, op string, now time.Time) (any, error) {
	session := ""
	if actor != nil {
		session = str(actor, "id")
	}
	row, err := linearWriteOwn(ctx, tx, pid, op, milestonePlanKind, session, s.principal)
	if err != nil {
		return nil, err
	}
	outcome := str(a, "outcome")
	if outcome != "accepted" && outcome != "uncertain" && outcome != "not_sent" {
		return nil, invalid("outcome must be accepted, uncertain or not_sent")
	}
	planned := changedOf(detailOf(row)["fields"])
	changed, err := fieldNames(a["changed"], "changed", milestonePlanChangeable)
	if err != nil {
		return nil, err
	}
	for _, name := range changed {
		if slices.Contains(planned, name) {
			return nil, invalid("changed names only fields the plan didn't send")
		}
	}
	if outcome != "accepted" && len(changed) > 0 {
		return nil, invalid("only an accepted plan names fields Linear changed")
	}
	_, what := splitMilestoneSubject(str(row, "subject"))
	created := ""
	if m, ok := a["milestone"]; ok && m != nil && m != "" {
		if created, err = lowerUUID(a, "milestone", true); err != nil {
			return nil, err
		}
		switch {
		case outcome != "accepted":
			return nil, invalid("only an accepted plan names its milestone")
		case str(detailOf(row), "op") == "update" && created != what:
			return nil, invalid("milestone must be the one the plan updates")
		}
	}
	// An update's milestone is its subject; only a create keeps it as the
	// result.
	result := ""
	if str(detailOf(row), "op") == "create" {
		result = created
	}
	detail := map[string]any{}
	if len(changed) > 0 {
		detail["changed"] = changed
	}
	if str(row, "state") != "reserved" && !slices.Equal(changedOf(detailOf(row)["changed"]), changed) {
		return nil, scoped("this plan was already settled as "+str(row, "state"), "conflict")
	}
	row, replay, err := settleLinearWrite(ctx, tx, pid, row, outcome, result, detail, now)
	if err != nil {
		return nil, err
	}
	if replay {
		return milestonePlanView(row, true), nil
	}
	if err = s.audit(ctx, tx, wid, pid, session, milestonePlanKind, op, outcome, milestonePlanAudit(row, str(row, "by_role"), str(row, "lane"), str(row, "team_id"), changed)); err != nil {
		return nil, err
	}
	return milestonePlanView(row, false), nil
}

// milestonePlanState is a row's outcome as shown: accepted_with_changes
// when Linear changed more than the planned fields.
func milestonePlanState(row map[string]any) string {
	state := str(row, "state")
	if state == "accepted" && len(changedOf(detailOf(row)["changed"])) > 0 {
		return "accepted_with_changes"
	}
	return state
}

func milestonePlanView(row map[string]any, replay bool) map[string]any {
	project, _ := splitMilestoneSubject(str(row, "subject"))
	return map[string]any{"operation_id": row["operation_id"], "linear_project": project, "op": str(detailOf(row), "op"), "milestone": milestoneOf(row), "fields": changedOf(detailOf(row)["fields"]), "team_id": row["team_id"], "by": row["by_role"], "lane": row["lane"], "state": milestonePlanState(row), "changed": changedOf(detailOf(row)["changed"]), "acknowledged_at": row["acknowledged_at"], "reserved_at": row["reserved_at"], "settled_at": row["settled_at"], "replay": replay}
}

// milestonePlansView is the settled milestone plans, newest first, and how
// many came back with other fields changed: doctor and the overview count
// them.
func milestonePlansView(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT operation_id,subject,team_id,by_role,lane,state,result,detail,reserved_at,settled_at,acknowledged_at FROM linear_write_reservations WHERE project_id=? AND kind=? AND state<>'reserved' ORDER BY settled_at DESC,operation_id`, pid, milestonePlanKind)
	if err != nil {
		return nil, err
	}
	planned, withChanges := 0, 0
	recent := []map[string]any{}
	for _, row := range rows {
		switch milestonePlanState(row) {
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
			recent = append(recent, milestonePlanView(row, false))
		}
	}
	return map[string]any{"planned": planned, "with_changes": withChanges, "recent": recent, "keep_days": linearWriteKeepDays}, nil
}

// SetMilestonePlanProven is serve --milestone-plan-proven.
func (s *Store) SetMilestonePlanProven(proven bool) { s.milestonePlanProven = proven }
