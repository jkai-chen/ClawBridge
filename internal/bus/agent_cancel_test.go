package bus

import (
	"context"
	"strings"
	"testing"
)

const acProject = "3333aaaa-3333-4333-8333-333333333333"

func acRecord(op, issue, action string) map[string]any {
	a := map[string]any{"operation_id": op, "issue": issue, "identifier": "FIX-1", "action": action, "reason": "superseded by FIX-9", "linear_project": acProject, "kind": "Engineering"}
	if action == "duplicated" {
		a["duplicate_of"] = sfIssueNext
		a["reason"] = ""
	}
	return a
}

func (w *watchHarness) agentCancelRecord(session string, a map[string]any) (map[string]any, error) {
	r, err := w.s.Execute(WithAppStep(context.Background()), w.request("agent_cancel_record", session, a))
	m, _ := r.Data.(map[string]any)
	return m, err
}

// agentCancelNotices are the agent-cancel notices, each with its one
// recipient's name.
func (w *watchHarness) agentCancelNotices() map[string]string {
	w.t.Helper()
	rows, err := w.s.db.Query(`SELECT m.body,s.name FROM messages m JOIN deliveries d ON d.message_id=m.id JOIN sessions s ON s.id=d.recipient_id WHERE m.kind='agent-cancel'`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var body, to string
		_ = rows.Scan(&body, &to)
		out[to] = body
	}
	return out
}

// AGENT-CANCEL R9: an agent's cancel is audited with IDs and counts (the
// reason only in the notice), and the coordinator and the issue's
// assignee are told; a retry of the same operation tells no one twice.
func TestAgentCancelRecordAuditsAndNotifies(t *testing.T) {
	w := newWatchHarness(t)
	got, err := w.agentCancelRecord("b", acRecord("op-1", watchIssue, "canceled"))
	if err != nil || got["notified"] != 2 {
		t.Fatal(got, err)
	}
	n := w.agentCancelNotices()
	for _, to := range []string{"c", "a"} {
		if n[to] != "FIX-1 canceled by agent session b: superseded by FIX-9\n"+agentCancelLine {
			t.Fatalf("%s: %q", to, n[to])
		}
	}
	var details, by string
	if err = w.s.db.QueryRow(`SELECT details,changed_by_session FROM history WHERE entity='linear_issue' AND entity_key=? AND action='issue_canceled'`, watchIssue).Scan(&details, &by); err != nil || by != w.sessionID("b") || !strings.Contains(details, `"operation_id":"op-1"`) || !strings.Contains(details, `"reason_chars":19`) || strings.Contains(details, "superseded") {
		t.Fatal("audit:", details, by, err)
	}
	if got, err = w.agentCancelRecord("b", acRecord("op-1", watchIssue, "canceled")); err != nil || got["replayed"] != true || len(w.agentCancelNotices()) != 2 {
		t.Fatal("replay:", got, err)
	}
	// The assignee's own duplicate tells only the coordinator.
	if got, err = w.agentCancelRecord("a", acRecord("op-2", watchIssue, "duplicated")); err != nil || got["notified"] != 1 {
		t.Fatal(got, err)
	}
	var body string
	if err = w.s.db.QueryRow(`SELECT body FROM messages WHERE kind='agent-cancel' AND body LIKE '%Duplicate%'`).Scan(&body); err != nil || body != "FIX-1 marked Duplicate of "+sfIssueNext+" by agent session a\n"+agentCancelLine {
		t.Fatalf("%q %v", body, err)
	}
}

// Only the server's own step records, as a session, with valid fields.
func TestAgentCancelRecordRefusals(t *testing.T) {
	w := newWatchHarness(t)
	if _, err := w.s.Execute(context.Background(), w.request("agent_cancel_record", "a", acRecord("op-1", watchIssue, "canceled"))); err == nil || !strings.Contains(err.Error(), "recorded by the server") {
		t.Fatal("a client's record:", err)
	}
	if _, err := w.agentCancelRecord("", acRecord("op-1", watchIssue, "canceled")); err == nil {
		t.Fatal("a person's record")
	}
	for name, edit := range map[string]func(map[string]any){
		"action":           func(a map[string]any) { a["action"] = "closed" },
		"no reason":        func(a map[string]any) { a["reason"] = "" },
		"issue":            func(a map[string]any) { a["issue"] = "FIX-1" },
		"duplicate_of":     func(a map[string]any) { a["duplicate_of"] = sfIssueNext },
		"long reason":      func(a map[string]any) { a["reason"] = strings.Repeat("x", 2001) },
		"no operation":     func(a map[string]any) { a["operation_id"] = "" },
		"kind with a line": func(a map[string]any) { a["kind"] = "a\nb" },
	} {
		a := acRecord("op-1", watchIssue, "canceled")
		edit(a)
		if _, err := w.agentCancelRecord("a", a); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	if n := w.agentCancelNotices(); len(n) != 0 {
		t.Fatal(n)
	}
}

// O4: after a cancel the agent releases with no outcome, and the release
// queues no state move, so the state stays canceled.
func TestAReleaseWithoutOutcomeMovesNoState(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	w.call("assignment_release", "b", map[string]any{"revision": w.live("b")["revision"]})
	if s, _, _ := w.stateRow(id, "close"); s == "pending" {
		t.Fatal("a release without an outcome has a state part:", s)
	}
}

// The shared ledger keeps a cancel's steps under the caller's ID and the
// step; a caller's own ID never holds a colon.
func TestLedgerKeepsCancelSteps(t *testing.T) {
	for id, want := range map[string]bool{
		"op-1": true, "op-1:comment": true, "op-1:relation": true, "op-1:state": true,
		"op-1:other": false, ":state": false, strings.Repeat("o", 120) + ":state": false, strings.Repeat("o", 119) + ":state": true,
	} {
		if got := validLedgerKey(strings.Repeat("a", 64), id) == nil; got != want {
			t.Errorf("%s: %v", id, got)
		}
	}
}
