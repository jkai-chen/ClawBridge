package bus

import (
	"context"
	"strings"
	"testing"
)

// R4 on Postgres: a rules escalation carries its payload; a session and a
// member can't answer; a lead answers once, the same answer is idempotent
// and another a conflict; the claim is untouched; the list shows the
// payload and the answer.
func TestPostgresEscalationAnswer(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	var pid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES($1,1,1,'rules','{"domains":[]}','2026-01-01T00:00:00Z','x')`, pid)
	registered := f.must(f.owner, "sub-owner", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "eng", Args: map[string]any{"instance": "eng-i"}})
	instance := registered.Data.(map[string]any)["instance"].(string)
	f.must(f.owner, "sub-owner", Request{Operation: "assignment_propose", Workspace: f.workspace, Project: "p", Session: "eng", Args: map[string]any{"instance": instance, "issue_uuid": budgetIssue, "issue_identifier": "FIX-1"}})
	var subject, revision string
	if err := f.s.db.QueryRowContext(ctx, `SELECT id,revision FROM session_assignments WHERE project_id=$1 AND state='proposed'`, pid).Scan(&subject, &revision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ExecuteDispatcher(ctx, pid, DispatcherSessionName, Request{Operation: "dispatcher_decision_record", Args: map[string]any{
		"subject_id": subject, "subject_revision": revision, "policy_revision": 1, "engine": "rules",
		"proposal": "confirm", "reason_code": "guardrail:facts_not_authoritative", "guardrail": "overridden", "action": "escalate", "facts_source": "client",
	}}); err != nil {
		t.Fatal(err)
	}
	var id, kind, question, options, suggested, timeout string
	if err := f.s.db.QueryRowContext(ctx, `SELECT id,kind,question,options,suggested,timeout_at FROM dispatcher_escalations WHERE project_id=$1`, pid).Scan(&id, &kind, &question, &options, &suggested, &timeout); err != nil {
		t.Fatal(err)
	}
	if kind != "approval" || question != "Should eng's claim of FIX-1 be confirmed?" || options != `[{"id":"confirm","label":"Confirm the claim"},{"id":"release","label":"Release the claim"}]` || suggested != "confirm" || timeout == "" {
		t.Fatal(kind, question, options, suggested, timeout)
	}
	answer := func(principal, sub, session string, a map[string]any) (map[string]any, error) {
		r := Request{Operation: "dispatcher_escalation_answer", Workspace: f.workspace, Project: "p", Session: session, Args: a}
		out, err := f.as(principal, sub, r)
		m, _ := out.Data.(map[string]any)
		return m, err
	}
	if _, err := answer(f.owner, "sub-owner", "eng", map[string]any{"instance": instance, "escalation_id": id, "option": "confirm"}); code(err) != "forbidden" || !strings.Contains(err.Error(), "a session can never answer an escalation") {
		t.Fatal("a session answered:", err)
	}
	member := newID("m") + "@identity-test.example"
	f.member(member)
	if _, err := answer(member, "sub-member", "", map[string]any{"escalation_id": id, "option": "confirm"}); err == nil || !strings.Contains(err.Error(), "owner or lead") {
		t.Fatal("a member answered:", err)
	}
	if _, err := answer(f.owner, "sub-owner", "", map[string]any{"escalation_id": id, "option": "wait"}); code(err) != "invalid" {
		t.Fatal("an option not offered:", err)
	}
	lead := newID("l") + "@identity-test.example"
	f.role(lead, "lead")
	out, err := answer(lead, "sub-lead", "", map[string]any{"escalation_id": id, "option": "release"})
	if err != nil || out["answer"] != "release" || out["answered_by"] != lead || out["note"] != "recorded; to act on it, run clawbridge assignment release eng --revision "+revision {
		t.Fatal(out, err)
	}
	if again, err := answer(f.owner, "sub-owner", "", map[string]any{"escalation_id": id, "option": "release"}); err != nil || again["answered_by"] != lead || again["answered_at"] != out["answered_at"] {
		t.Fatal("the same answer:", again, err)
	}
	if _, err = answer(f.owner, "sub-owner", "", map[string]any{"escalation_id": id, "option": "confirm"}); code(err) != "conflict" {
		t.Fatal("a different answer:", err)
	}
	var state string
	if err = f.s.db.QueryRowContext(ctx, `SELECT state FROM session_assignments WHERE id=$1`, subject).Scan(&state); err != nil || state != "proposed" {
		t.Fatal("the claim changed:", state, err)
	}
	var decisions int
	if err = f.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dispatcher_decisions WHERE project_id=$1 AND kind='escalation' AND reason_code='answered' AND explanation='answer:release'`, pid).Scan(&decisions); err != nil || decisions != 1 {
		t.Fatal("the answer's decision:", decisions, err)
	}
	if n := f.audited("dispatcher_escalation_answer", id); n != 1 {
		t.Fatal("audit:", n)
	}
	listed, err := f.as(f.owner, "sub-owner", Request{Operation: "dispatcher_escalations_list", Workspace: f.workspace, Project: "p", Args: map[string]any{"all": true}})
	if err != nil {
		t.Fatal(err)
	}
	items := listed.Data.(map[string]any)["escalations"].([]any)
	if len(items) != 1 {
		t.Fatal(items)
	}
	item := items[0].(map[string]any)
	if item["kind"] != "approval" || item["suggested"] != "confirm" || item["answer"] != "release" || item["answered_by"] != lead || item["state"] != "resolved" || item["timeout_at"] == nil || len(item["options"].([]EscalationOption)) != 2 {
		t.Fatal(item)
	}
}
