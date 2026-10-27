package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// chatHarness is a project in model mode with a live agent: the owner's
// active key (4 runs an hour, so a chat share of 1) and a stamped project
// token.
type chatHarness struct {
	*modelHarness
	pid, token string
}

func newChatHarness(t *testing.T) *chatHarness {
	t.Helper()
	h := newModelHarness(t)
	h.setup(map[string]any{"caps": map[string]any{"daily_tokens": 500000, "runs_per_hour": 4, "runs_per_day": 8}})
	cred, cfg := h.start()
	if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 1, "active", ""); err != nil {
		t.Fatal(err)
	}
	c := &chatHarness{modelHarness: h}
	if err := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&c.pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.db.Exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES(?,1,1,'model','{"domains":[]}','2026-01-01T00:00:00Z','x')`, c.pid); err != nil {
		t.Fatal(err)
	}
	tokens, err := h.s.RenewAgentTokens(h.ctx, cred, cfg.BindingSecret)
	if err != nil || len(tokens) != 1 || tokens[0].ProjectID != c.pid {
		t.Fatal(tokens, err)
	}
	c.token = tokens[0].Token
	if _, err = h.s.ModelHello(h.ctx, c.token, 0); err != nil {
		t.Fatal(err)
	}
	return c
}

// as runs op as principal, with no session.
func (c *chatHarness) as(principal, op string, a map[string]any) (map[string]any, error) {
	scoped := *c.s
	if principal != "" {
		scoped.principal = principal
	}
	r, err := scoped.Execute(context.Background(), Request{Operation: op, Workspace: "w", Project: "p", Args: a})
	m, _ := r.Data.(map[string]any)
	return m, err
}

// op runs a chat op as principal holding role, as the hosted authority
// would resolve it: a local store makes everyone its owner.
func (c *chatHarness) op(principal, role, op string, a map[string]any) (any, error) {
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
	out, err := scoped.dispatcherChatOp(c.ctx, t, w, p, role, Request{Operation: op, Args: a})
	if err == nil {
		err = tx.Commit()
	}
	return out, err
}

// propose makes session a's pending claim, returning its ID and revision.
func (c *chatHarness) propose(issue string) (string, string) {
	c.t.Helper()
	c.call("assignment_propose", "a", map[string]any{"issue_uuid": issue, "issue_identifier": "FIX-2"})
	var id, revision string
	if err := c.s.db.QueryRow(`SELECT id,revision FROM session_assignments WHERE issue_uuid=?`, issue).Scan(&id, &revision); err != nil {
		c.t.Fatal(err)
	}
	return id, revision
}

func code(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return ""
}

// The ask: an owner's question opens one chat case, as the agent sees it;
// one open chat per asker, naming the open case; never from a session;
// bounded text; only in model mode with a live agent.
func TestChatAsk(t *testing.T) {
	c := newChatHarness(t)
	if _, err := c.s.Execute(c.ctx, c.request("dispatcher_chat_ask", "a", map[string]any{"text": "why?"})); code(err) != "forbidden" {
		t.Fatal("a session asked:", err)
	}
	for _, text := range []any{"", "   ", strings.Repeat("x", maxChatQuestion+1), 5} {
		if _, err := c.as("", "dispatcher_chat_ask", map[string]any{"text": text}); err == nil {
			t.Fatal("asked", text)
		}
	}
	asked, err := c.as("", "dispatcher_chat_ask", map[string]any{"text": "Why was FIX-1 escalated?"})
	if err != nil {
		t.Fatal(err)
	}
	id := asked["case_id"].(string)
	if _, err = c.as("", "dispatcher_chat_ask", map[string]any{"text": "again"}); code(err) != "conflict" || !strings.Contains(err.Error(), id) {
		t.Fatal("a second open chat:", err)
	}
	var history string
	if err = c.s.db.QueryRow(`SELECT details FROM history WHERE action='dispatcher_chat_ask'`).Scan(&history); err != nil || strings.Contains(history, "FIX-1") || !strings.Contains(history, `"length":24`) {
		t.Fatal("the audit:", history, err)
	}
	next, err := c.s.ModelNextCase(c.ctx, c.token, 0)
	if err != nil || next.Case == nil {
		t.Fatal(next, err)
	}
	view := next.Case
	message := view["message"].(map[string]any)
	from := message["from"].(map[string]any)
	if view["case_id"] != id || view["kind"] != "chat" || message["text"] != "Why was FIX-1 escalated?" || from["session"] != "" || !strings.HasPrefix(from["principal"].(string), "p_") || strings.Contains(from["principal"].(string), "@") {
		t.Fatal("the chat case:", view)
	}
	if len(view["permitted_actions"].([]string)) != 0 || len(view["reason_codes"].([]string)) != 0 || len(view["facts"].(map[string]any)) != 0 {
		t.Fatal("a chat case carries no decide fields:", view)
	}
	// Off: refused.
	if _, err = c.s.db.Exec(`UPDATE dispatcher_policies SET mode='rules'`); err != nil {
		t.Fatal(err)
	}
	if _, err = c.as("", "dispatcher_chat_ask", map[string]any{"text": "hello"}); code(err) != "invalid" {
		t.Fatal("asked in rules mode:", err)
	}
}

// No live agent: refused at once, so the CLI doesn't wait for nothing.
func TestChatNeedsAnAgent(t *testing.T) {
	c := newChatHarness(t)
	if _, err := c.s.db.Exec(`UPDATE dispatcher_model_tokens SET last_used_at='2026-01-01T00:00:00.000000000Z'`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.as("", "dispatcher_chat_ask", map[string]any{"text": "hello"}); code(err) != "unavailable" {
		t.Fatal(err)
	}
}

// D6-M1: chats use at most a quarter of the runs, the ask is refused past
// it, and cases/next serves a decide case before an older chat.
func TestChatShareAndDecideFirst(t *testing.T) {
	c := newChatHarness(t)
	asked, err := c.as("", "dispatcher_chat_ask", map[string]any{"text": "first"})
	if err != nil {
		t.Fatal(err)
	}
	// A decide case opened after the chat.
	subject, revision := c.propose("bbbb1111-1111-4111-8111-111111111111")
	if _, err = c.s.db.Exec(`INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,deadline,created_at) VALUES('case_d',?,'decide','proposal',?,?,'2999-01-01T00:00:00.000000000Z','2999-01-01T00:00:00.000000000Z')`, c.pid, subject, revision); err != nil {
		t.Fatal(err)
	}
	next, err := c.s.ModelNextCase(c.ctx, c.token, 0)
	if err != nil || next.Case == nil || next.Case["case_id"] != "case_d" {
		t.Fatal("the decide case first:", next, err)
	}
	if _, err = c.s.db.Exec(`UPDATE dispatcher_cases SET closed_at=created_at,outcome='abstain' WHERE id='case_d'`); err != nil {
		t.Fatal(err)
	}
	if next, err = c.s.ModelNextCase(c.ctx, c.token, 0); err != nil || next.Case == nil || next.Case["case_id"] != asked["case_id"] {
		t.Fatal("then the chat:", next, err)
	}
	// The served chat used the hour's chat share (4 runs / 4).
	if _, err = c.s.db.Exec(`UPDATE dispatcher_cases SET closed_at=served_at,outcome='reply' WHERE kind='chat'`); err != nil {
		t.Fatal(err)
	}
	if _, err = c.as("", "dispatcher_chat_ask", map[string]any{"text": "second"}); code(err) != "rate_limited" {
		t.Fatal("past the chat share:", err)
	}
	// And an unserved chat waits rather than eat into the decides' runs.
	if _, err = c.s.db.Exec(`INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,deadline,created_at,asked_by,message) VALUES('case_c',?,'chat','message','msg_x','2999-01-01T00:00:00.000000000Z','2026-01-01T00:00:00.000000000Z','someone','{}')`, c.pid); err != nil {
		t.Fatal(err)
	}
	if next, err = c.s.ModelNextCase(c.ctx, c.token, 0); err != nil || next.Case != nil {
		t.Fatal("a chat past the share was served:", next, err)
	}
}

// #121 M1: the share is a quarter, rounded down: caps under 4 runs an hour
// (or a day) leave chat none, refused as such, and no chat is served; at 4
// it gets one.
func TestChatShareNeedsFourRuns(t *testing.T) {
	for _, runs := range []int{1, 3, 4} {
		c := newChatHarness(t)
		if _, err := c.s.db.Exec(`UPDATE dispatcher_model_keys SET caps=?`, fmt.Sprintf(`{"daily_tokens":500000,"runs_per_hour":%d,"runs_per_day":%d}`, runs, 8)); err != nil {
			t.Fatal(err)
		}
		_, err := c.as("", "dispatcher_chat_ask", map[string]any{"text": "why?"})
		if runs == 4 {
			if err != nil {
				t.Fatal(runs, err)
			}
			if next, err := c.s.ModelNextCase(c.ctx, c.token, 0); err != nil || next.Case == nil {
				t.Fatal("at 4 runs the chat is served:", next, err)
			}
			continue
		}
		if code(err) != "rate_limited" || !strings.Contains(err.Error(), "no runs for chat") {
			t.Fatal(runs, "runs an hour:", err)
		}
		if _, err = c.s.db.Exec(`INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,deadline,created_at,asked_by,message) VALUES('case_c',?,'chat','message','msg_x','2999-01-01T00:00:00.000000000Z','2026-01-01T00:00:00.000000000Z','someone','{}')`, c.pid); err != nil {
			t.Fatal(err)
		}
		if next, err := c.s.ModelNextCase(c.ctx, c.token, 0); err != nil || next.Case != nil {
			t.Fatal(runs, "runs an hour served a chat:", next, err)
		}
	}
}

// D6-M1(c): leads ask only once the owner allows it; show is the asker's
// own; the decision log reads the same for owners and leads, and no
// further.
func TestChatLeads(t *testing.T) {
	c := newChatHarness(t)
	owner, lead := c.s.principal, "lead@example.test"
	owners, err := c.op(owner, "owner", "dispatcher_chat_ask", map[string]any{"text": "owner's question"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.op(lead, "lead", "dispatcher_chat_ask", map[string]any{"text": "lead's question"}); code(err) != "forbidden" {
		t.Fatal("a lead asked unallowed:", err)
	}
	if _, err = c.op(lead, "lead", "dispatcher_chat_leads", map[string]any{"allow": true, "expected_revision": 1}); err == nil {
		t.Fatal("a lead allowed leads")
	}
	if _, err = c.op(owner, "owner", "dispatcher_chat_leads", map[string]any{"allow": true, "expected_revision": 7}); code(err) != "conflict" {
		t.Fatal("a stale revision:", err)
	}
	set, err := c.op(owner, "owner", "dispatcher_chat_leads", map[string]any{"allow": true, "expected_revision": 1})
	if m := set.(map[string]any); err != nil || m["revision"] != int64(2) || m["chat_leads"] != true {
		t.Fatal(set, err)
	}
	asked, err := c.op(lead, "lead", "dispatcher_chat_ask", map[string]any{"text": "lead's question"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.op("member@example.test", "member", "dispatcher_chat_ask", map[string]any{"text": "member's"}); err == nil {
		t.Fatal("a member asked")
	}
	if _, err = c.op(lead, "lead", "dispatcher_chat_show", map[string]any{"case_id": owners.(map[string]any)["case_id"]}); code(err) != "not_found" {
		t.Fatal("a lead read the owner's chat:", err)
	}
	shown, err := c.op(lead, "lead", "dispatcher_chat_show", map[string]any{})
	if m := shown.(map[string]any); err != nil || m["case_id"] != asked.(map[string]any)["case_id"] || m["question"] != "lead's question" || m["state"] != "waiting" {
		t.Fatal(shown, err)
	}
	subject, revision := c.propose("cccc1111-1111-4111-8111-111111111111")
	if _, err = c.s.db.Exec(`INSERT INTO dispatcher_decisions(id,project_id,subject_type,subject_id,subject_revision,engine,proposal,reason_code,explanation,guardrail,action,policy_revision,created_at) VALUES('d1',?,'proposal',?,?,'model','wait','uncertain','checking','permitted','wait',1,'2999-01-01T00:00:00.000000000Z'),('d0',?,'proposal',?,'r0','rules','wait','uncertain','','permitted','wait',1,'2999-01-01T00:00:00.000000000Z')`, c.pid, subject, revision, c.pid, subject); err != nil {
		t.Fatal(err)
	}
	if _, err = c.s.db.Exec(`INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,deadline,created_at) VALUES('case_d',?,'decide','proposal',?,?,'2026-01-01T00:00:00.000000000Z','2026-01-01T00:00:00.000000000Z')`, c.pid, subject, revision); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"owner", "lead"} {
		log, err := c.op(lead, role, "dispatcher_decisions_list", map[string]any{"since_hours": 24, "limit": 50})
		list, _ := log.([]any)
		if err != nil || len(list) != 2 {
			t.Fatal(role, log, err)
		}
		cases := map[any]any{}
		for _, d := range list {
			cases[d.(map[string]any)["engine"]] = d.(map[string]any)["case_id"]
		}
		if cases["model"] != "case_d" || cases["rules"] != nil {
			t.Fatal("a model decision names its case, a rules one none:", list)
		}
	}
	for _, a := range []map[string]any{{"since_hours": 0, "limit": 5}, {"since_hours": 24, "limit": 51}} {
		if _, err = c.op(owner, "owner", "dispatcher_decisions_list", a); err == nil {
			t.Fatal("read the log with", a)
		}
	}
	if _, err = c.op("member@example.test", "member", "dispatcher_decisions_list", map[string]any{"since_hours": 24, "limit": 50}); err == nil {
		t.Fatal("a member read the decision log")
	}
}

// Show's states: a reply, an abstain, an expiry and a switch-off each
// read as what happened (D6-L3).
func TestChatShowStates(t *testing.T) {
	c := newChatHarness(t)
	asked, err := c.as("", "dispatcher_chat_ask", map[string]any{"text": "q"})
	if err != nil {
		t.Fatal(err)
	}
	id := asked["case_id"].(string)
	for _, s := range []struct{ set, state, field, want string }{
		{`closed_at=NULL,outcome='',deadline='2000-01-01T00:00:00.000000000Z'`, "expired", "", ""},
		{`closed_at=created_at,outcome='reply',outcome_body='{"tool":"reply","text":"because"}'`, "replied", "reply", "because"},
		{`closed_at=created_at,outcome='abstain',outcome_body='{"tool":"abstain","reason_code":"uncertain"}'`, "abstained", "reason_code", "uncertain"},
		{`closed_at=created_at,outcome='model_off'`, "closed", "reason_code", "model_off"},
	} {
		if _, err = c.s.db.Exec(`UPDATE dispatcher_cases SET `+s.set+` WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		shown, err := c.as("", "dispatcher_chat_show", map[string]any{"case_id": id})
		if err != nil || shown["state"] != s.state || s.field != "" && shown[s.field] != s.want {
			t.Fatal(s.state, shown, err)
		}
	}
	// #121 L2: an open chat, waiting or past its deadline, in a project now
	// in rules mode or off reads as the model turned off.
	for _, policy := range []string{`mode='rules'`, `mode='model',enabled=0`} {
		for _, deadline := range []string{"2999-01-01T00:00:00.000000000Z", "2000-01-01T00:00:00.000000000Z"} {
			if _, err = c.s.db.Exec(`UPDATE dispatcher_cases SET closed_at=NULL,outcome='',deadline=? WHERE id=?`, deadline, id); err != nil {
				t.Fatal(err)
			}
			if _, err = c.s.db.Exec(`UPDATE dispatcher_policies SET ` + policy); err != nil {
				t.Fatal(err)
			}
			shown, err := c.as("", "dispatcher_chat_show", map[string]any{"case_id": id})
			if err != nil || shown["state"] != "closed" || shown["reason_code"] != "model_off" {
				t.Fatal(policy, deadline, shown, err)
			}
		}
	}
}

// #121 L3: a chat past its deadline answers deadline_passed but nudges no
// rules fallback; a decide case past its deadline does.
func TestChatDeadlineNudgesNoFallback(t *testing.T) {
	c := newChatHarness(t)
	asked, err := c.as("", "dispatcher_chat_ask", map[string]any{"text": "why?"})
	if err != nil {
		t.Fatal(err)
	}
	if next, err := c.s.ModelNextCase(c.ctx, c.token, 0); err != nil || next.Case == nil {
		t.Fatal(next, err)
	}
	subject, revision := c.propose("cccc1111-1111-4111-8111-111111111111")
	if _, err = c.s.db.Exec(`INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,deadline,created_at,lease_key,lease_instance,served_at) SELECT 'case_d',project_id,'decide','proposal',?,?,deadline,created_at,lease_key,lease_instance,served_at FROM dispatcher_cases WHERE id=?`, subject, revision, asked["case_id"]); err != nil {
		t.Fatal(err)
	}
	if _, err = c.s.db.Exec(`UPDATE dispatcher_cases SET deadline='2000-01-01T00:00:00.000000000Z'`); err != nil {
		t.Fatal(err)
	}
	for id, fallback := range map[string]bool{asked["case_id"].(string): false, "case_d": true} {
		out, err := c.s.ModelTool(c.ctx, c.token, id, "list_sessions", map[string]any{"active_only": true})
		if err != nil || out.Error != "deadline_passed" || out.Fallback != fallback {
			t.Fatal(id, out, err)
		}
	}
}
