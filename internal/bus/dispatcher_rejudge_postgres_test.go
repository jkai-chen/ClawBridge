package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

// R5 on Postgres: a re-judge resets the issue's budget and allows attempt
// 1 on a revision whose attempt 0 is closed. With two cases on one
// revision the due read and the decision log still read one case each
// (Postgres refuses a scalar subquery of two rows), and the log names
// each judgement's own case. A session and a member are refused.
func TestPostgresRejudge(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	pid := f.projectID()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(q, err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := f.s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(q, err)
		}
		return n
	}
	exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES($1,1,1,'model','{"domains":[]}','2026-01-01T00:00:00Z','x')`, pid)
	f.worker("eng")
	if _, err := f.in("assignment_propose", "eng", map[string]any{"issue_uuid": budgetIssue, "issue_identifier": "FIX-1"}); err != nil {
		t.Fatal(err)
	}
	var subject, revision string
	if err := f.s.db.QueryRowContext(ctx, `SELECT id,revision FROM session_assignments WHERE project_id=$1 AND state='proposed'`, pid).Scan(&subject, &revision); err != nil {
		t.Fatal(err)
	}
	// Attempt 0: its case closed, the model escalated, and two older
	// judgements put the issue at its budget of 3.
	first := newID("case_")
	exec(`INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,attempt,policy_revision,deadline,created_at,closed_at,outcome,result) VALUES($1,$2,'decide','proposal',$3,$4,0,1,$5,$5,$5,'decide','{"guardrail":"permitted","action_taken":"escalate"}')`, first, pid, subject, revision, layout(time.Now().Add(-2*time.Minute)))
	judgement := func(revision, action string, ago time.Duration) {
		t.Helper()
		exec(`INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,subject_revision,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at,kind) VALUES($1,$2,'proposal',$3,$4,'model',$5,'uncertain','','permitted',$5,1,$6,'judgement')`, newID("dcs_"), pid, subject, revision, action, layout(time.Now().Add(-ago)))
	}
	judgement("r0", "wait", 3*time.Minute)
	judgement("r0", "wait", 3*time.Minute)
	judgement(revision, "escalate", time.Minute)
	due := func() map[string]any {
		t.Helper()
		out, err := f.s.ExecuteDispatcher(ctx, pid, DispatcherSessionName, Request{Operation: "dispatcher_cases_due"})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range out.Data.(map[string]any)["cases"].([]any) {
			if d.(map[string]any)["subject_id"] == subject {
				return d.(map[string]any)
			}
		}
		return nil
	}
	if d := due(); d != nil {
		t.Fatal("decided, yet due:", d)
	}
	rejudge := func(principal, sub string, a map[string]any) (Result, error) {
		return f.as(principal, sub, Request{Operation: "dispatcher_model_rejudge", Workspace: f.workspace, Project: "p", Args: a})
	}
	args := map[string]any{"proposal_id": subject, "revision": revision}
	// Refusals first: they record nothing.
	if _, err := f.in("dispatcher_model_rejudge", "eng", map[string]any{"proposal_id": subject, "revision": revision}); code(err) != "forbidden" {
		t.Fatal("a session:", err)
	}
	member := newID("m") + "@identity-test.example"
	f.member(member)
	if _, err := rejudge(member, "sub-member", args); err == nil || !strings.Contains(err.Error(), "owner or lead") {
		t.Fatal("a member:", err)
	}
	if _, err := rejudge(f.owner, "sub-owner", map[string]any{"proposal_id": subject, "revision": "rev_other"}); code(err) != "conflict" {
		t.Fatal("another revision:", err)
	}
	if _, err := rejudge(f.owner, "sub-owner", map[string]any{"proposal_id": "asg_missing", "revision": revision}); code(err) != "not_found" {
		t.Fatal("a missing claim:", err)
	}
	if n := count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE project_id=$1 AND kind='setting_change'`, pid); n != 0 {
		t.Fatal("a refusal recorded", n)
	}
	lead := newID("l") + "@identity-test.example"
	f.member(lead)
	f.role(lead, "lead")
	out, err := rejudge(lead, "sub-lead", args)
	if err != nil || out.Data.(map[string]any)["attempt"] != 1 {
		t.Fatal("a lead's re-judge:", out.Data, err)
	}
	if n := count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE project_id=$1 AND kind='setting_change' AND reason_code='budget_reset' AND subject_type='issue' AND subject_id=$2 AND explanation=$3`, pid, budgetIssue, "by:"+lead); n != 1 {
		t.Fatal("the reset:", n)
	}
	if d := due(); d == nil || d["model_case"] != "" || d["over_budget"] != false || d["budget_left"] != 3 {
		t.Fatal("after the re-judge:", d)
	}
	opened, err := f.s.ExecuteDispatcher(ctx, pid, DispatcherSessionName, Request{Operation: "dispatcher_case_open", Args: map[string]any{
		"kind": "decide", "subject_id": subject, "subject_revision": revision, "policy_revision": 1,
		"permitted": []any{"escalate"}, "issue": map[string]any{"id": budgetIssue}}})
	if err != nil || opened.Data.(map[string]any)["opened"] != true {
		t.Fatal("attempt 1:", opened.Data, err)
	}
	second := opened.Data.(map[string]any)["case_id"].(string)
	if n := count(`SELECT COUNT(*) FROM dispatcher_cases WHERE project_id=$1 AND subject_id=$2 AND subject_revision=$3 AND attempt=1 AND id=$4`, pid, subject, revision, second); n != 1 {
		t.Fatal("attempt 1's case:", n)
	}
	// Two cases on one revision: the due read takes attempt 1's (no
	// agent runs here, so the read closes it for want of one).
	if d := due(); d == nil || d["model_case"] == "" || d["model_case"] == "closed:decide" {
		t.Fatal("the due read with two attempts:", d)
	}
	judgement(revision, "wait", 0)
	listed, err := f.as(f.owner, "sub-owner", Request{Operation: "dispatcher_decisions_list", Workspace: f.workspace, Project: "p", Args: map[string]any{"since_hours": 24, "limit": 50, "kind": "judgement"}})
	if err != nil {
		t.Fatal("the log with two attempts:", err)
	}
	cases := map[string]string{}
	for _, d := range listed.Data.([]any) {
		m := d.(map[string]any)
		if id, _ := m["case_id"].(string); id != "" {
			cases[m["action"].(string)] = id
		}
	}
	if cases["escalate"] != first || cases["wait"] != second {
		t.Fatal("the log's cases:", cases, first, second)
	}
}
