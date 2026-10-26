package watcher

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

const (
	projPay   = "aaaa1111-1111-4111-8111-111111111111"
	projOther = "bbbb2222-2222-4222-8222-222222222222"
)

// dispatcherOn binds the project to projPay and projOther, sets one domain
// (payments: projPay, coordinated by coord, trusting eng), and turns the
// dispatcher on as the project's coordinator.
func (e *env) dispatcherOn(trusted ...string) {
	e.t.Helper()
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 1, "mode": "project_set", "linear_team_ids": []string{team}, "linear_default_team_id": team, "linear_project_ids": []string{projPay, projOther}}}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.ExecuteDispatcher(context.Background(), e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_get"}); err != nil {
		e.t.Fatal(err)
	}
	trust := []any{}
	for _, name := range trusted {
		trust = append(trust, map[string]any{"name": name})
	}
	owner := e.call("whoami", "coord", map[string]any{})["principal"].(string)
	e.call("dispatcher_domains_set", "coord", map[string]any{"expected_revision": 0, "dispatcher_domains": map[string]any{"domains": []any{
		map[string]any{"id": "payments", "name": "Payments", "match": map[string]any{"linear_projects": []any{projPay}}, "owner_principal": owner, "coordinator_session": map[string]any{"name": "coord"}, "trusted_sessions": trust},
	}}})
	e.sql(`UPDATE dispatcher_policies SET enabled=1`)
	e.sql(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='clawbridge-dispatcher')`)
}

func (e *env) propose(session, issue, project string) {
	e.t.Helper()
	e.call("assignment_propose", session, map[string]any{"issue_uuid": issue, "issue_identifier": "FIX-1", "issue_project_id": project})
}

func (e *env) assignmentState(session string) string {
	e.t.Helper()
	var state string
	e.queryRow(`SELECT COALESCE(MAX(a.state),'none') FROM session_assignments a JOIN sessions s ON s.id=a.session_id WHERE s.name='`+session+`' AND a.state<>'released'`, &state)
	return state
}

func appIssue(id, project, state string) map[string]any {
	return map[string]any{"id": id, "project": map[string]any{"id": project}, "labels": map[string]any{"nodes": []any{}}, "state": map[string]any{"type": state}}
}

// A trusted session's claim on an open issue in its domain's project, as the
// app reads it, is confirmed within the tick.
func TestDispatcherConfirmsATrustedClaim(t *testing.T) {
	e := setup(t)
	e.dispatcherOn("eng")
	e.f.dispatchIssues = map[string]any{issueA: appIssue(issueA, projPay, "unstarted")}
	e.propose("eng", issueA, projPay)
	tick := e.tick()
	if tick.Error != "" || tick.Dispatch == nil || tick.Dispatch.Actions["confirm"] != 1 || tick.Dispatch.Reads != 1 || e.assignmentState("eng") != "confirmed" {
		t.Fatal(tick.Dispatch, tick.Error, e.assignmentState("eng"))
	}
	var engine, source string
	e.queryRow(`SELECT engine,facts_source FROM dispatcher_decisions WHERE action='confirm'`, &engine, &source)
	if engine != "rules" || source != "app" {
		t.Fatal(engine, source)
	}
	// Decided once: the next tick has nothing to do.
	if again := e.tick(); again.Dispatch != nil {
		t.Fatal("decided twice", again.Dispatch)
	}
}

// What the client reports never grants: the app's read says the issue is in
// another project, so it escalates instead.
func TestDispatcherClientFactsNeverGrant(t *testing.T) {
	e := setup(t)
	e.dispatcherOn("eng")
	e.f.dispatchIssues = map[string]any{issueA: appIssue(issueA, projOther, "unstarted")}
	e.propose("eng", issueA, projPay)
	tick := e.tick()
	if tick.Dispatch.Actions["escalate"] != 1 || e.assignmentState("eng") != "proposed" {
		t.Fatal(tick.Dispatch, tick.Error, e.assignmentState("eng"))
	}
	var reason string
	e.queryRow(`SELECT reason_code FROM dispatcher_decisions`, &reason)
	if reason != "no_domain" {
		t.Fatal(reason)
	}
}

// An untrusted session's claim is escalated to the domain's coordinator,
// once, with no issue text, and never confirmed however long it waits.
func TestDispatcherEscalatesAndSilenceNeverGrants(t *testing.T) {
	e := setup(t)
	e.dispatcherOn()
	e.f.dispatchIssues = map[string]any{issueA: appIssue(issueA, projPay, "unstarted")}
	e.propose("eng", issueA, projPay)
	tick := e.tick()
	if tick.Dispatch.Actions["escalate"] != 1 || tick.Dispatch.Reads != 0 {
		t.Fatal("an untrusted claim spent a read or was not escalated", tick.Dispatch)
	}
	notes := e.inbox("coord", "dispatcher")
	if len(notes) != 1 || !strings.Contains(fmt.Sprint(notes[0]["body"]), "untrusted") || !strings.Contains(fmt.Sprint(notes[0]["body"]), "nothing was confirmed") {
		t.Fatal(notes)
	}
	// A day passes with nobody answering: it stays proposed.
	e.sql(`UPDATE session_assignments SET proposed_at='2000-01-01T00:00:00.000000000Z'`)
	for i := 0; i < 3; i++ {
		e.tick()
	}
	if e.assignmentState("eng") != "proposed" {
		t.Fatal("silence granted the claim")
	}
	var escalations int
	e.queryRow(`SELECT COUNT(*) FROM dispatcher_escalations`, &escalations)
	if escalations != 1 {
		t.Fatal("escalated more than once", escalations)
	}
}

// Two trusted claims on one issue in the same tick: the first is confirmed,
// the second sees the hold and is escalated as an overlap.
func TestDispatcherSameIssueInOneTick(t *testing.T) {
	e := setup(t)
	out := e.call("register_session", "eng2", map[string]any{"instance": "eng2-i"})
	e.insts["eng2"] = out["instance"].(string)
	e.dispatcherOn("eng", "eng2")
	e.f.dispatchIssues = map[string]any{issueA: appIssue(issueA, projPay, "started")}
	e.propose("eng", issueA, projPay)
	e.propose("eng2", issueA, projPay)
	tick := e.tick()
	if tick.Dispatch.Actions["confirm"] != 1 || tick.Dispatch.Actions["escalate"]+tick.Dispatch.Actions["superseded"] != 1 {
		t.Fatal(tick.Dispatch)
	}
	if (e.assignmentState("eng") == "confirmed") == (e.assignmentState("eng2") == "confirmed") {
		t.Fatal("both or neither confirmed", e.assignmentState("eng"), e.assignmentState("eng2"))
	}
}

// Off by default: nothing is decided until the owner enables it.
func TestDispatcherOffDecidesNothing(t *testing.T) {
	e := setup(t)
	e.dispatcherOn("eng")
	e.sql(`UPDATE dispatcher_policies SET enabled=0`)
	e.propose("eng", issueA, projPay)
	if tick := e.tick(); tick.Dispatch != nil || e.assignmentState("eng") != "proposed" {
		t.Fatal(tick.Dispatch)
	}
}

// The bus's own layers, sent directly: no case while off; a decision
// recorded after the dispatcher was turned off is superseded; and the
// dispatcher can't confirm unless it is the project's coordinator.
func TestDispatcherBusLayers(t *testing.T) {
	e := setup(t)
	e.dispatcherOn("eng")
	e.propose("eng", issueA, projPay)
	due := func() map[string]any {
		r, err := e.s.ExecuteDispatcher(context.Background(), e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_cases_due"})
		if err != nil {
			t.Fatal(err)
		}
		return r.Data.(map[string]any)
	}
	c := due()["cases"].([]any)[0].(map[string]any)
	record := func(action string) (map[string]any, error) {
		r, err := e.s.ExecuteDispatcher(context.Background(), e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_decision_record", Args: map[string]any{
			"subject_id": c["subject_id"], "subject_revision": c["subject_revision"], "policy_revision": 1, "engine": "rules", "proposal": action, "reason_code": "trusted", "action": action, "domain": "payments"}})
		if err != nil {
			return nil, err
		}
		return r.Data.(map[string]any), nil
	}
	e.sql(`UPDATE dispatcher_policies SET enabled=0`)
	if cases := due()["cases"].([]any); len(cases) != 0 {
		t.Fatal("cases while off", cases)
	}
	if out, err := record("confirm"); err != nil || out["action"] != "superseded" || e.assignmentState("eng") != "proposed" {
		t.Fatal("a decision recorded while off acted", out, err)
	}
	e.sql(`UPDATE dispatcher_policies SET enabled=1`)
	e.sql(`DELETE FROM dispatcher_decisions`)
	e.sql(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='coord')`)
	if _, err := record("confirm"); err == nil || e.assignmentState("eng") != "proposed" {
		t.Fatal("the dispatcher confirmed without being the coordinator", err)
	}
}
