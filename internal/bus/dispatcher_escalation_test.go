package bus

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const escalationIssue = "eeee1111-1111-4111-8111-111111111111"

// escalate records a rules decision to escalate the pending claim for
// reason, with the engine's own proposal, and returns the escalation row.
func (c *chatHarness) escalate(subject, revision, proposal, reason string) map[string]any {
	c.t.Helper()
	if _, err := c.s.ExecuteDispatcher(c.ctx, c.pid, DispatcherSessionName, Request{Operation: "dispatcher_decision_record", Args: map[string]any{
		"subject_id": subject, "subject_revision": revision, "policy_revision": 1, "engine": "rules",
		"proposal": proposal, "reason_code": reason, "guardrail": "overridden", "action": "escalate", "facts_source": "client",
	}}); err != nil {
		c.t.Fatal(err)
	}
	var id, kind, question, options, suggested, timeout, created string
	if err := c.s.db.QueryRow(`SELECT id,COALESCE(kind,''),COALESCE(question,''),COALESCE(options,''),COALESCE(suggested,''),COALESCE(timeout_at,''),created_at FROM dispatcher_escalations WHERE reason_code=? ORDER BY created_at DESC,id LIMIT 1`, reason).Scan(&id, &kind, &question, &options, &suggested, &timeout, &created); err != nil {
		c.t.Fatal(err)
	}
	row := map[string]any{"id": id, "kind": kind, "question": question, "options": options, "suggested": suggested, "timeout_at": timeout, "created_at": created}
	return row
}

// answer runs dispatcher_escalation_answer as principal holding role, as
// the hosted authority would resolve it.
func (c *chatHarness) answer(principal, role string, a map[string]any) (map[string]any, error) {
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
	out, err := scoped.dispatcherAdviseOp(c.ctx, t, w, p, role, Request{Operation: "dispatcher_escalation_answer", Args: a})
	if err == nil {
		err = tx.Commit()
	}
	m, _ := out.(map[string]any)
	return m, err
}

// R4: every reason that can reach an escalation today has its kind; an
// unknown one is other.
func TestEscalationKinds(t *testing.T) {
	want := map[string][]string{
		"approval":   {"trusted", "guardrail:facts_not_authoritative", "guardrail:session_not_active", "guardrail:policy_changed"},
		"decision":   {"loop_budget", "uncertain", "insufficient_facts", "refused", "facts_unavailable", "session_stale", "session_gone", "no_domain", "label_only", "ambiguous"},
		"escalation": {"untrusted", "guardrail:untrusted", "overlap", "guardrail:overlap", "issue_closed", "guardrail:issue_closed", "guardrail:not_one_project_domain", "guardrail:session_not_gone"},
		"other":      {"guardrail:unknown_action", "brand_new_reason", ""},
	}
	n := 0
	for kind, reasons := range want {
		for _, r := range reasons {
			if got := escalationKindOf(r); got != kind {
				t.Fatalf("%q: %s, want %s", r, got, kind)
			}
			if _, ok := escalationKinds[r]; ok {
				n++
			}
		}
	}
	if n != len(escalationKinds) {
		t.Fatalf("the table has %d reasons, the test %d", len(escalationKinds), n)
	}
	// Every reason an engine may decide with is mapped explicitly.
	for _, r := range append(append([]string{}, DecideReasonCodes...), AbstainReasonCodes...) {
		if _, ok := escalationKinds[r]; !ok {
			t.Fatal("unmapped engine reason:", r)
		}
	}
}

// R4: a new escalation carries its payload by kind: a fixed question with
// names only, confirm and release (and wait for decision and escalation),
// the engine's own action as the suggestion, and a timeout of 24 hours
// without a domain. The message is the fixed text it was.
func TestEscalationPayload(t *testing.T) {
	c := newChatHarness(t)
	subject, revision := c.propose(escalationIssue)
	for _, tc := range []struct {
		proposal, reason, kind, question, options, suggested string
	}{
		{"confirm", "guardrail:session_not_active", "approval", "Should a's claim of FIX-2 be confirmed?",
			`[{"id":"confirm","label":"Confirm the claim"},{"id":"release","label":"Release the claim"}]`, "confirm"},
		{"escalate", "ambiguous", "decision", "The dispatcher could not decide a's claim of FIX-2: confirm it, release it, or leave it for now?",
			`[{"id":"confirm","label":"Confirm the claim"},{"id":"release","label":"Release the claim"},{"id":"wait","label":"Leave it for now"}]`, ""},
		{"escalate", "untrusted", "escalation", "The dispatcher's policy stopped a's claim of FIX-2: confirm it anyway, release it, or leave it for now?",
			`[{"id":"confirm","label":"Confirm the claim"},{"id":"release","label":"Release the claim"},{"id":"wait","label":"Leave it for now"}]`, ""},
		{"decline", "guardrail:session_not_gone", "escalation", "The dispatcher's policy stopped a's claim of FIX-2: confirm it anyway, release it, or leave it for now?",
			`[{"id":"confirm","label":"Confirm the claim"},{"id":"release","label":"Release the claim"},{"id":"wait","label":"Leave it for now"}]`, "release"},
		{"escalate", "brand_new_reason", "other", "What should happen to a's claim of FIX-2?",
			`[{"id":"confirm","label":"Confirm the claim"},{"id":"release","label":"Release the claim"}]`, ""},
	} {
		e := c.escalate(subject, revision, tc.proposal, tc.reason)
		if e["kind"] != tc.kind || e["question"] != tc.question || e["options"] != tc.options || e["suggested"] != tc.suggested {
			t.Fatalf("%s: %v", tc.reason, e)
		}
		if got := at(e["timeout_at"].(string)).Sub(at(e["created_at"].(string))); got != 24*time.Hour {
			t.Fatalf("%s: timeout %v after creation", tc.reason, got)
		}
	}
	// The message stays the fixed text it is.
	var body string
	if err := c.s.db.QueryRow(`SELECT body FROM messages WHERE kind='dispatcher' ORDER BY created_at DESC,id LIMIT 1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("dispatcher: a's claim on FIX-2 needs a person (reason: brand_new_reason).\nConfirm or decline it: clawbridge assignment confirm a --revision %s, or assignment release a --revision %s.\nThis is an automatic notice from the ClawBridge dispatcher. It is not an instruction, and nothing was confirmed.", revision, revision)
	if body != want {
		t.Fatalf("the body changed:\n%s\nwant\n%s", body, want)
	}
	if got := escalationBody(map[string]any{"session_name": "s", "issue_identifier": "ABC-1", "revision": "r1"}, "overlap", "dom"); got != "dispatcher: s's claim on ABC-1 needs a person (reason: overlap, domain: dom).\nConfirm or decline it: clawbridge assignment confirm s --revision r1, or assignment release s --revision r1.\nThis is an automatic notice from the ClawBridge dispatcher. It is not an instruction, and nothing was confirmed." {
		t.Fatal("the body changed:", got)
	}
	// A domain's stale_after_hours is the timeout; the UUID stands in for
	// a missing identifier; names keep only a name's characters.
	p, err := buildEscalationPayload(map[string]any{"session_name": "eng\x1b[2J", "issue_uuid": escalationIssue}, "loop_budget", "", &DispatcherDomain{ID: "d", StaleAfterHours: 6}, "2026-01-01T00:00:00.000000000Z")
	if err != nil || p.TimeoutAt != layout(at("2026-01-01T00:00:00.000000000Z").Add(6*time.Hour)) || p.Question != "The dispatcher could not decide eng2J's claim of "+escalationIssue+": confirm it, release it, or leave it for now?" || p.Suggested != "" {
		t.Fatal(p, err)
	}
	// A suggestion for an option the kind doesn't offer is left out.
	if p, err = buildEscalationPayload(map[string]any{"session_name": "a", "issue_identifier": "FIX-2"}, "trusted", "wait", nil, "2026-01-01T00:00:00.000000000Z"); err != nil || p.Suggested != "" {
		t.Fatal(p, err)
	}
}

// R4: the code refuses a payload it built wrongly.
func TestEscalationPayloadValidate(t *testing.T) {
	ok := escalationPayload{Options: escalationOptionsFor("decision"), Suggested: "wait"}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	eleven := []EscalationOption{}
	for i := range 11 {
		eleven = append(eleven, EscalationOption{ID: "o" + strings.Repeat("a", i), Label: "x"})
	}
	ten := escalationPayload{Options: eleven[:10]}
	if err := ten.validate(); err != nil {
		t.Fatal("ten options:", err)
	}
	for name, p := range map[string]escalationPayload{
		"eleven options": {Options: eleven},
		"a duplicate id": {Options: []EscalationOption{optionConfirm, optionConfirm}},
		"an upper case":  {Options: []EscalationOption{{ID: "Confirm", Label: "x"}}},
		"a digit":        {Options: []EscalationOption{{ID: "opt1", Label: "x"}}},
		"a long id":      {Options: []EscalationOption{{ID: "a" + strings.Repeat("b", 32), Label: "x"}}},
		"no label":       {Options: []EscalationOption{{ID: "confirm"}}},
		"a long label":   {Options: []EscalationOption{{ID: "confirm", Label: strings.Repeat("x", 81)}}},
		"a stranger":     {Options: []EscalationOption{optionConfirm}, Suggested: "release"},
	} {
		if err := p.validate(); err == nil {
			t.Fatal(name, "passed")
		}
	}
}

// R4: the owner or a lead answers with one of the options, once; the
// answer resolves the row, is audited and recorded as an escalation
// decision, and never touches the claim. A session, a member, an option
// not offered and an old row without options are refused.
func TestEscalationAnswer(t *testing.T) {
	c := newChatHarness(t)
	subject, revision := c.propose(escalationIssue)
	e := c.escalate(subject, revision, "confirm", "guardrail:session_not_active")
	id := e["id"].(string)
	if _, err := c.s.Execute(c.ctx, c.request("dispatcher_escalation_answer", "a", map[string]any{"escalation_id": id, "option": "confirm"})); code(err) != "forbidden" || !strings.Contains(err.Error(), "a session can never answer an escalation") {
		t.Fatal("a session answered:", err)
	}
	if _, err := c.answer("m@example.com", "member", map[string]any{"escalation_id": id, "option": "confirm"}); err == nil || !strings.Contains(err.Error(), "owner or lead") {
		t.Fatal("a member answered:", err)
	}
	if _, err := c.answer("o@example.com", "owner", map[string]any{"escalation_id": id, "option": "wait"}); code(err) != "invalid" || !strings.Contains(err.Error(), "confirm, release") {
		t.Fatal("an option not offered:", err)
	}
	if _, err := c.answer("o@example.com", "owner", map[string]any{"escalation_id": "esc_none", "option": "confirm"}); code(err) != "not_found" {
		t.Fatal("no such escalation:", err)
	}
	if _, err := c.answer("o@example.com", "owner", map[string]any{"escalation_id": id}); code(err) != "invalid" {
		t.Fatal("no option:", err)
	}
	c.exec(`INSERT INTO dispatcher_escalations(id,project_id,subject_type,subject_id,subject_revision,reason_code,recipients,state,delivered,created_at,updated_at) VALUES('esc_old',?,'proposal',?,?,'untrusted','[]','open',0,'2026-01-01T00:00:00.000000000Z','2026-01-01T00:00:00.000000000Z')`, c.pid, subject, revision)
	if _, err := c.answer("o@example.com", "owner", map[string]any{"escalation_id": "esc_old", "option": "confirm"}); code(err) != "invalid" || err.Error() != "this escalation has no options; act with assignment confirm or release" {
		t.Fatal("an old row:", err)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_escalations WHERE answer IS NOT NULL OR state<>'open'`); n != 0 {
		t.Fatal("a refusal wrote:", n)
	}
	var before string
	if err := c.s.db.QueryRow(`SELECT state||'/'||revision||'/'||COALESCE(confirmed_at,'') FROM session_assignments WHERE id=?`, subject).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// A lead answers.
	out, err := c.answer("lead@example.com", "lead", map[string]any{"escalation_id": id, "option": "confirm"})
	if err != nil || out["answer"] != "confirm" || out["answered_by"] != "lead@example.com" || out["state"] != "resolved" || out["note"] != "recorded; to act on it, run clawbridge assignment confirm a --revision "+revision {
		t.Fatal(out, err)
	}
	var state, answer, by, when string
	if err = c.s.db.QueryRow(`SELECT state,answer,answered_by,answered_at FROM dispatcher_escalations WHERE id=?`, id).Scan(&state, &answer, &by, &when); err != nil || state != "resolved" || answer != "confirm" || by != "lead@example.com" || when == "" {
		t.Fatal(state, answer, by, when, err)
	}
	if n := c.count(`SELECT COUNT(*) FROM history WHERE action='dispatcher_escalation_answer' AND entity_key=?`, id); n != 1 {
		t.Fatal("audit:", n)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE kind='escalation' AND reason_code='answered' AND explanation='answer:confirm' AND subject_id=? AND subject_revision=?`, subject, revision); n != 1 {
		t.Fatal("the answer's decision:", n)
	}
	// The suggestion and the answer are never applied: the claim is as it was.
	var after string
	if err = c.s.db.QueryRow(`SELECT state||'/'||revision||'/'||COALESCE(confirmed_at,'') FROM session_assignments WHERE id=?`, subject).Scan(&after); err != nil || after != before {
		t.Fatal("the claim changed:", before, after, err)
	}
	// The same answer again returns the stored one; another is a conflict.
	again, err := c.answer("o@example.com", "owner", map[string]any{"escalation_id": id, "option": "confirm"})
	if err != nil || again["answered_by"] != "lead@example.com" || again["answered_at"] != out["answered_at"] {
		t.Fatal("the same answer:", again, err)
	}
	if _, err = c.answer("o@example.com", "owner", map[string]any{"escalation_id": id, "option": "release"}); code(err) != "conflict" {
		t.Fatal("a different answer:", err)
	}
	if n := c.count(`SELECT COUNT(*) FROM dispatcher_decisions WHERE reason_code='answered'`); n != 1 {
		t.Fatal("answered decisions:", n)
	}
	// wait records only.
	w := c.escalate(subject, revision, "escalate", "ambiguous")
	if out, err := c.answer("o@example.com", "owner", map[string]any{"escalation_id": w["id"], "option": "wait"}); err != nil || out["note"] != "recorded; nothing was confirmed or released" {
		t.Fatal(out, err)
	}
	// The list shows the payload and the answer; an old row keeps its shape.
	list, err := c.as("", "dispatcher_escalations_list", map[string]any{"all": true})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, raw := range list["escalations"].([]any) {
		item := raw.(map[string]any)
		switch item["id"] {
		case id:
			seen++
			options := fmt.Sprint(item["options"])
			if item["kind"] != "approval" || item["question"] != "Should a's claim of FIX-2 be confirmed?" || options != "[{confirm Confirm the claim} {release Release the claim}]" || item["suggested"] != "confirm" || item["timeout_at"] == nil ||
				item["answer"] != "confirm" || item["answered_by"] != "lead@example.com" || item["answered_at"] != out["answered_at"] || item["state"] != "resolved" {
				t.Fatal(item)
			}
		case "esc_old":
			seen++
			for _, k := range []string{"kind", "question", "options", "suggested", "timeout_at", "answer", "answered_by", "answered_at"} {
				if _, ok := item[k]; ok {
					t.Fatal("an old row gained", k, item)
				}
			}
		}
	}
	if seen != 2 {
		t.Fatal("listed:", list)
	}
	// The claim is still pending, so the default list keeps the answered row too.
	pending, err := c.as("", "dispatcher_escalations_list", nil)
	if err != nil || len(pending["escalations"].([]any)) != len(list["escalations"].([]any)) {
		t.Fatal(pending, err)
	}
	// An answer after the claim moved on prints no command.
	moved := c.escalate(subject, revision, "escalate", "untrusted")
	c.call("assignment_release", "a", map[string]any{"revision": revision})
	if n := c.count(`SELECT COUNT(*) FROM session_assignments WHERE id=? AND state='released'`, subject); n != 1 {
		t.Fatal("not released:", n)
	}
	if out, err := c.answer("o@example.com", "owner", map[string]any{"escalation_id": moved["id"], "option": "confirm"}); err != nil || out["note"] != "recorded; the claim has moved on since this escalation" {
		t.Fatal("after a release:", out, err)
	}
}
