package bus

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const budgetIssue = "dddd1111-1111-4111-8111-111111111111"

// judged seeds n model judgements on subject, at an earlier revision, ago
// before now.
func (c *chatHarness) judged(subject string, n int, ago time.Duration) {
	c.t.Helper()
	for range n {
		if _, err := c.s.db.Exec(`INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,subject_revision,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at,kind) VALUES(?,?,'proposal',?,'r0','model','wait','uncertain','','permitted','wait',1,?,'judgement')`, newID("dcs_"), c.pid, subject, layout(time.Now().Add(-ago))); err != nil {
			c.t.Fatal(err)
		}
	}
}

// openDecide asks the bus, as the dispatcher, for a decide case on a claim.
func (c *chatHarness) openDecide(subject, revision string) map[string]any {
	c.t.Helper()
	out, err := c.s.ExecuteDispatcher(c.ctx, c.pid, DispatcherSessionName, Request{Operation: "dispatcher_case_open", Args: map[string]any{
		"kind": "decide", "subject_id": subject, "subject_revision": revision, "policy_revision": 1,
		"permitted": []any{"escalate"}, "issue": map[string]any{"id": budgetIssue}}})
	if err != nil {
		c.t.Fatal(err)
	}
	return out.Data.(map[string]any)
}

func (c *chatHarness) count(q string, args ...any) int {
	c.t.Helper()
	var n int
	if err := c.s.db.QueryRow(q, args...).Scan(&n); err != nil {
		c.t.Fatal(q, err)
	}
	return n
}

func (c *chatHarness) exec(q string, args ...any) {
	c.t.Helper()
	if _, err := c.s.db.Exec(q, args...); err != nil {
		c.t.Fatal(q, err)
	}
}

// R1–R2: at the default budget of 3, an issue with three judgements in 24
// hours gets no decide case, on this proposal or a new one: the bus records
// one budget_skip per proposal revision and tells a person once. Judgements
// older than 24 hours, a reset after them, or no cap open it again.
func TestLoopBudgetGate(t *testing.T) {
	c := newChatHarness(t)
	subject, revision := c.propose(budgetIssue)
	c.judged(subject, 2, time.Hour)
	due := func() map[string]any {
		t.Helper()
		out, err := c.s.ExecuteDispatcher(c.ctx, c.pid, DispatcherSessionName, Request{Operation: "dispatcher_cases_due"})
		if err != nil {
			t.Fatal(err)
		}
		m := out.Data.(map[string]any)
		for _, d := range m["cases"].([]any) {
			if d.(map[string]any)["subject_id"] == subject {
				d.(map[string]any)["budget"] = m["judgement_budget"]
				return d.(map[string]any)
			}
		}
		t.Fatal("due:", m)
		return nil
	}
	if d := due(); d["over_budget"] != false || d["budget_left"] != 1 || d["budget"] != 3 {
		t.Fatal("two of three used:", d)
	}
	c.judged(subject, 1, time.Minute)
	if d := due(); d["over_budget"] != true || d["budget_left"] != 0 {
		t.Fatal("three of three used:", d)
	}
	for range 2 {
		if out := c.openDecide(subject, revision); out["opened"] != false || out["reason"] != "over_budget" {
			t.Fatal("a case past the budget:", out)
		}
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_cases WHERE kind='decide'`); n != 0 {
		t.Fatal("a case was stored:", n)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE kind='budget_skip' AND subject_id=? AND subject_revision=? AND reason_code='loop_budget' AND action='none' AND explanation='budget:3'`, subject, revision); n != 1 {
		t.Fatal("budget skips:", n)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_escalations WHERE reason_code='loop_budget'`); n != 1 {
		t.Fatal("loop_budget escalations:", n)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE kind='escalation' AND reason_code='loop_budget' AND action='escalate'`); n != 1 {
		t.Fatal("the escalation's decision:", n)
	}
	// The skip and the escalation's row don't decide the claim: it is
	// still due, for the rules.
	due()
	// A new proposal on the same issue is still over: its own skip, no
	// second escalation in the window.
	c.call("assignment_propose", "b", map[string]any{"issue_uuid": budgetIssue, "issue_identifier": "FIX-2"})
	var other, otherRevision string
	if err := c.s.db.QueryRow(`SELECT a.id,a.revision FROM session_assignments a JOIN sessions s ON s.id=a.session_id WHERE s.name='b'`).Scan(&other, &otherRevision); err != nil {
		t.Fatal(err)
	}
	if out := c.openDecide(other, otherRevision); out["reason"] != "over_budget" {
		t.Fatal("a new proposal escaped the budget:", out)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE kind='budget_skip'`); n != 2 {
		t.Fatal("budget skips:", n)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_escalations WHERE reason_code='loop_budget'`); n != 1 {
		t.Fatal("a second loop_budget escalation in the window:", n)
	}
	// Budget 0 is no cap.
	c.exec(`UPDATE dispatcher_policies SET model='{"model_judgement_budget":0}'`)
	if d := due(); d["over_budget"] != false || d["budget_left"] != -1 || d["budget"] != 0 {
		t.Fatal("no cap:", d)
	}
	if out := c.openDecide(other, otherRevision); out["opened"] != true {
		t.Fatal("no cap:", out)
	}
	c.exec(`UPDATE dispatcher_policies SET model='{}'`)
	// A reset after the judgements restores the budget.
	c.exec(`INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,subject_revision,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at,kind) VALUES('dcs_reset',?,'issue',?,'','rules','none','budget_reset','','','none',1,?,'setting_change')`, c.pid, budgetIssue, layout(time.Now()))
	if d := due(); d["over_budget"] != false || d["budget_left"] != 3 {
		t.Fatal("after a reset:", d)
	}
	c.exec(`DELETE FROM dispatcher_decisions WHERE id='dcs_reset'`)
	// Judgements older than 24 hours don't count.
	c.exec(`UPDATE dispatcher_decisions SET created_at=? WHERE kind='judgement'`, layout(time.Now().Add(-25*time.Hour)))
	if out := c.openDecide(subject, revision); out["opened"] != true {
		t.Fatal("past the window:", out)
	}
}

// A repeat open of a revision that has its case answers case_exists, as
// before, and records no budget skip even once the issue is over budget.
func TestLoopBudgetRepeatOpenIsIdempotent(t *testing.T) {
	c := newChatHarness(t)
	subject, revision := c.propose(budgetIssue)
	if out := c.openDecide(subject, revision); out["opened"] != true {
		t.Fatal(out)
	}
	c.judged(subject, 3, time.Minute)
	if out := c.openDecide(subject, revision); out["opened"] != false || out["reason"] != "case_exists" {
		t.Fatal("a repeat open:", out)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE kind IN ('budget_skip','escalation')`); n != 0 {
		t.Fatal("a repeat open recorded", n)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_escalations`); n != 0 {
		t.Fatal("a repeat open escalated", n)
	}
}

// The owner sets the budget with a preview, by compare-and-swap, 0 to 100;
// the change is audited and is a setting_change decision. A lead, a
// session and a stale revision are refused.
func TestLoopBudgetSetting(t *testing.T) {
	c := newChatHarness(t)
	if _, err := c.s.Execute(c.ctx, c.request("dispatcher_model_budget", "a", map[string]any{"budget": 5, "expected_revision": 1})); code(err) != "forbidden" {
		t.Fatal("a session set the budget:", err)
	}
	if _, err := c.advise("lead@example.test", "lead", map[string]any{"budget": 5, "expected_revision": 1, "confirm": true}); err == nil {
		t.Fatal("a lead set the budget")
	}
	for _, bad := range []any{101, -1, "5", nil} {
		if _, err := c.as("", "dispatcher_model_budget", map[string]any{"budget": bad, "expected_revision": 1, "confirm": true}); err == nil {
			t.Fatal("budget", bad, "accepted")
		}
	}
	preview, err := c.as("", "dispatcher_model_budget", map[string]any{"budget": 5, "expected_revision": 1})
	if err != nil || preview["preview"] != true || preview["model_judgement_budget"] != 5 || preview["was"] != 3 {
		t.Fatal("the preview:", preview, err)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_decisions`); n != 0 {
		t.Fatal("a preview recorded", n)
	}
	if _, err = c.as("", "dispatcher_model_budget", map[string]any{"budget": 5, "expected_revision": 7, "confirm": true}); code(err) != "conflict" {
		t.Fatal("a stale revision:", err)
	}
	set, err := c.as("", "dispatcher_model_budget", map[string]any{"budget": 0, "expected_revision": 1, "confirm": true})
	if err != nil || fmt.Sprint(set["revision"]) != "2" {
		t.Fatal(set, err)
	}
	var model string
	if err = c.s.db.QueryRow(`SELECT model FROM dispatcher_policies`).Scan(&model); err != nil || model != `{"model_judgement_budget":0}` {
		t.Fatal(model, err)
	}
	var subjectType, subject, engine, reason, explanation, action string
	var policyRevision int
	if err = c.s.db.QueryRow(`SELECT subject_type,subject_id,engine,reason_code,explanation,action,policy_revision FROM dispatcher_decisions WHERE kind='setting_change'`).Scan(&subjectType, &subject, &engine, &reason, &explanation, &action, &policyRevision); err != nil {
		t.Fatal(err)
	}
	if subjectType != "project" || subject != c.pid || engine != "rules" || reason != "budget_set" || explanation != "budget:0" || action != "none" || policyRevision != 2 {
		t.Fatal(subjectType, subject, engine, reason, explanation, action, policyRevision)
	}
	if n := c.count(`SELECT COUNT(*) FROM history WHERE action='dispatcher_model_budget'`); n != 1 {
		t.Fatal("audit:", n)
	}
}

// advise runs an owner-switch op as principal holding role, as the hosted
// authority would resolve it.
func (c *chatHarness) advise(principal, role string, a map[string]any) (any, error) {
	c.t.Helper()
	scoped := *c.s
	scoped.principal = principal
	tx, err := c.s.db.BeginTx(c.ctx, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	defer tx.Rollback()
	t := &transaction{Tx: tx}
	w, err := one(c.ctx, t, `SELECT * FROM workspaces`)
	if err != nil {
		c.t.Fatal(err)
	}
	p, err := one(c.ctx, t, `SELECT * FROM projects WHERE id=?`, c.pid)
	if err != nil {
		c.t.Fatal(err)
	}
	return scoped.dispatcherAdviseOp(c.ctx, t, w, p, role, Request{Operation: "dispatcher_model_budget", Args: a})
}

// R3: every row has a kind, and the log filters by one of the five; an
// unknown kind is refused. A rules decision past the budget reads as
// over_budget.
func TestDecisionKindsFilter(t *testing.T) {
	c := newChatHarness(t)
	subject, revision := c.propose(budgetIssue)
	c.judged(subject, 1, time.Minute)
	c.exec(`INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,subject_revision,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at,kind) VALUES('dcs_r',?,'proposal',?,?,'rules','wait','facts_unavailable','model_fallback:loop_budget','permitted','wait',1,?,'rules')`, c.pid, subject, revision, layout(time.Now()))
	if _, err := c.as("", "dispatcher_model_budget", map[string]any{"budget": 4, "expected_revision": 1, "confirm": true}); err != nil {
		t.Fatal(err)
	}
	list := func(kind string) []any {
		t.Helper()
		a := map[string]any{"since_hours": 24, "limit": 50}
		if kind != "" {
			a["kind"] = kind
		}
		out, err := c.op("x@example.test", "owner", "dispatcher_decisions_list", a)
		if err != nil {
			t.Fatal(kind, err)
		}
		return out.([]any)
	}
	if all := list(""); len(all) != 3 {
		t.Fatal(all)
	}
	for kind, want := range map[string]int{"judgement": 1, "rules": 1, "setting_change": 1, "escalation": 0, "budget_skip": 0} {
		got := list(kind)
		if len(got) != want {
			t.Fatal(kind, got)
		}
		for _, d := range got {
			m := d.(map[string]any)
			if m["kind"] != kind || m["over_budget"] != (kind == "rules") {
				t.Fatal(kind, m)
			}
		}
	}
	if _, err := c.op("x@example.test", "owner", "dispatcher_decisions_list", map[string]any{"since_hours": 24, "limit": 50, "kind": "advice"}); code(err) != "invalid" {
		t.Fatal("an unknown kind:", err)
	}
}

// Schema 54 on SQLite: old decisions get their kind from their engine, an
// escalation has the payload columns, and a case's attempt is in the
// subject index.
func TestLoopBudgetMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(loopBudgetDowngradeSQLite + `PRAGMA user_version=53;
INSERT INTO workspaces(id,name,mode,retention_days,owner_principal,created_at) VALUES('w','w','local',30,'o','2026-01-01T00:00:00Z');
INSERT INTO projects(id,workspace_id,slug,name,created_at) VALUES('prj_1','w','p','p','2026-01-01T00:00:00Z');
INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,engine,proposal,reason_code,guardrail,action,policy_revision,created_at) VALUES('d_m','prj_1','proposal','asg_1','model','wait','uncertain','permitted','wait',1,'2026-01-01T00:00:00Z'),('d_r','prj_1','proposal','asg_1','rules','wait','uncertain','permitted','wait',1,'2026-01-01T00:00:00Z');
INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,deadline,created_at) VALUES('case_old','prj_1','decide','proposal','asg_old','1','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),('chat_old','prj_1','chat','project','prj_1','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	kinds := map[string]string{}
	rows, err := s.db.Query(`SELECT id,kind FROM dispatcher_decisions`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, kind string
		if err = rows.Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		kinds[id] = kind
	}
	rows.Close()
	if kinds["d_m"] != "judgement" || kinds["d_r"] != "rules" {
		t.Fatal(kinds)
	}
	if _, err = s.db.Exec(`SELECT kind,question,options,suggested,timeout_at,answer,answered_by,answered_at FROM dispatcher_escalations LIMIT 0`); err != nil {
		t.Fatal(err)
	}
	// Cases from before keep their rows at attempt 0, under the rebuilt
	// index.
	var old int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM dispatcher_cases WHERE id IN ('case_old','chat_old') AND attempt=0`).Scan(&old); err != nil || old != 2 {
		t.Fatal("cases from before:", old, err)
	}
	if _, err = s.db.Exec(`INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,attempt,deadline,created_at) VALUES('case_dup','prj_1','decide','proposal','asg_old','1',0,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("a second attempt 0 beside a case from before")
	}
	insert := `INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,attempt,deadline,created_at) VALUES(?,'prj_1','decide','proposal','asg_1','1',?,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`
	if _, err = s.db.Exec(insert, "case_0", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(insert, "case_1", 1); err != nil {
		t.Fatal("a second attempt on the revision:", err)
	}
	if _, err = s.db.Exec(insert, "case_2", 1); err == nil {
		t.Fatal("the same attempt twice")
	}
	// The window's dedupe reads the escalations through their reason index.
	plan, err := s.db.Query(`EXPLAIN QUERY PLAN `+loopBudgetToldQuery, "prj_1", budgetIssue, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err = plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, detail)
	}
	plan.Close()
	if !strings.Contains(strings.Join(steps, "; "), "dispatcher_escalations_reason") {
		t.Fatal("the dedupe doesn't use the reason index:", steps)
	}
	var version int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != loopBudgetSchemaVersion {
		t.Fatal(version, err)
	}
}
