package bus

import (
	"context"
	"fmt"
	"time"
)

// The dispatcher model's loop budget (v0.4, R1–R3):
// at most model_judgement_budget model judgements per issue in 24 hours,
// counted across its proposals and revisions from the issue's latest reset.
// Past it the bus opens no decide case: the rules decide, the record says
// model_fallback:loop_budget, and a person is told once per issue per
// window. Every control action is a decision row of one kind, so the log
// shows the judgements, the rules' decisions, escalations, budget skips
// and setting changes apart.

// loopBudgetSchemaVersion is the decision kinds, the escalation payload
// and the case attempts.
const loopBudgetSchemaVersion = 54

// loopBudgetSchema is valid for SQLite and Postgres.
//   - dispatcher_decisions gains kind; old rows are judgements (engine
//     model) or rules. The kinds are checked in Go (decisionKinds): a CHECK
//     on an existing SQLite table needs a rebuild.
//   - dispatcher_escalations gains the payload an escalation may carry
//     (its kind, question, options as JSON, the suggested option and a
//     timeout) and its answer, all optional, and an index for finding
//     an issue's recent escalations of one reason.
//   - dispatcher_cases gains attempt, so one subject revision can have
//     another model case after a person asks for it; the unique index
//     includes it.
const loopBudgetSchema = `
ALTER TABLE dispatcher_decisions ADD COLUMN kind TEXT NOT NULL DEFAULT '';
UPDATE dispatcher_decisions SET kind=CASE WHEN engine='model' THEN 'judgement' ELSE 'rules' END;
CREATE INDEX dispatcher_decisions_kind ON dispatcher_decisions(project_id,kind,created_at);
ALTER TABLE dispatcher_escalations ADD COLUMN kind TEXT;
ALTER TABLE dispatcher_escalations ADD COLUMN question TEXT;
ALTER TABLE dispatcher_escalations ADD COLUMN options TEXT;
ALTER TABLE dispatcher_escalations ADD COLUMN suggested TEXT;
ALTER TABLE dispatcher_escalations ADD COLUMN timeout_at TEXT;
ALTER TABLE dispatcher_escalations ADD COLUMN answer TEXT;
ALTER TABLE dispatcher_escalations ADD COLUMN answered_by TEXT;
ALTER TABLE dispatcher_escalations ADD COLUMN answered_at TEXT;
CREATE INDEX dispatcher_escalations_reason ON dispatcher_escalations(project_id,reason_code,created_at);
ALTER TABLE dispatcher_cases ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0;
DROP INDEX dispatcher_cases_subject;
CREATE UNIQUE INDEX dispatcher_cases_subject ON dispatcher_cases(project_id,kind,subject_id,subject_revision,attempt);
`

// loopBudgetToldQuery finds an issue's loop_budget escalation in its
// window, through dispatcher_escalations_reason.
const loopBudgetToldQuery = `SELECT 1 FROM dispatcher_escalations e JOIN session_assignments a ON a.project_id=e.project_id AND a.id=e.subject_id WHERE e.project_id=? AND e.reason_code='loop_budget' AND a.issue_uuid=? AND e.created_at>=? AND e.created_at>?`

const (
	// defaultJudgementBudget is the budget when the owner set none.
	defaultJudgementBudget = 3
	maxJudgementBudget     = 100
	// judgementWindow is how far back judgements count.
	judgementWindow = 24 * time.Hour
)

// decisionKinds are the kinds a decision row may have.
var decisionKinds = map[string]bool{"judgement": true, "rules": true, "escalation": true, "budget_skip": true, "setting_change": true}

// judgementBudget is the owner's budget: 0 is no cap.
func (m modelSettings) judgementBudget() int {
	if m.JudgementBudget == nil {
		return defaultJudgementBudget
	}
	return *m.JudgementBudget
}

// budgetWindow is where issue's judgement count starts: 24 hours ago, and
// after its latest reset (a setting_change with reason budget_reset on the
// issue). reset is "" when there is none.
func budgetWindow(ctx context.Context, tx *transaction, pid, issue, nowRaw string) (since, reset string, err error) {
	rows, err := query(ctx, tx, `SELECT MAX(created_at) AS latest FROM dispatcher_decisions WHERE project_id=? AND kind='setting_change' AND reason_code='budget_reset' AND subject_type='issue' AND subject_id=?`, pid, issue)
	if err != nil {
		return "", "", err
	}
	if len(rows) == 1 {
		reset = str(rows[0], "latest")
	}
	return layout(at(nowRaw).Add(-judgementWindow)), reset, nil
}

// issueJudgements counts the model's judgements on issue (R2): decisions of
// kind judgement on any of its proposals, in the window budgetWindow gives.
// Every judgement counts, whatever it chose and whether it stood.
func issueJudgements(ctx context.Context, tx *transaction, pid, issue, nowRaw string) (int, error) {
	since, reset, err := budgetWindow(ctx, tx, pid, issue, nowRaw)
	if err != nil {
		return 0, err
	}
	var n int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dispatcher_decisions d JOIN session_assignments a ON a.project_id=d.project_id AND a.id=d.subject_id WHERE d.project_id=? AND d.kind='judgement' AND d.subject_type='proposal' AND a.issue_uuid=? AND d.created_at>=? AND d.created_at>?`, pid, issue, since, reset).Scan(&n)
	return n, err
}

// overBudget reports whether issue has used pid's judgement budget, and
// what is left of it (-1 with no cap). An issue the bus can't name has no
// budget.
func overBudget(ctx context.Context, tx *transaction, pid, issue, nowRaw string) (bool, int, error) {
	settings, _, err := readModelSettings(ctx, tx, pid)
	if err != nil {
		return false, 0, err
	}
	budget := settings.judgementBudget()
	if budget == 0 || issue == "" {
		return false, -1, nil
	}
	n, err := issueJudgements(ctx, tx, pid, issue, nowRaw)
	if err != nil {
		return false, 0, err
	}
	return n >= budget, max(budget-n, 0), nil
}

// controlDecision records a control action that isn't a decision on a
// claim: an escalation, a budget skip or a setting change. Its engine is
// rules (the column requires one) and its policy revision the current.
func controlDecision(ctx context.Context, tx *transaction, pid, kind, subjectType, subject, revision, domain, reason, explanation, action, nowRaw string) error {
	if !decisionKinds[kind] {
		return fmt.Errorf("unknown decision kind %q", kind)
	}
	var policyRevision int64
	rows, err := query(ctx, tx, `SELECT revision FROM dispatcher_policies WHERE project_id=?`, pid)
	if err != nil {
		return err
	}
	if len(rows) == 1 {
		policyRevision = int64(asInt(rows[0]["revision"]))
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,subject_revision,domain,facts_source,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at,kind) VALUES(?,?,?,?,?,?,'','rules',?,?,?,'',?,?,?,?)`,
		newID("dcs_"), pid, subjectType, subject, revision, domain, action, reason, trimTo(explanation, 280), action, policyRevision, nowRaw, kind)
	return err
}

// loopBudgetStop records that a claim past its issue's budget gets no model
// case (R1): a budget_skip decision once per proposal revision, and a
// loop_budget escalation once per issue per window. proposal is the
// pending claim's row with its session's name. It re-counts first, so a
// caller's stale view records nothing; it reports whether the claim is
// over.
func (s *Store) loopBudgetStop(ctx context.Context, tx *transaction, w map[string]any, pid, domainID string, proposal map[string]any) (bool, error) {
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return false, err
	}
	issue := str(proposal, "issue_uuid")
	over, _, err := overBudget(ctx, tx, pid, issue, nowRaw)
	if err != nil || !over {
		return false, err
	}
	settings, _, err := readModelSettings(ctx, tx, pid)
	if err != nil {
		return false, err
	}
	subject, revision := str(proposal, "id"), str(proposal, "revision")
	skipped, err := query(ctx, tx, `SELECT 1 FROM dispatcher_decisions WHERE project_id=? AND kind='budget_skip' AND subject_id=? AND subject_revision=?`, pid, subject, revision)
	if err != nil {
		return false, err
	}
	if len(skipped) == 0 {
		if err = controlDecision(ctx, tx, pid, "budget_skip", "proposal", subject, revision, domainID, "loop_budget", fmt.Sprintf("budget:%d", settings.judgementBudget()), "none", nowRaw); err != nil {
			return false, err
		}
	}
	since, reset, err := budgetWindow(ctx, tx, pid, issue, nowRaw)
	if err != nil {
		return false, err
	}
	told, err := query(ctx, tx, loopBudgetToldQuery, pid, issue, since, reset)
	if err != nil || len(told) > 0 {
		return true, err
	}
	domains, _, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return false, err
	}
	if w == nil {
		if w, err = one(ctx, tx, `SELECT w.* FROM workspaces w JOIN projects p ON p.workspace_id=w.id WHERE p.id=?`, pid); err != nil {
			return false, err
		}
	}
	_, _, err = s.dispatcherEscalate(ctx, tx, w, pid, domains, domainID, proposal, "loop_budget", "")
	return true, err
}
