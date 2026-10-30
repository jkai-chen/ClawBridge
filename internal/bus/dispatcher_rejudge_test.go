package bus

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// rejudge runs dispatcher_model_rejudge as principal holding role, as the
// hosted authority would resolve it.
func (c *chatHarness) rejudge(principal, role string, a map[string]any) (map[string]any, error) {
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
	out, err := scoped.dispatcherAdviseOp(c.ctx, t, w, p, role, Request{Operation: "dispatcher_model_rejudge", Args: a})
	if err == nil {
		err = tx.Commit()
	}
	m, _ := out.(map[string]any)
	return m, err
}

// dueCase is subject's entry in dispatcher_cases_due, or nil when it isn't
// due.
func (c *chatHarness) dueCase(subject string) map[string]any {
	c.t.Helper()
	out, err := c.s.ExecuteDispatcher(c.ctx, c.pid, DispatcherSessionName, Request{Operation: "dispatcher_cases_due"})
	if err != nil {
		c.t.Fatal(err)
	}
	for _, d := range out.Data.(map[string]any)["cases"].([]any) {
		if d.(map[string]any)["subject_id"] == subject {
			return d.(map[string]any)
		}
	}
	return nil
}

// judgeOpenCase has the model agent lease the open case and decide it.
func (c *chatHarness) judgeOpenCase(caseID, action string) {
	c.t.Helper()
	next, err := c.s.ModelNextCase(c.ctx, c.token, 0)
	if err != nil || next.Case == nil || next.Case["case_id"] != caseID {
		c.t.Fatal("the model's next case:", next, err)
	}
	out, err := c.s.ModelTool(c.ctx, c.token, caseID, "decide", map[string]any{"action": action, "reason_code": "uncertain", "explanation": "unsure"})
	if err != nil || out.Error != "" {
		c.t.Fatal("decide:", out, err)
	}
}

func (c *chatHarness) caseAt(subject, revision string, attempt int) string {
	c.t.Helper()
	var id string
	if err := c.s.db.QueryRow(`SELECT id FROM dispatcher_cases WHERE kind='decide' AND subject_id=? AND subject_revision=? AND attempt=?`, subject, revision, attempt).Scan(&id); err != nil {
		c.t.Fatal("the case at attempt", attempt, err)
	}
	return id
}

// R5: at budget 3, three judgements put the issue over; a person's
// re-judge resets it and allows one more case on the same revision, at
// attempt 1; a repeat open is case_exists; once it closes there is no
// other until the next re-judge, which is attempt 2. The log names each
// judgement's own case.
func TestRejudge(t *testing.T) {
	c := newChatHarness(t)
	subject, revision := c.propose(budgetIssue)
	c.judged(subject, 3, time.Minute)
	if d := c.dueCase(subject); d == nil || d["over_budget"] != true || d["model_case"] != "" {
		t.Fatal("three of three used:", d)
	}
	if out := c.openDecide(subject, revision); out["reason"] != "over_budget" {
		t.Fatal(out)
	}
	got, err := c.rejudge("lead@example.test", "lead", map[string]any{"proposal_id": subject, "revision": revision})
	if err != nil || got["rejudge"] != true || got["proposal_id"] != subject || got["revision"] != revision || got["attempt"] != 1 || !strings.Contains(str(got, "note"), "next tick") {
		t.Fatal("a lead's re-judge:", got, err)
	}
	var subjectType, subjectID, subjectRevision, engine, explanation, action string
	if err = c.s.db.QueryRow(`SELECT subject_type,subject_id,subject_revision,engine,explanation,action FROM dispatcher_decisions WHERE kind='setting_change' AND reason_code='budget_reset'`).Scan(&subjectType, &subjectID, &subjectRevision, &engine, &explanation, &action); err != nil {
		t.Fatal(err)
	}
	if subjectType != "issue" || subjectID != budgetIssue || subjectRevision != "" || engine != "rules" || explanation != "by:lead@example.test" || action != "none" {
		t.Fatal("the reset:", subjectType, subjectID, subjectRevision, engine, explanation, action)
	}
	if err = c.s.db.QueryRow(`SELECT subject_type,subject_id,subject_revision,explanation FROM dispatcher_decisions WHERE kind='setting_change' AND reason_code='rejudge'`).Scan(&subjectType, &subjectID, &subjectRevision, &explanation); err != nil {
		t.Fatal(err)
	}
	if subjectType != "proposal" || subjectID != subject || subjectRevision != revision || explanation != "by:lead@example.test" {
		t.Fatal("the re-judge:", subjectType, subjectID, subjectRevision, explanation)
	}
	if n := c.count(`SELECT COUNT(*) FROM history WHERE action='dispatcher_model_rejudge' AND entity_key=?`, subject); n != 1 {
		t.Fatal("audit:", n)
	}
	if d := c.dueCase(subject); d == nil || d["over_budget"] != false || d["budget_left"] != 3 || d["model_case"] != "" {
		t.Fatal("after the re-judge:", d)
	}
	// One re-judge, one case: a second before the model had it is refused.
	if _, err = c.rejudge("", "owner", map[string]any{"proposal_id": subject, "revision": revision}); code(err) != "conflict" || !strings.Contains(err.Error(), "waits for the model's case (attempt 1)") {
		t.Fatal("a second re-judge before the case:", err)
	}
	opened := c.openDecide(subject, revision)
	if opened["opened"] != true {
		t.Fatal("attempt 1:", opened)
	}
	first := c.caseAt(subject, revision, 1)
	if first != opened["case_id"] {
		t.Fatal("attempt 1 is", first, "not", opened["case_id"])
	}
	if out := c.openDecide(subject, revision); out["opened"] != false || out["reason"] != "case_exists" {
		t.Fatal("a second open at attempt 1:", out)
	}
	if d := c.dueCase(subject); d["model_case"] != "open" {
		t.Fatal("the open case:", d)
	}
	if _, err = c.rejudge("", "owner", map[string]any{"proposal_id": subject, "revision": revision}); code(err) != "conflict" || !strings.Contains(err.Error(), first) {
		t.Fatal("a re-judge while the model judges:", err)
	}
	c.judgeOpenCase(first, "escalate")
	// Decided: no more cases until another re-judge.
	if d := c.dueCase(subject); d != nil {
		t.Fatal("decided, yet due:", d)
	}
	if out := c.openDecide(subject, revision); out["reason"] != "case_exists" {
		t.Fatal("a case after the attempt closed:", out)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_cases WHERE kind='decide'`); n != 1 {
		t.Fatal("cases:", n)
	}
	// The second re-judge: attempt 2, the claim due again though the
	// model escalated it before.
	if got, err = c.rejudge("", "owner", map[string]any{"proposal_id": subject, "revision": revision}); err != nil || got["attempt"] != 2 {
		t.Fatal("the second re-judge:", got, err)
	}
	if d := c.dueCase(subject); d == nil || d["model_case"] != "" || d["over_budget"] != false || d["budget_left"] != 3 {
		t.Fatal("after the second re-judge:", d)
	}
	out, err := c.s.ExecuteDispatcher(c.ctx, c.pid, DispatcherSessionName, Request{Operation: "dispatcher_case_open", Args: map[string]any{
		"kind": "decide", "subject_id": subject, "subject_revision": revision, "policy_revision": 1,
		"permitted": []any{"wait", "escalate"}, "issue": map[string]any{"id": budgetIssue}}})
	if err != nil || out.Data.(map[string]any)["opened"] != true {
		t.Fatal("attempt 2:", out, err)
	}
	second := c.caseAt(subject, revision, 2)
	if d := c.dueCase(subject); d["model_case"] != "open" {
		t.Fatal("attempt 2's case:", d)
	}
	c.judgeOpenCase(second, "wait")
	// A model wait isn't final: the claim is due, and its case at this
	// attempt is closed, so the rules take it.
	if d := c.dueCase(subject); d == nil || d["model_case"] != "closed:model_wait" || d["budget_left"] != 2 {
		t.Fatal("after attempt 2:", d)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE kind='setting_change' AND reason_code='rejudge' AND subject_id=?`, subject); n != 2 {
		t.Fatal("re-judges:", n)
	}
	// The log names each judgement's own case.
	listed, err := c.s.Execute(c.ctx, Request{Operation: "dispatcher_decisions_list", Workspace: "w", Project: "p", Args: map[string]any{"since_hours": 24, "limit": 50, "kind": "judgement"}})
	if err != nil {
		t.Fatal(err)
	}
	rows := listed.Data.([]any)
	cases := map[string]string{}
	for _, r := range rows {
		m := r.(map[string]any)
		if id, _ := m["case_id"].(string); id != "" {
			cases[m["action"].(string)] = id
		}
	}
	if cases["escalate"] != first || cases["wait"] != second {
		t.Fatal("the log's cases:", cases, first, second)
	}
}

// R5: who may judge again, and when. A session, an instance, the
// dispatcher's own session and a member are refused; so is the dispatcher
// off or in rules mode, a claim missing, moved on or without an issue.
// Refusals record nothing, and nothing a session can call resets a budget.
func TestRejudgeRefusals(t *testing.T) {
	c := newChatHarness(t)
	subject, revision := c.propose(budgetIssue)
	args := map[string]any{"proposal_id": subject, "revision": revision}
	if _, err := c.s.Execute(c.ctx, c.request("dispatcher_model_rejudge", "a", args)); code(err) != "forbidden" || !strings.Contains(err.Error(), "never ask the model to judge again") {
		t.Fatal("a session:", err)
	}
	if _, err := c.as("", "dispatcher_model_rejudge", map[string]any{"proposal_id": subject, "revision": revision, "instance": "a-i"}); err == nil {
		t.Fatal("an instance re-judged")
	}
	for _, name := range []string{DispatcherSessionName, DispatcherModelSessionName} {
		if _, err := c.s.ExecuteDispatcher(c.ctx, c.pid, name, Request{Operation: "dispatcher_model_rejudge", Args: args}); err == nil {
			t.Fatal(name, "re-judged")
		}
	}
	if _, err := c.rejudge("member@example.test", "member", args); err == nil || !strings.Contains(err.Error(), "owner or lead") {
		t.Fatal("a member:", err)
	}
	for _, bad := range []map[string]any{{"proposal_id": subject}, {"revision": revision}} {
		if _, err := c.rejudge("", "owner", bad); code(err) != "invalid" {
			t.Fatal(bad, err)
		}
	}
	if _, err := c.rejudge("", "owner", map[string]any{"proposal_id": "asg_missing", "revision": revision}); code(err) != "not_found" {
		t.Fatal("a missing claim:", err)
	}
	if _, err := c.rejudge("", "owner", map[string]any{"proposal_id": subject, "revision": "rev_other"}); code(err) != "conflict" || !strings.Contains(err.Error(), "moved on") {
		t.Fatal("another revision:", err)
	}
	c.exec(`UPDATE dispatcher_policies SET mode='rules'`)
	if _, err := c.rejudge("", "owner", args); code(err) != "invalid" || !strings.Contains(err.Error(), "model engine") {
		t.Fatal("rules mode:", err)
	}
	c.exec(`UPDATE dispatcher_policies SET mode='model',enabled=0`)
	if _, err := c.rejudge("", "owner", args); code(err) != "invalid" || !strings.Contains(err.Error(), "model engine") {
		t.Fatal("dispatcher off:", err)
	}
	c.exec(`UPDATE dispatcher_policies SET enabled=1`)
	c.exec(`UPDATE session_assignments SET issue_uuid='' WHERE id=?`, subject)
	if _, err := c.rejudge("", "owner", args); code(err) != "invalid" || !strings.Contains(err.Error(), "no issue") {
		t.Fatal("no issue:", err)
	}
	c.exec(`UPDATE session_assignments SET issue_uuid=? WHERE id=?`, budgetIssue, subject)
	c.exec(`UPDATE session_assignments SET state='confirmed' WHERE id=?`, subject)
	if _, err := c.rejudge("", "owner", args); code(err) != "conflict" || !strings.Contains(err.Error(), "confirmed") {
		t.Fatal("not pending:", err)
	}
	c.exec(`UPDATE session_assignments SET state='proposed' WHERE id=?`, subject)
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_decisions`); n != 0 {
		t.Fatal("a refusal recorded", n)
	}
	if n := c.count(`SELECT COUNT(*) FROM history WHERE action='dispatcher_model_rejudge'`); n != 0 {
		t.Fatal("a refusal audited", n)
	}
	// Over budget, the dispatcher session records a decision whose reason
	// reads budget_reset: it is a rules decision on the claim, not a reset.
	c.judged(subject, 3, time.Minute)
	if _, err := c.s.ExecuteDispatcher(c.ctx, c.pid, DispatcherSessionName, Request{Operation: "dispatcher_decision_record", Args: map[string]any{
		"subject_id": subject, "subject_revision": revision, "policy_revision": 1, "engine": "rules",
		"proposal": "wait", "reason_code": "budget_reset", "guardrail": "permitted", "action": "wait", "facts_source": "client",
	}}); err != nil {
		t.Fatal(err)
	}
	if d := c.dueCase(subject); d == nil || d["over_budget"] != true {
		t.Fatal("a session's decision reset the budget:", d)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE kind='setting_change'`); n != 0 {
		t.Fatal("a session wrote a setting change:", n)
	}
	// A rejudge after the rules decided it, before the model had the
	// case, is allowed: that re-judge's case can no longer open. The
	// waits the rules count start again from the re-judge.
	if d := c.dueCase(subject); d["waits"] != 1 {
		t.Fatal("waits before the re-judge:", d)
	}
	if _, err := c.rejudge("", "owner", args); err != nil {
		t.Fatal(err)
	}
	if d := c.dueCase(subject); d == nil || d["waits"] != 0 {
		t.Fatal("waits after the re-judge:", d)
	}
	c.escalate(subject, revision, "escalate", "ambiguous")
	if got, err := c.rejudge("", "owner", args); err != nil || got["attempt"] != 2 {
		t.Fatal("a re-judge after the rules decided:", got, err)
	}
}

// I1 (D5-L3): the people behind a re-judge and an answer reach the model
// only as opaque refs. The model's list_decisions holds no address; the
// person's own decision list still names who re-judged.
func TestRejudgeNamesNoOneToTheModel(t *testing.T) {
	c := newChatHarness(t)
	subject, revision := c.propose(escalationIssue)
	if _, err := c.rejudge("lead@example.test", "lead", map[string]any{"proposal_id": subject, "revision": revision}); err != nil {
		t.Fatal(err)
	}
	e := c.escalate(subject, revision, "escalate", "ambiguous")
	if _, err := c.answer("lead@example.test", "lead", map[string]any{"escalation_id": e["id"], "option": "wait"}); err != nil {
		t.Fatal(err)
	}
	asked, err := c.as("", "dispatcher_chat_ask", map[string]any{"text": "Who re-judged FIX-2?"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := c.s.ModelNextCase(c.ctx, c.token, 0)
	if err != nil || next.Case == nil || next.Case["case_id"] != asked["case_id"] {
		t.Fatal("the chat case:", next, err)
	}
	out, err := c.s.ModelTool(c.ctx, c.token, asked["case_id"].(string), "list_decisions", map[string]any{"since_hours": 24, "limit": 50})
	if err != nil || out.Error != "" {
		t.Fatal(out, err)
	}
	raw, _ := json.Marshal(out.Result)
	if strings.Contains(string(raw), "@") || !strings.Contains(string(raw), `"explanation":"by:p_`) || !strings.Contains(string(raw), `"answer:wait"`) {
		t.Fatal("the model's decision list:", string(raw))
	}
	var ref string
	if err = c.s.db.QueryRow(`SELECT ref FROM dispatcher_principal_refs WHERE principal='lead@example.test'`).Scan(&ref); err != nil || !strings.Contains(string(raw), `"by:`+ref+`"`) {
		t.Fatal("the lead's ref:", ref, err)
	}
	listed, err := c.s.Execute(c.ctx, Request{Operation: "dispatcher_decisions_list", Workspace: "w", Project: "p", Args: map[string]any{"since_hours": 24, "limit": 50, "kind": "setting_change"}})
	if err != nil {
		t.Fatal(err)
	}
	if person, _ := json.Marshal(listed.Data); !strings.Contains(string(person), "by:lead@example.test") {
		t.Fatal("the person's list:", string(person))
	}
}
