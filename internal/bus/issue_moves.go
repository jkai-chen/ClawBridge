package bus

import (
	"context"
	"fmt"
	"slices"
)

// An issue's move to another bound team (FR7, live/ISSUE-TEAM-MOVE-DESIGN.md
// r5). The client plans and previews it; a confirm is reserved here before
// anything reaches Linear, and settled after the read-back, as a Linear
// write of kind issue_move (linear_writes.go). The server decides who may
// confirm: the project owner or a project lead without a session, or the project's
// designated coordinator session when both teams are write teams of its own
// lane. Nothing is reserved until the operator has set
// serve --issue-move-proven.

// issueMoveKind is a move's kind in the Linear write reservations.
const issueMoveKind = "issue_move"

// issueIdentifierStores are the tables that keep an issue's display
// identifier beside its UUID; an accepted move rewrites each (M8). Every
// other store is keyed by UUID. TestIssueMoveUpdatesEveryIdentifierStore
// pins this list against the schema.
var issueIdentifierStores = []string{"session_assignments", "intake_queue"}

func issueMoveOperation(op string) bool {
	return op == "issue_move_reserve" || op == "issue_move_settle"
}

func (s *Store) issueMoveOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor map[string]any, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	if r.Session == "" && str(a, "instance") != "" {
		return nil, invalid("instance requires session")
	}
	op := str(a, "operation_id")
	if !commentOperationID.MatchString(op) {
		return nil, invalid("operation_id must be 1–128 letters, digits, '.', '_', ':' or '-'")
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err = pruneLinearWrites(ctx, tx, pid, at(nowRaw)); err != nil {
		return nil, err
	}
	if r.Operation == "issue_move_reserve" {
		return s.reserveIssueMove(ctx, tx, wid, pid, projectRole, actor, a, op, nowRaw)
	}
	return s.settleIssueMove(ctx, tx, wid, pid, actor, a, op, nowRaw)
}

// issueMoveMover is who confirms the move, and the lane it was checked
// against: the owner or a project lead in their own terminal, or the designated
// coordinator inside its lane. Every other caller is refused.
func (s *Store) issueMoveMover(ctx context.Context, tx *transaction, pid, projectRole string, actor map[string]any, b TaskboardBindingV2, from, to string) (string, string, error) {
	if actor == nil {
		if projectRole != "owner" && projectRole != "lead" {
			return "", "", scoped("only the project owner or a project lead confirms an issue's team move, from their own terminal; preview it and ask them", "forbidden")
		}
		return projectRole, "", nil
	}
	ok, err := isProjectCoordinator(ctx, tx, pid, actor)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", scoped("only the project's designated coordinator session confirms an issue's team move; other sessions preview it and ask the project owner or a project lead", "forbidden")
	}
	if len(b.Lanes) == 0 {
		return "", "", scoped("outside_lane: this binding has no lanes, so a coordinator confirms no team move; the project owner or a project lead confirms it", "forbidden")
	}
	lane, _, _, err := s.sessionLane(ctx, tx, pid, actor)
	if err != nil {
		return "", "", err
	}
	l, exists := b.Lane(lane)
	if !exists {
		return "", "", scoped("outside_lane: the coordinator session has no lane, so it confirms no team move; the project owner pins it with linear lanes pin", "forbidden")
	}
	for _, team := range []string{from, to} {
		if !slices.Contains(l.WriteTeamIDs, team) {
			return "", "", scoped(fmt.Sprintf("outside_lane: team %s isn't a write team of lane %s; a coordinator moves issues only between its lane's teams", team, l.Name), "forbidden")
		}
	}
	return "coordinator", l.Name, nil
}

// reserveIssueMove records a confirmed move before any Linear call. A
// repeat with the same operation_id and plan returns the same reservation;
// one that never reached Linear reopens. from_team is the client's own read
// of the issue's team, so it is policy, not proof: the server decides who
// may move between these two teams, and the write itself goes through the
// confirming person's own Linear profile, whose access Linear checks. No
// agent-facing tool takes a source team (pinned in the MCP tests).
func (s *Store) reserveIssueMove(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor, a map[string]any, op, nowRaw string) (any, error) {
	if !s.issueMoveProven {
		return nil, scoped("issue_move_unproven: this server confirms no team move until its operator sets serve --issue-move-proven after the move probe; the preview works", "forbidden")
	}
	issue, err := lowerUUID(a, "issue_uuid", true)
	if err != nil {
		return nil, err
	}
	from, err := lowerUUID(a, "from_team", true)
	if err != nil {
		return nil, err
	}
	to, err := lowerUUID(a, "to_team", true)
	if err != nil {
		return nil, err
	}
	plan := str(a, "plan_sha256")
	if !sha256Pattern.MatchString(plan) {
		return nil, invalid("plan_sha256 must be a lower-case SHA-256 hex digest")
	}
	if from == to {
		return nil, invalid("the issue is already in that team")
	}
	b, linked, err := storedBinding(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if !linked {
		return nil, invalid("the task board isn't linked; an issue moves only between bound teams")
	}
	for _, team := range []string{from, to} {
		if !slices.Contains(b.LinearTeamIDs, team) || b.TeamAccess(team) != TeamAccessWrite {
			return nil, scoped(fmt.Sprintf("team %s isn't a write team of this binding; an issue moves only between bound write teams", team), "forbidden")
		}
	}
	by, lane, err := s.issueMoveMover(ctx, tx, pid, projectRole, actor, b, from, to)
	if err != nil {
		return nil, err
	}
	session := ""
	if actor != nil {
		session = str(actor, "id")
	}
	now := at(nowRaw)
	// The plan names the move by IDs; the client's digest covers the rest.
	row, replay, err := reserveLinearWrite(ctx, tx, pid, linearWrite{Kind: issueMoveKind, Operation: op, Session: session, Principal: s.principal, ByRole: by, Lane: lane, Team: to, Subject: issue, Plan: issue + "\n" + from + "\n" + to + "\n" + plan, Detail: map[string]any{"from_team": from}}, now)
	if err != nil {
		return nil, err
	}
	if !replay {
		// History by IDs only: no title, no names.
		if err = s.audit(ctx, tx, wid, pid, session, "issue_move", op, "reserve", map[string]any{"issue_uuid": issue, "from_team": from, "to_team": to, "by": by, "lane": lane}); err != nil {
			return nil, err
		}
	}
	return issueMoveView(row, replay), nil
}

// settleIssueMove closes the caller's own reservation with the read-back's
// outcome. An accepted move rewrites the stored identifier everywhere it is
// kept, so sessions --issue NEW-ID finds the holder. A settle repeats
// safely; a different outcome for a settled move is a conflict.
func (s *Store) settleIssueMove(ctx context.Context, tx *transaction, wid, pid string, actor, a map[string]any, op, nowRaw string) (any, error) {
	session := ""
	if actor != nil {
		session = str(actor, "id")
	}
	row, err := linearWriteOwn(ctx, tx, pid, op, issueMoveKind, session, s.principal)
	if err != nil {
		return nil, err
	}
	outcome := str(a, "outcome")
	if outcome != "accepted" && outcome != "uncertain" && outcome != "not_sent" {
		return nil, invalid("outcome must be accepted, uncertain or not_sent")
	}
	oldID, newID := str(a, "old_identifier"), str(a, "new_identifier")
	for _, v := range []string{oldID, newID} {
		if v != "" && !issueIdentifierPattern.MatchString(v) {
			return nil, invalid("identifiers must be Linear display identifiers")
		}
	}
	if outcome == "accepted" && newID == "" {
		return nil, invalid("an accepted move names the issue's new identifier")
	}
	var detail map[string]any
	if oldID != "" {
		detail = map[string]any{"old_identifier": oldID}
	}
	row, replay, err := settleLinearWrite(ctx, tx, pid, row, outcome, newID, detail, at(nowRaw))
	if err != nil {
		return nil, err
	}
	if replay {
		return issueMoveView(row, true), nil
	}
	issue, from := str(row, "subject"), str(detailOf(row), "from_team")
	updated := 0
	if outcome == "accepted" {
		for _, table := range issueIdentifierStores {
			res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET issue_identifier=? WHERE project_id=? AND issue_uuid=?`, newID, pid, issue)
			if err != nil {
				return nil, err
			}
			n, _ := res.RowsAffected()
			updated += int(n)
		}
	}
	if err = s.audit(ctx, tx, wid, pid, session, "issue_move", op, outcome, map[string]any{"issue_uuid": issue, "from_team": from, "to_team": str(row, "team_id"), "by": str(row, "by_role"), "lane": str(row, "lane"), "old_identifier": oldID, "new_identifier": newID, "rows_updated": updated}); err != nil {
		return nil, err
	}
	out := issueMoveView(row, false)
	out["rows_updated"] = updated
	return out, nil
}

func issueMoveView(row map[string]any, replay bool) map[string]any {
	d := detailOf(row)
	return map[string]any{"operation_id": str(row, "operation_id"), "issue_uuid": str(row, "subject"), "from_team": str(d, "from_team"), "to_team": str(row, "team_id"), "by": str(row, "by_role"), "lane": str(row, "lane"), "state": str(row, "state"), "old_identifier": str(d, "old_identifier"), "new_identifier": str(row, "result"), "replay": replay}
}

// SetIssueMoveProven is serve --issue-move-proven.
func (s *Store) SetIssueMoveProven(proven bool) { s.issueMoveProven = proven }
