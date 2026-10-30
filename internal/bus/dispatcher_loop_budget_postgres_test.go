package bus

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The loop budget on Postgres: the gate refuses a decide case past it,
// once-only skip and escalation; the owner's setting is a setting_change
// decision; the log filters by kind; a member is refused.
func TestPostgresLoopBudget(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	var pid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
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
	registered := f.must(f.owner, "sub-owner", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "eng", Args: map[string]any{"instance": "eng-i"}})
	instance := registered.Data.(map[string]any)["instance"].(string)
	f.must(f.owner, "sub-owner", Request{Operation: "assignment_propose", Workspace: f.workspace, Project: "p", Session: "eng", Args: map[string]any{"instance": instance, "issue_uuid": budgetIssue, "issue_identifier": "FIX-1"}})
	var subject, revision string
	if err := f.s.db.QueryRowContext(ctx, `SELECT id,revision FROM session_assignments WHERE project_id=$1 AND state='proposed'`, pid).Scan(&subject, &revision); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		exec(`INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,subject_revision,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at,kind) VALUES($1,$2,'proposal',$3,'r0','model','wait','uncertain','','permitted','wait',1,$4,'judgement')`, newID("dcs_"), pid, subject, layout(time.Now().Add(-time.Minute)))
	}
	open := func() map[string]any {
		t.Helper()
		out, err := f.s.ExecuteDispatcher(ctx, pid, DispatcherSessionName, Request{Operation: "dispatcher_case_open", Args: map[string]any{
			"kind": "decide", "subject_id": subject, "subject_revision": revision, "policy_revision": 1,
			"permitted": []any{"escalate"}, "issue": map[string]any{"id": budgetIssue}}})
		if err != nil {
			t.Fatal(err)
		}
		return out.Data.(map[string]any)
	}
	for range 2 {
		if out := open(); out["opened"] != false || out["reason"] != "over_budget" {
			t.Fatal(out)
		}
	}
	if n := count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE project_id=$1 AND kind='budget_skip'`, pid); n != 1 {
		t.Fatal("budget skips:", n)
	}
	if n := count(`SELECT COUNT(*) FROM dispatcher_escalations WHERE project_id=$1 AND reason_code='loop_budget'`, pid); n != 1 {
		t.Fatal("loop_budget escalations:", n)
	}
	due, err := f.s.ExecuteDispatcher(ctx, pid, DispatcherSessionName, Request{Operation: "dispatcher_cases_due"})
	if err != nil {
		t.Fatal(err)
	}
	cases := due.Data.(map[string]any)["cases"].([]any)
	if len(cases) != 1 || cases[0].(map[string]any)["over_budget"] != true || cases[0].(map[string]any)["budget_left"] != 0 {
		t.Fatal("due:", due.Data)
	}
	// A reset after the judgements opens it.
	exec(`INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,subject_revision,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at,kind) VALUES($1,$2,'issue',$3,'','rules','none','budget_reset','','','none',1,$4,'setting_change')`, newID("dcs_"), pid, budgetIssue, layout(time.Now()))
	if out := open(); out["opened"] != true {
		t.Fatal("after a reset:", out)
	}
	// The setting: a member is refused, the owner previews and confirms.
	member := newID("m") + "@identity-test.example"
	f.member(member)
	budget := func(principal, sub string, a map[string]any) (Result, error) {
		return f.as(principal, sub, Request{Operation: "dispatcher_model_budget", Workspace: f.workspace, Project: "p", Args: a})
	}
	if _, err = budget(member, "sub-member", map[string]any{"budget": 5, "expected_revision": 1, "confirm": true}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatal("a member set the budget:", err)
	}
	if out, err := budget(f.owner, "sub-owner", map[string]any{"budget": 5, "expected_revision": 1}); err != nil || out.Data.(map[string]any)["preview"] != true {
		t.Fatal(out, err)
	}
	if _, err = budget(f.owner, "sub-owner", map[string]any{"budget": 5, "expected_revision": 9, "confirm": true}); code(err) != "conflict" {
		t.Fatal("a stale revision:", err)
	}
	if _, err = budget(f.owner, "sub-owner", map[string]any{"budget": 5, "expected_revision": 1, "confirm": true}); err != nil {
		t.Fatal(err)
	}
	list := func(kind string) []any {
		t.Helper()
		out, err := f.as(f.owner, "sub-owner", Request{Operation: "dispatcher_decisions_list", Workspace: f.workspace, Project: "p", Args: map[string]any{"since_hours": 24, "limit": 50, "kind": kind}})
		if err != nil {
			t.Fatal(kind, err)
		}
		return out.Data.([]any)
	}
	settings := list("setting_change")
	if len(settings) != 2 {
		t.Fatal("setting changes:", settings)
	}
	for _, d := range settings {
		if m := d.(map[string]any); m["kind"] != "setting_change" {
			t.Fatal(m)
		}
	}
	if got := list("judgement"); len(got) != 3 {
		t.Fatal("judgements:", got)
	}
	if got := list("escalation"); len(got) != 1 || got[0].(map[string]any)["reason_code"] != "loop_budget" {
		t.Fatal("escalations:", got)
	}
	if _, err = f.as(f.owner, "sub-owner", Request{Operation: "dispatcher_decisions_list", Workspace: f.workspace, Project: "p", Args: map[string]any{"since_hours": 24, "limit": 50, "kind": "advice"}}); code(err) != "invalid" {
		t.Fatal("an unknown kind:", err)
	}
}

// Schema 54 on Postgres: old decisions get their kind from their engine,
// the escalation payload columns exist, and a case's attempt is in the
// subject index.
func TestPostgresLoopBudgetMigration(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := strings.ReplaceAll(newID("lb"), "-", "_")
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	owner := newID("owner") + "@identity-test.example"
	s, err := OpenPostgres(ctx, parsed.String(), owner)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err = admin.ExecContext(ctx, loopBudgetDowngradePostgres(schema)+`DELETE FROM `+schema+`.clawbridge_schema WHERE version>=54;
INSERT INTO `+schema+`.workspaces(id,name,mode,retention_days,owner_principal,created_at) VALUES('w','w','server',30,'o','2026-01-01T00:00:00Z');
INSERT INTO `+schema+`.projects(id,workspace_id,slug,name,created_at) VALUES('prj_1','w','p','p','2026-01-01T00:00:00Z');
INSERT INTO `+schema+`.dispatcher_decisions(id,project_id,subject_type,subject_id,engine,proposal,reason_code,guardrail,action,policy_revision,created_at) VALUES('d_m','prj_1','proposal','asg_1','model','wait','uncertain','permitted','wait',1,'2026-01-01T00:00:00Z'),('d_r','prj_1','proposal','asg_1','rules','wait','uncertain','permitted','wait',1,'2026-01-01T00:00:00Z');
INSERT INTO `+schema+`.dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,deadline,created_at) VALUES('case_old','prj_1','decide','proposal','asg_old','1','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),('chat_old','prj_1','chat','project','prj_1','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if s, err = OpenPostgres(ctx, parsed.String(), owner); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var version int
	if err = admin.QueryRowContext(ctx, `SELECT MAX(version) FROM `+schema+`.clawbridge_schema`).Scan(&version); err != nil || version != loopBudgetSchemaVersion {
		t.Fatal(version, err)
	}
	var model, rules string
	if err = admin.QueryRowContext(ctx, `SELECT (SELECT kind FROM `+schema+`.dispatcher_decisions WHERE id='d_m'),(SELECT kind FROM `+schema+`.dispatcher_decisions WHERE id='d_r')`).Scan(&model, &rules); err != nil || model != "judgement" || rules != "rules" {
		t.Fatal(model, rules, err)
	}
	if _, err = admin.ExecContext(ctx, `SELECT kind,question,options,suggested,timeout_at,answer,answered_by,answered_at FROM `+schema+`.dispatcher_escalations LIMIT 0`); err != nil {
		t.Fatal(err)
	}
	// Cases from before keep their rows at attempt 0, under the rebuilt
	// index.
	var old int
	if err = admin.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+schema+`.dispatcher_cases WHERE id IN ('case_old','chat_old') AND attempt=0`).Scan(&old); err != nil || old != 2 {
		t.Fatal("cases from before:", old, err)
	}
	if _, err = admin.ExecContext(ctx, `INSERT INTO `+schema+`.dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,attempt,deadline,created_at) VALUES('case_dup','prj_1','decide','proposal','asg_old','1',0,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("a second attempt 0 beside a case from before")
	}
	insert := `INSERT INTO ` + schema + `.dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,attempt,deadline,created_at) VALUES($1,'prj_1','decide','proposal','asg_1','1',$2,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`
	if _, err = admin.ExecContext(ctx, insert, "case_0", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.ExecContext(ctx, insert, "case_1", 1); err != nil {
		t.Fatal("a second attempt on the revision:", err)
	}
	if _, err = admin.ExecContext(ctx, insert, "case_2", 1); err == nil {
		t.Fatal("the same attempt twice")
	}
}
