package bus

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Judge again (v0.4, R5): the project owner or a lead asks the model to
// judge a pending claim once more, with dispatcher_model_rejudge. It
// resets the loop budget of the claim's issue (a setting_change decision,
// reason budget_reset, on the issue: the only writer of that reason) and
// allows one more model case on the claim's current revision (a
// setting_change decision, reason rejudge, on the claim at that revision).
// A claim's attempt is how many rejudge decisions its revision has: the
// next decide case opens at that attempt, so each re-judge allows exactly
// one more case. A session or the model never re-judges.

// errRejudgeFromSession refuses a re-judge through a session or an
// instance: it is a person's, as answering an escalation is.
var errRejudgeFromSession = scoped("a session can never ask the model to judge again; the project owner or a lead runs it without --session", "forbidden")

// The SQL below is the one source of a claim revision's re-judges: the
// attempt its next decide case opens at (dispatcherCaseOpen, the due read
// and the op itself) and when it was last re-judged (the finality and the
// waits the due read counts). Each takes SQL expressions for the project,
// the claim and its revision: placeholders or a query's columns.

// rejudgeWhere matches the re-judge decisions r of claim subject at
// revision in project.
func rejudgeWhere(project, subject, revision string) string {
	return "r.project_id=" + project + " AND r.kind='setting_change' AND r.reason_code='rejudge' AND r.subject_type='proposal' AND r.subject_id=" + subject + " AND r.subject_revision=" + revision
}

// rejudgeCountSQL is how many times the claim revision was re-judged: the
// attempt its next decide case opens at.
func rejudgeCountSQL(project, subject, revision string) string {
	return "(SELECT COUNT(*) FROM dispatcher_decisions r WHERE " + rejudgeWhere(project, subject, revision) + ")"
}

// rejudgedAtSQL is when the claim revision was last re-judged, empty if
// never: what was decided before it no longer counts.
func rejudgedAtSQL(project, subject, revision string) string {
	return "COALESCE((SELECT MAX(r.created_at) FROM dispatcher_decisions r WHERE " + rejudgeWhere(project, subject, revision) + "),'')"
}

// finalDecisionSQL is true when the claim revision has a final decision
// (a rules or model decision other than wait or superseded) since its
// latest re-judge.
func finalDecisionSQL(project, subject, revision string) string {
	return "EXISTS (SELECT 1 FROM dispatcher_decisions f WHERE f.project_id=" + project + " AND f.subject_id=" + subject + " AND f.subject_revision=" + revision +
		" AND f.kind IN ('rules','judgement') AND f.action NOT IN ('wait','superseded') AND f.created_at>" + rejudgedAtSQL("f.project_id", "f.subject_id", "f.subject_revision") + ")"
}

// claimAttempt is the attempt a decide case on subject at revision opens
// at: 0 until a person asks for another judgement.
func claimAttempt(ctx context.Context, tx *transaction, pid, subject, revision string) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT "+rejudgeCountSQL("?", "?", "?"), pid, subject, revision).Scan(&n)
	return n, err
}

// lockedStamp is the clock after the op took its locks. On Postgres
// dbStamp is the transaction's start; a re-judge stamped then could sort
// before a decision committed while it waited for the workspace lock, and
// that decision would then end the re-judge's attempt unseen (R5).
func lockedStamp(ctx context.Context, tx *transaction) (string, error) {
	if !tx.postgres {
		return stamp(), nil
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return "", err
	}
	return now.UTC().Format(timestampLayout), nil
}

// rejudge runs dispatcher_model_rejudge for the project owner or a lead:
// the claim must still be pending at the revision the person saw, in model
// mode, with an issue to reset. A re-judge the model hasn't had its case
// for yet, or a case the model is still on, is a conflict, so two
// re-judges always mean two more cases.
func (s *Store) rejudge(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, r Request) (any, error) {
	if projectRole != "owner" && projectRole != "lead" {
		return nil, errors.New("project owner or lead permission required")
	}
	pid := str(p, "id")
	subject, revision := str(r.Args, "proposal_id"), str(r.Args, "revision")
	if subject == "" || revision == "" {
		return nil, invalid("dispatcher_model_rejudge needs proposal_id and the revision you saw")
	}
	_, meta, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if meta["enabled"] != true || meta["mode"] != "model" {
		return nil, invalid("judging again needs the dispatcher on with the model engine (dispatcher enable --engine model)")
	}
	rows, err := query(ctx, tx, `SELECT id,state,revision,issue_uuid FROM session_assignments WHERE project_id=? AND id=?`, pid, subject)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, scoped("no claim "+subject+" in this project", "not_found")
	}
	claim := rows[0]
	if str(claim, "state") != "proposed" || str(claim, "revision") != revision {
		return nil, conflict(fmt.Sprintf("the claim has moved on: it is %s at revision %s", str(claim, "state"), str(claim, "revision")))
	}
	issue := str(claim, "issue_uuid")
	if issue == "" {
		return nil, invalid("this claim names no issue, so it has no budget to reset and the model has nothing to judge")
	}
	attempt, err := claimAttempt(ctx, tx, pid, subject, revision)
	if err != nil {
		return nil, err
	}
	cases, err := query(ctx, tx, `SELECT id,closed_at FROM dispatcher_cases WHERE project_id=? AND kind='decide' AND subject_id=? AND subject_revision=? AND attempt=?`, pid, subject, revision, attempt)
	if err != nil {
		return nil, err
	}
	switch {
	case len(cases) == 1 && cases[0]["closed_at"] == nil:
		return nil, conflict("the model is judging this claim now (case " + str(cases[0], "id") + "); judge again once it has decided")
	case len(cases) == 0 && attempt > 0:
		// The last re-judge still waits for its case, unless the rules
		// decided the claim first.
		decided, err := query(ctx, tx, `SELECT 1 FROM session_assignments a WHERE a.project_id=? AND a.id=? AND `+finalDecisionSQL("a.project_id", "a.id", "a.revision"), pid, subject)
		if err != nil {
			return nil, err
		}
		if len(decided) == 0 {
			return nil, conflict(fmt.Sprintf("this claim was already re-judged and waits for the model's case (attempt %d)", attempt))
		}
	}
	// Stamped after the workspace lock, so a decision committed while this
	// waited for it sorts before the re-judge.
	nowRaw, err := lockedStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	by := "by:" + s.principal
	if err = controlDecision(ctx, tx, pid, "setting_change", "issue", issue, "", "", "budget_reset", by, "none", nowRaw); err != nil {
		return nil, err
	}
	if err = controlDecision(ctx, tx, pid, "setting_change", "proposal", subject, revision, "", "rejudge", by, "none", nowRaw); err != nil {
		return nil, err
	}
	next := attempt + 1
	if err = s.audit(ctx, tx, str(w, "id"), pid, "", "assignment", subject, "dispatcher_model_rejudge", map[string]any{"revision": revision, "issue_uuid": issue, "attempt": next}); err != nil {
		return nil, err
	}
	return map[string]any{"rejudge": true, "proposal_id": subject, "revision": revision, "attempt": next,
		"note": "the issue's loop budget is reset; the dispatcher's next tick opens the model's case for this claim"}, nil
}
