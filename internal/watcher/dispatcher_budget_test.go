package watcher

import (
	"context"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// judged seeds n model judgements on the session's proposal, at an earlier
// revision, a minute ago.
func (e *env) judged(session string, n int) {
	e.t.Helper()
	at := time.Now().UTC().Add(-time.Minute).Format("2006-01-02T15:04:05.000000000Z07:00")
	for i := range n {
		e.sql(`INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,subject_revision,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at,kind) SELECT ?,a.project_id,'proposal',a.id,'r0','model','wait','uncertain','','permitted','wait',1,?,'judgement' FROM session_assignments a JOIN sessions s ON s.id=a.session_id WHERE s.name=?`, "dcs_seed"+string(rune('a'+i)), at, session)
	}
}

func (e *env) count(q string) int {
	e.t.Helper()
	var n int
	e.queryRow(q, &n)
	return n
}

// R1: at the default budget of 3, a claim on an issue the model judged
// three times gets no case. The rules decide it with
// model_fallback:loop_budget; the bus records one budget_skip for the
// revision and tells a person once, however many ticks it takes.
func TestModelLoopBudgetFallsToRules(t *testing.T) {
	e := setup(t)
	e.modelOn("eng")
	e.propose("eng", issueA, projPay)
	e.judged("eng", 3)
	// No authoritative read: the rules wait, tick after tick.
	for round := range 2 {
		tick := e.tick()
		if tick.Dispatch == nil || tick.Dispatch.Opened != 0 || tick.Dispatch.Actions["wait"] != 1 {
			t.Fatal(round, "not the rules:", tick.Dispatch, tick.Error)
		}
		if engine, action, explanation := e.decision(); engine != "rules" || action != "wait" || explanation != "model_fallback:loop_budget" {
			t.Fatal(round, engine, action, explanation)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM dispatcher_cases`); n != 0 {
		t.Fatal("a case past the budget:", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE kind='budget_skip' AND reason_code='loop_budget'`); n != 1 {
		t.Fatal("budget skips:", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM dispatcher_escalations WHERE reason_code='loop_budget'`); n != 1 {
		t.Fatal("loop_budget escalations:", n)
	}
	if len(e.inbox("coord", "dispatcher")) != 1 {
		t.Fatal("the person wasn't told once")
	}
	// With the read, the rules confirm; the record still says why.
	e.f.dispatchIssues = map[string]any{issueA: appIssue(issueA, projPay, "unstarted")}
	if tick := e.tick(); tick.Dispatch == nil || tick.Dispatch.Actions["confirm"] != 1 || e.assignmentState("eng") != "confirmed" {
		t.Fatal(tick.Dispatch, tick.Error)
	}
	var explanation string
	e.queryRow(`SELECT explanation FROM dispatcher_decisions WHERE kind='rules' AND action='confirm'`, &explanation)
	if explanation != "model_fallback:loop_budget" {
		t.Fatal(explanation)
	}
	if n := e.count(`SELECT COUNT(*) FROM dispatcher_escalations WHERE reason_code='loop_budget'`); n != 1 {
		t.Fatal("loop_budget escalations:", n)
	}
}

// Budget 0 is no cap: the same claim gets its case.
func TestModelLoopBudgetZeroIsNoCap(t *testing.T) {
	e := setup(t)
	e.modelOn("eng")
	e.sql(`UPDATE dispatcher_policies SET model='{"model_judgement_budget":0}'`)
	e.f.dispatchIssues = map[string]any{issueA: appIssue(issueA, projPay, "unstarted")}
	e.propose("eng", issueA, projPay)
	e.judged("eng", 5)
	if tick := e.tick(); tick.Dispatch == nil || tick.Dispatch.Opened != 1 {
		t.Fatal(tick.Dispatch, tick.Error)
	}
}

// R5: past the budget the rules escalate; a person's re-judge resets the
// issue's budget and makes the claim due again, and the next tick opens
// the model's case for the same revision at attempt 1. The tick after that waits on the case and opens
// no other.
func TestModelRejudgeOpensAnotherCase(t *testing.T) {
	e := setup(t)
	e.modelOn("eng")
	e.f.dispatchIssues = map[string]any{issueA: appIssue(issueA, projPay, "unstarted")}
	e.propose("eng", issueA, projPay)
	e.judged("eng", 3)
	e.sql(`UPDATE dispatcher_policies SET domains='{"domains":[]}'`)
	// Over budget, with no domain the rules escalate: a final decision,
	// and the claim stays pending.
	if tick := e.tick(); tick.Dispatch == nil || tick.Dispatch.Opened != 0 {
		t.Fatal("over budget:", tick.Dispatch, tick.Error)
	}
	if engine, action, explanation := e.decision(); engine != "rules" || action != "escalate" || explanation != "model_fallback:loop_budget" {
		t.Fatal(engine, action, explanation)
	}
	if e.assignmentState("eng") != "proposed" {
		t.Fatal("the rules decided the claim:", e.assignmentState("eng"))
	}
	var subject, revision string
	e.queryRow(`SELECT a.id,a.revision FROM session_assignments a JOIN sessions s ON s.id=a.session_id WHERE s.name='eng' AND a.state='proposed'`, &subject, &revision)
	r, err := e.s.Execute(context.Background(), bus.Request{Operation: "dispatcher_model_rejudge", Workspace: "w", Project: "p", Args: map[string]any{"proposal_id": subject, "revision": revision}})
	if err != nil || r.Data.(map[string]any)["attempt"] != 1 {
		t.Fatal(r.Data, err)
	}
	if tick := e.tick(); tick.Dispatch == nil || tick.Dispatch.Opened != 1 || len(tick.Dispatch.Actions) != 0 {
		t.Fatal("after the re-judge:", tick.Dispatch, tick.Error)
	}
	if n := e.count(`SELECT COUNT(*) FROM dispatcher_cases WHERE kind='decide' AND attempt=1 AND subject_revision='` + revision + `'`); n != 1 {
		t.Fatal("attempt 1:", n)
	}
	if tick := e.tick(); tick.Dispatch == nil || tick.Dispatch.Opened != 0 || tick.Dispatch.Waiting != 1 {
		t.Fatal("the next tick:", tick.Dispatch, tick.Error)
	}
	if n := e.count(`SELECT COUNT(*) FROM dispatcher_cases`); n != 1 {
		t.Fatal("cases:", n)
	}
}

// R5 with R1: the tick read the claim over budget, and a person re-judged
// before its rules decision was recorded. The bus's own count is no longer
// over, so the decision is superseded (budget_reset), nothing is done, and
// the next tick opens the model's case at attempt 1.
func TestModelRejudgeBeforeTheRecord(t *testing.T) {
	e := setup(t)
	e.modelOn("eng")
	e.f.dispatchIssues = map[string]any{issueA: appIssue(issueA, projPay, "unstarted")}
	e.propose("eng", issueA, projPay)
	e.judged("eng", 3)
	var subject, revision string
	e.queryRow(`SELECT a.id,a.revision FROM session_assignments a JOIN sessions s ON s.id=a.session_id WHERE s.name='eng' AND a.state='proposed'`, &subject, &revision)
	ctx := context.Background()
	due, err := e.s.ExecuteDispatcher(ctx, e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_cases_due"})
	if err != nil || due.Data.(map[string]any)["cases"].([]any)[0].(map[string]any)["over_budget"] != true {
		t.Fatal("the due read:", due.Data, err)
	}
	if _, err = e.s.Execute(ctx, bus.Request{Operation: "dispatcher_model_rejudge", Workspace: "w", Project: "p", Args: map[string]any{"proposal_id": subject, "revision": revision}}); err != nil {
		t.Fatal(err)
	}
	// The rules' escalate, recorded as the tick would after its read.
	out, err := e.s.ExecuteDispatcher(ctx, e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_decision_record", Args: map[string]any{
		"subject_id": subject, "subject_revision": revision, "policy_revision": 1, "engine": "rules",
		"proposal": "escalate", "reason_code": "no_domain", "guardrail": "permitted", "action": "escalate", "facts_source": "client",
		"explanation": "model_fallback:loop_budget", "over_budget": true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if m := out.Data.(map[string]any); m["action"] != "superseded" || m["reason_code"] != "budget_reset" || m["over_budget"] != false {
		t.Fatal("the stale record:", m)
	}
	if n := e.count(`SELECT COUNT(*) FROM dispatcher_escalations`); n != 0 {
		t.Fatal("a superseded record escalated:", n)
	}
	if tick := e.tick(); tick.Dispatch == nil || tick.Dispatch.Opened != 1 {
		t.Fatal("the next tick:", tick.Dispatch, tick.Error)
	}
	if n := e.count(`SELECT COUNT(*) FROM dispatcher_cases WHERE kind='decide' AND attempt=1 AND subject_revision='` + revision + `'`); n != 1 {
		t.Fatal("attempt 1:", n)
	}
}
