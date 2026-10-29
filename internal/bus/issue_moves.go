package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
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

// A family move (r6): one write on the parent, whose sub-issues Linear
// moves with it. Each child is recorded as its own issue_move_child row,
// under the parent's operation, and settled from its own read-back.
const (
	issueMoveChildKind = "issue_move_child"
	// maxFamilyChildren bounds a family (O8).
	maxFamilyChildren = 50
)

// parentDetail is a move's detail: its source team, and a family's
// children, which find the children's rows again.
func parentDetail(from string, children []string) map[string]any {
	d := map[string]any{"from_team": from}
	if len(children) > 0 {
		d["children"] = children
	}
	return d
}

// childPrefix is what a family's child rows' operation IDs start with:
// the family's operation, or its digest when that would make a child's ID
// longer than an operation ID may be (128).
func childPrefix(op string) string {
	if len(op)+len(":c:")+36 > 128 {
		sum := sha256.Sum256([]byte(op))
		op = hex.EncodeToString(sum[:16])
	}
	return op + ":c:"
}

// childOperation is a child's row under its family's operation: the same
// for every replay of the family.
func childOperation(op, child string) string { return childPrefix(op) + child }

// maxMoveCalls bounds one move's estimate: a family of the most children,
// each read twice, with its plan's reads.
const maxMoveCalls = 400

// moveCalls is the Linear calls a move's client estimates it makes, from
// its plan: at least the write and its read-back.
func moveCalls(a map[string]any) (int, error) {
	n, err := integer(a["calls"])
	if err != nil || n < 2 || n > maxMoveCalls {
		return 0, invalid(fmt.Sprintf("calls must be the move's estimated Linear calls, 2 to %d", maxMoveCalls))
	}
	return n, nil
}

// familyChildren reads a reserve's children: distinct issue UUIDs, never
// the parent, at most maxFamilyChildren.
func familyChildren(a map[string]any, parent string) ([]string, error) {
	raw, present := a["children"]
	if !present {
		return nil, nil
	}
	list, ok := raw.([]any)
	if strs, isStrings := raw.([]string); isStrings {
		list, ok = make([]any, len(strs)), true
		for i, v := range strs {
			list[i] = v
		}
	}
	if !ok || len(list) > maxFamilyChildren {
		return nil, invalid(fmt.Sprintf("children must be a list of at most %d issue UUIDs", maxFamilyChildren))
	}
	var out []string
	for _, v := range list {
		id, err := lowerUUID(map[string]any{"id": v}, "id", true)
		if err != nil {
			return nil, err
		}
		if id == parent || slices.Contains(out, id) {
			return nil, invalid("children must be distinct issues, none of them the parent")
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}

// familyRows are the child rows reserved under a family's parent row, by
// child UUID: the parent's detail lists its children, and each child's
// row is read by its exact operation ID, never by a range, whose order
// would depend on the database's collation.
func familyRows(ctx context.Context, tx *transaction, pid, op string, parent map[string]any) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	kids, _ := detailOf(parent)["children"].([]any)
	for _, k := range kids {
		child, _ := k.(string)
		row, err := one(ctx, tx, `SELECT * FROM linear_write_reservations WHERE project_id=? AND operation_id=? AND kind=?`, pid, childOperation(op, child), issueMoveChildKind)
		if err != nil {
			return nil, fmt.Errorf("a family's child row is missing: %w", err)
		}
		out[child] = row
	}
	return out, nil
}

// issueIdentifierStores are the tables that keep an issue's display
// identifier beside its UUID; an accepted move rewrites each (M8). Every
// other store is keyed by UUID. TestIssueMoveUpdatesEveryIdentifierStore
// pins this list against the schema.
var issueIdentifierStores = []string{"session_assignments", "intake_queue"}

func issueMoveOperation(op string) bool {
	return op == "issue_move_reserve" || op == "issue_move_settle" || op == "issue_move_status"
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
	if r.Operation == "issue_move_status" {
		return issueMoveStatus(ctx, tx, pid, actor, s.principal, op)
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
	children, err := familyChildren(a, issue)
	if err != nil {
		return nil, err
	}
	if len(children) > 0 && !s.issueFamilyMoveProven {
		return nil, scoped("issue_family_move_unproven: this server confirms no family move until its operator sets serve --issue-family-move-proven after the family probe; the preview works", "forbidden")
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
	calls, err := moveCalls(a)
	if err != nil {
		return nil, err
	}
	// The estimate comes from the client; a family's floor is the write,
	// its read-back, and each child's sub-issue read and read-back.
	if floor := 2 + 2*len(children); calls < floor {
		return nil, invalid(fmt.Sprintf("calls must be at least %d for a move of %d sub-issues", floor, len(children)))
	}
	// The move's Linear calls come from the project's hourly budget (FR8's
	// first slice, r6 O2): charged before anything reaches Linear, a family
	// in full, in the reservation's own transaction. No room refuses a
	// session's move, sending nothing; a person without a session is never
	// refused for budget (FR8 Q2), only charged, past the limit if need be.
	// A repeat of a move already reserved is never charged again.
	prior, err := query(ctx, tx, `SELECT state FROM linear_write_reservations WHERE project_id=? AND operation_id=?`, pid, op)
	if err != nil {
		return nil, err
	}
	if len(prior) == 0 || str(prior[0], "state") == "not_sent" {
		budget, err := s.projectBudget(ctx, tx, pid)
		if err != nil {
			return nil, err
		}
		if actor != nil && budget.used+calls > budget.limit {
			return nil, &serviceError{code: "rate_limited", cause: fmt.Errorf("the project's hourly Linear budget has %d of %d calls left, and this move needs %d; retry after %s; nothing was sent", max(budget.limit-budget.used, 0), budget.limit, calls, budget.resetAt.UTC().Format(time.RFC3339)), public: true}
		}
		if err = budget.charge(ctx, tx, pid, calls); err != nil {
			return nil, err
		}
	}
	canonical := issue + "\n" + from + "\n" + to + "\n" + plan
	if len(children) > 0 {
		canonical += "\n" + strings.Join(children, ",")
	}
	row, replay, err := reserveLinearWrite(ctx, tx, pid, linearWrite{Kind: issueMoveKind, Operation: op, Session: session, Principal: s.principal, ByRole: by, Lane: lane, Team: to, Subject: issue, Plan: canonical, Detail: parentDetail(from, children)}, now)
	if err != nil {
		return nil, err
	}
	out := issueMoveView(row, replay)
	if len(children) > 0 {
		var views []map[string]any
		for _, child := range children {
			crow, _, err := reserveLinearWrite(ctx, tx, pid, linearWrite{Kind: issueMoveChildKind, Operation: childOperation(op, child), Session: session, Principal: s.principal, ByRole: by, Lane: lane, Team: to, Subject: child, Plan: canonical + "\n" + op, Detail: map[string]any{"parent": issue, "from_team": from}}, now)
			if err != nil {
				return nil, err
			}
			views = append(views, issueMoveView(crow, replay))
		}
		out["children"] = views
	}
	if !replay {
		// History by IDs only: no title, no names.
		if err = s.audit(ctx, tx, wid, pid, session, "issue_move", op, "reserve", map[string]any{"issue_uuid": issue, "from_team": from, "to_team": to, "by": by, "lane": lane, "children": len(children), "budget_calls": calls}); err != nil {
			return nil, err
		}
	}
	return out, nil
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
	family, err := familyRows(ctx, tx, pid, op, row)
	if err != nil {
		return nil, err
	}
	settles, err := childSettles(a, family, outcome)
	if err != nil {
		return nil, err
	}
	row, replay, err := settleLinearWrite(ctx, tx, pid, row, outcome, newID, detail, at(nowRaw))
	if err != nil {
		return nil, err
	}
	issue, from := str(row, "subject"), str(detailOf(row), "from_team")
	updated := 0
	if !replay && outcome == "accepted" {
		if updated, err = rewriteIdentifier(ctx, tx, pid, issue, newID); err != nil {
			return nil, err
		}
	}
	var views []map[string]any
	for _, c := range settles {
		crow, creplay, err := settleLinearWrite(ctx, tx, pid, family[c.issue], c.outcome, c.newID, c.detail, at(nowRaw))
		if err != nil {
			return nil, err
		}
		if !creplay && c.outcome == "accepted" {
			n, err := rewriteIdentifier(ctx, tx, pid, c.issue, c.newID)
			if err != nil {
				return nil, err
			}
			updated += n
		}
		views = append(views, issueMoveView(crow, creplay))
	}
	if replay {
		out := issueMoveView(row, true)
		if views != nil {
			out["children"] = views
		}
		return out, nil
	}
	if err = s.audit(ctx, tx, wid, pid, session, "issue_move", op, outcome, map[string]any{"issue_uuid": issue, "from_team": from, "to_team": str(row, "team_id"), "by": str(row, "by_role"), "lane": str(row, "lane"), "old_identifier": oldID, "new_identifier": newID, "rows_updated": updated, "children": len(settles)}); err != nil {
		return nil, err
	}
	out := issueMoveView(row, false)
	out["rows_updated"] = updated
	if views != nil {
		out["children"] = views
	}
	return out, nil
}

// rewriteIdentifier sets an issue's stored display identifier in every
// store that keeps one (M8).
func rewriteIdentifier(ctx context.Context, tx *transaction, pid, issue, identifier string) (int, error) {
	updated := 0
	for _, table := range issueIdentifierStores {
		res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET issue_identifier=? WHERE project_id=? AND issue_uuid=?`, identifier, pid, issue)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		updated += int(n)
	}
	return updated, nil
}

// childSettle is one child's outcome in a family's settle.
type childSettle struct {
	issue, outcome, newID string
	detail                map[string]any
}

// childSettles reads a settle's children against the rows reserved for the
// family. A parent not sent leaves every child not sent. Otherwise every
// reserved child is named exactly once, and an accepted family has every
// child accepted (r5: one that didn't follow makes the family uncertain).
func childSettles(a map[string]any, family map[string]map[string]any, outcome string) ([]childSettle, error) {
	raw, present := a["children"]
	if len(family) == 0 {
		if present {
			return nil, invalid("this move reserved no children")
		}
		return nil, nil
	}
	var out []childSettle
	if outcome == "not_sent" {
		for issue := range family {
			out = append(out, childSettle{issue: issue, outcome: "not_sent"})
		}
		slices.SortFunc(out, func(x, y childSettle) int { return strings.Compare(x.issue, y.issue) })
		return out, nil
	}
	list, ok := raw.([]any)
	if !ok || len(list) != len(family) {
		return nil, invalid("a family's settle names each of its reserved children once")
	}
	for _, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, invalid("each child is {issue_uuid, outcome, old_identifier, new_identifier}")
		}
		issue, err := lowerUUID(m, "issue_uuid", true)
		if err != nil {
			return nil, err
		}
		if family[issue] == nil || slices.ContainsFunc(out, func(c childSettle) bool { return c.issue == issue }) {
			return nil, invalid("a family's settle names each of its reserved children once")
		}
		c := childSettle{issue: issue, outcome: str(m, "outcome"), newID: str(m, "new_identifier")}
		if c.outcome != "accepted" && c.outcome != "uncertain" {
			return nil, invalid("a sent family's child is accepted or uncertain")
		}
		oldID := str(m, "old_identifier")
		for _, id := range []string{oldID, c.newID} {
			if id != "" && !issueIdentifierPattern.MatchString(id) {
				return nil, invalid("identifiers must be Linear display identifiers")
			}
		}
		if c.outcome == "accepted" && c.newID == "" {
			return nil, invalid("an accepted child names its new identifier")
		}
		if outcome == "accepted" && c.outcome != "accepted" {
			return nil, invalid("a family is accepted only when every child is")
		}
		if oldID != "" {
			c.detail = map[string]any{"old_identifier": oldID}
		}
		out = append(out, c)
	}
	return out, nil
}

func issueMoveView(row map[string]any, replay bool) map[string]any {
	d := detailOf(row)
	return map[string]any{"operation_id": str(row, "operation_id"), "issue_uuid": str(row, "subject"), "from_team": str(d, "from_team"), "to_team": str(row, "team_id"), "by": str(row, "by_role"), "lane": str(row, "lane"), "state": str(row, "state"), "old_identifier": str(d, "old_identifier"), "new_identifier": str(row, "result"), "replay": replay}
}

// issueMoveStatus reads the caller's own move under op, for a batch's
// resume (r6 P2): its state and new identifier, and whether an uncertain
// one was acknowledged. A move never reserved answers state "none".
func issueMoveStatus(ctx context.Context, tx *transaction, pid string, actor map[string]any, principal, op string) (any, error) {
	session := ""
	if actor != nil {
		session = str(actor, "id")
	}
	rows, err := query(ctx, tx, `SELECT * FROM linear_write_reservations WHERE project_id=? AND operation_id=? AND kind=? AND session_id=? AND principal=?`, pid, op, issueMoveKind, session, principal)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return map[string]any{"operation_id": op, "state": "none"}, nil
	}
	out := issueMoveView(rows[0], false)
	delete(out, "replay")
	out["acknowledged"] = rows[0]["acknowledged_at"] != nil
	return out, nil
}

// SetIssueMoveProven is serve --issue-move-proven.
func (s *Store) SetIssueMoveProven(proven bool) { s.issueMoveProven = proven }

// SetIssueFamilyMoveProven is serve --issue-family-move-proven.
func (s *Store) SetIssueFamilyMoveProven(proven bool) { s.issueFamilyMoveProven = proven }
