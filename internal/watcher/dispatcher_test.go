package watcher

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

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
	trust := []any{}
	for _, name := range trusted {
		trust = append(trust, map[string]any{"name": name})
	}
	owner := e.call("whoami", "coord", map[string]any{})["principal"].(string)
	e.call("dispatcher_domains_set", "coord", map[string]any{"expected_revision": 0, "dispatcher_domains": map[string]any{"domains": []any{
		map[string]any{"id": "payments", "name": "Payments", "match": map[string]any{"linear_projects": []any{projPay}}, "owner_principal": owner, "coordinator_session": map[string]any{"name": "coord"}, "trusted_sessions": trust},
	}}})
	e.sql(`UPDATE dispatcher_policies SET enabled=1`)
	// Enabled, the dispatcher's sessions exist from its first call.
	if _, err := e.s.ExecuteDispatcher(context.Background(), e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_get"}); err != nil {
		e.t.Fatal(err)
	}
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
			"subject_id": c["subject_id"], "subject_revision": c["subject_revision"], "policy_revision": 1, "engine": "rules", "proposal": action, "reason_code": "trusted", "action": action, "domain": "payments", "facts_source": "app", "app_project_id": projPay}})
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

// The bus re-checks a grant itself, whatever engine sent it (D3-M1): a
// confirm without the app's read, on a project outside the domain, or for a
// session the domain doesn't trust, and a decline of a session that isn't
// gone, are all superseded and change nothing.
func TestDispatcherBusRechecksGrants(t *testing.T) {
	e := setup(t)
	e.dispatcherOn("eng")
	e.propose("eng", issueA, projPay)
	r, err := e.s.ExecuteDispatcher(context.Background(), e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_cases_due"})
	if err != nil {
		t.Fatal(err)
	}
	c := r.Data.(map[string]any)["cases"].([]any)[0].(map[string]any)
	send := func(action, source, project string) string {
		out, err := e.s.ExecuteDispatcher(context.Background(), e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_decision_record", Args: map[string]any{
			"subject_id": c["subject_id"], "subject_revision": c["subject_revision"], "policy_revision": 1, "engine": "model", "proposal": action, "reason_code": "x", "action": action, "facts_source": source, "app_project_id": project}})
		if err != nil {
			t.Fatal(err)
		}
		m := out.Data.(map[string]any)
		return fmt.Sprint(m["action"], ":", m["reason_code"])
	}
	for name, got := range map[string]string{
		"client facts":           send("confirm", "client", projPay),
		"no app project":         send("confirm", "app", ""),
		"other project":          send("confirm", "app", projOther),
		"decline a live session": send("decline", "app", ""),
	} {
		if got != "superseded:guardrail" || e.assignmentState("eng") != "proposed" {
			t.Fatal(name, got, e.assignmentState("eng"))
		}
	}
	// The trusted entry's recorded principal no longer matches the session.
	var principal string
	e.queryRow(`SELECT owner_principal FROM sessions WHERE name='eng'`, &principal)
	e.sql(`UPDATE dispatcher_policies SET domains=REPLACE(domains,'"principal":"` + principal + `"','"principal":"someone@example.com"')`)
	if got := send("confirm", "app", projPay); got != "superseded:guardrail" {
		t.Fatal("a session trusted under another principal was confirmed:", got)
	}
	e.sql(`UPDATE dispatcher_policies SET domains=REPLACE(domains,'"principal":"someone@example.com"','"principal":"` + principal + `"')`)
	e.sql(`UPDATE dispatcher_policies SET domains=REPLACE(domains,(SELECT id FROM sessions WHERE name='eng'),'ses_someone_else')`)
	if got := send("confirm", "app", projPay); got != "superseded:guardrail" {
		t.Fatal("an untrusted session was confirmed:", got)
	}
}

// A superseded decision is not final (D3-M2): the claim is decided again on
// the next tick, never left to stall.
func TestDispatcherSupersededIsDecidedAgain(t *testing.T) {
	e := setup(t)
	e.dispatcherOn("eng")
	e.f.dispatchIssues = map[string]any{issueA: appIssue(issueA, projPay, "unstarted")}
	e.propose("eng", issueA, projPay)
	// The engine decided under policy revision 1; the owner edits it before
	// the record lands, so the record is superseded.
	r, _ := e.s.ExecuteDispatcher(context.Background(), e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_cases_due"})
	c := r.Data.(map[string]any)["cases"].([]any)[0].(map[string]any)
	out, err := e.s.ExecuteDispatcher(context.Background(), e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_decision_record", Args: map[string]any{
		"subject_id": c["subject_id"], "subject_revision": c["subject_revision"], "policy_revision": 0, "engine": "rules", "proposal": "confirm", "reason_code": "trusted", "action": "confirm", "facts_source": "app", "app_project_id": projPay}})
	if err != nil || out.Data.(map[string]any)["action"] != "superseded" {
		t.Fatal(out, err)
	}
	if tick := e.tick(); tick.Dispatch == nil || tick.Dispatch.Actions["confirm"] != 1 || e.assignmentState("eng") != "confirmed" {
		t.Fatal("a superseded claim was not decided again", tick.Dispatch)
	}
}

// A case carries the holder's leased presence (§1): a live lease on a quiet
// session is idle, an expired one asleep, whatever it last claimed.
func TestDispatcherCaseLiveness(t *testing.T) {
	e := setup(t)
	e.dispatcherOn("eng")
	e.propose("eng", issueA, projPay)
	liveness := func() any {
		t.Helper()
		r, err := e.s.ExecuteDispatcher(context.Background(), e.pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_cases_due"})
		if err != nil {
			t.Fatal(err)
		}
		return r.Data.(map[string]any)["cases"].([]any)[0].(map[string]any)["liveness"]
	}
	at := func(d time.Duration) string {
		return time.Now().UTC().Add(d).Format("2006-01-02T15:04:05.000000000Z07:00")
	}
	e.sql(`UPDATE sessions SET last_seen='` + at(-time.Hour) + `',alive_until='` + at(time.Minute) + `',alive_state='idle' WHERE name='eng'`)
	if got := liveness(); got != "idle" {
		t.Fatal("live lease, quiet:", got)
	}
	e.sql(`UPDATE sessions SET alive_until='` + at(-time.Second) + `',alive_state='working' WHERE name='eng'`)
	if got := liveness(); got != "asleep" {
		t.Fatal("expired lease, says working:", got)
	}
}

// v0.3.5: a tick with the dispatcher off creates no dispatcher session, and
// retires (never deletes) any left from an earlier enable or from v0.3.4,
// which created them on every tick; enabling makes the same ones active.
func TestDispatcherOffHasNoSessions(t *testing.T) {
	e := setup(t)
	count := func(status string) int {
		t.Helper()
		var n int
		e.queryRow(`SELECT COUNT(*) FROM sessions WHERE role='system' AND name LIKE 'clawbridge-dispatcher%' AND status='`+status+`'`, &n)
		return n
	}
	for range 2 {
		if tick := e.tick(); tick.Error != "" || tick.Dispatch != nil || count("active")+count("retired") != 0 {
			t.Fatal("a tick with the dispatcher off created its sessions:", tick.Error, count("active"), count("retired"))
		}
	}
	// As v0.3.4 left them: two active sessions, with the dispatcher off.
	e.sql(`INSERT INTO dispatcher_policies(project_id,revision,enabled,domains,updated_at,updated_by_principal) VALUES(?,1,1,'{"domains":[]}','2026-01-01T00:00:00Z','x')`, e.pid)
	e.tick()
	var ids string
	e.queryRow(`SELECT GROUP_CONCAT(id) FROM sessions WHERE name LIKE 'clawbridge-dispatcher%'`, &ids)
	if count("active") != 2 {
		t.Fatal("an enabled tick has no sessions:", count("active"))
	}
	e.sql(`UPDATE dispatcher_policies SET enabled=0`)
	for range 2 {
		if tick := e.tick(); tick.Error != "" || count("active") != 0 || count("retired") != 2 {
			t.Fatal("the leftovers were not retired:", tick.Error, count("active"), count("retired"))
		}
	}
	e.sql(`UPDATE dispatcher_policies SET enabled=1`)
	e.tick()
	var again string
	e.queryRow(`SELECT GROUP_CONCAT(id) FROM sessions WHERE name LIKE 'clawbridge-dispatcher%'`, &again)
	if count("active") != 2 || count("retired") != 0 || again != ids {
		t.Fatal("enabling did not bring back the same sessions:", count("active"), ids, again)
	}
}
