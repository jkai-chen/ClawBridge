package bus

import (
	"fmt"
	"strings"
	"testing"
)

// d7 Q7: the token rule and the launcher's listing follow the model's use,
// through one predicate. Rules mode without advice gets neither; advice
// on gets both, as model mode does; off gets neither.
func TestAdviseTokenRule(t *testing.T) {
	h := newModelHarness(t)
	h.setup(nil)
	cred, cfg := h.start()
	if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 1, "active", ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		enabled     int
		mode, model string
		want        int
	}{
		{1, "rules", `{}`, 0},
		{1, "rules", `{"advise":true}`, 1},
		{1, "model", `{}`, 1},
		{0, "rules", `{"advise":true}`, 0},
	} {
		if _, err := h.s.db.Exec(`DELETE FROM dispatcher_policies`); err != nil {
			t.Fatal(err)
		}
		if _, err := h.s.db.Exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,model,updated_at,updated_by_principal) VALUES(?,1,?,?,'{}',?,'2026-01-01T00:00:00Z','x')`, h.pid, c.enabled, c.mode, c.model); err != nil {
			t.Fatal(err)
		}
		tokens, err := h.s.RenewAgentTokens(h.ctx, cred, cfg.BindingSecret)
		if err != nil && c.want > 0 {
			t.Fatal(c, err)
		}
		if len(tokens) != c.want || len(h.wanted()) != c.want {
			t.Fatal(c, "tokens", tokens, "wanted", h.wanted())
		}
	}
}

// The owner's switches: a preview, then --confirm, by compare-and-swap,
// audited; never from a session; advice only in rules mode with a tested
// key; known tools only; hello says what is on.
func TestAdviseSwitches(t *testing.T) {
	c := newChatHarness(t)
	if _, err := c.s.Execute(c.ctx, c.request("dispatcher_model_advise", "a", map[string]any{"on": true, "expected_revision": 1})); code(err) != "forbidden" {
		t.Fatal("a session switched advice:", err)
	}
	if _, err := c.as("", "dispatcher_model_advise", map[string]any{"on": true, "expected_revision": 1, "confirm": true}); code(err) != "invalid" {
		t.Fatal("advice in model mode:", err)
	}
	if _, err := c.s.db.Exec(`UPDATE dispatcher_policies SET mode='rules'`); err != nil {
		t.Fatal(err)
	}
	preview, err := c.as("", "dispatcher_model_advise", map[string]any{"on": true, "expected_revision": 1})
	if err != nil || preview["preview"] != true || preview["issue_text_to_provider"] != true || preview["provider"] != "anthropic" {
		t.Fatal("the preview:", preview, err)
	}
	if _, err = c.as("", "dispatcher_model_advise", map[string]any{"on": true, "expected_revision": 7, "confirm": true}); code(err) != "conflict" {
		t.Fatal("a stale revision:", err)
	}
	on, err := c.as("", "dispatcher_model_advise", map[string]any{"on": true, "expected_revision": 1, "confirm": true})
	if err != nil || fmt.Sprint(on["revision"]) != "2" {
		t.Fatal(on, err)
	}
	if _, err = c.as("", "dispatcher_model_tools", map[string]any{"tools": []any{"shell"}, "expected_revision": 2, "confirm": true}); code(err) != "invalid" {
		t.Fatal("an unknown tool:", err)
	}
	if _, err = c.as("", "dispatcher_model_tools", map[string]any{"tools": []any{"ask_session", "route_notice", "ask_session"}, "expected_revision": 2, "confirm": true}); err != nil {
		t.Fatal(err)
	}
	// Advice off and on again: the preview lists the tools coming back.
	if _, err = c.as("", "dispatcher_model_advise", map[string]any{"on": false, "expected_revision": 3, "confirm": true}); err != nil {
		t.Fatal(err)
	}
	if again, err := c.as("", "dispatcher_model_advise", map[string]any{"on": true, "expected_revision": 4}); err != nil || fmt.Sprint(again["opt_in_tools"]) != "[route_notice ask_session]" {
		t.Fatal("the preview hid the tools:", again, err)
	}
	if _, err = c.as("", "dispatcher_model_advise", map[string]any{"on": true, "expected_revision": 4, "confirm": true}); err != nil {
		t.Fatal(err)
	}
	hello, err := c.s.ModelHello(c.ctx, c.token, 0)
	if err != nil || !hello.Enabled || fmt.Sprint(hello.OptInTools) != "[route_notice ask_session]" {
		t.Fatal("hello:", hello, err)
	}
	var audited int
	if err = c.s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE action IN ('dispatcher_model_advise','dispatcher_model_tools')`).Scan(&audited); err != nil || audited != 4 {
		t.Fatal("audit:", audited, err)
	}
	if _, err = c.as("", "dispatcher_model_advise", map[string]any{"on": false, "expected_revision": 5, "confirm": true}); err != nil {
		t.Fatal(err)
	}
	if hello, err = c.s.ModelHello(c.ctx, c.token, 0); err != nil || hello.Enabled {
		t.Fatal("hello with advice off:", hello, err)
	}
}

// R132-M1: disable and every enable turn advice and the opt-in tools off,
// and the enable preview says so; the other model settings stay.
func TestAdviseOptInsClearedOnSwitch(t *testing.T) {
	h := dispatcherHarness(t)
	var pid string
	if err := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.switchCall("dispatcher_enable", map[string]any{"expected_revision": 0, "confirm": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.db.Exec(`INSERT INTO dispatcher_model_keys(owner_principal,state,provider_id,base_url,model,ciphertext,nonce,key_fingerprint,config_revision,caps,egress_text_version,egress_confirmed_at,created_by,created_at) VALUES(?,'active','anthropic','https://api.anthropic.com','claude-test','x','y','-key',1,'{"daily_tokens":5000}','v2','2026-01-01T00:00:00Z',?,'2026-01-01T00:00:00Z')`, h.principal(), h.principal()); err != nil {
		t.Fatal(err)
	}
	armed := `{"chat_leads":true,"advise":true,"opt_in_tools":["ask_session"]}`
	model := func() string {
		var m string
		if err := h.s.db.QueryRow(`SELECT model FROM dispatcher_policies WHERE project_id=?`, pid).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if _, err := h.s.db.Exec(`UPDATE dispatcher_policies SET model=? WHERE project_id=?`, armed, pid); err != nil {
		t.Fatal(err)
	}
	preview, err := h.switchCall("dispatcher_enable", map[string]any{"expected_revision": 2, "mode": "model"})
	if err != nil || fmt.Sprint(preview["turns_off"]) != "[advise ask_session]" || model() != armed {
		t.Fatal("the preview:", preview, err, model())
	}
	if _, err = h.switchCall("dispatcher_enable", map[string]any{"expected_revision": 2, "mode": "model", "confirm": true}); err != nil || model() != `{"chat_leads":true}` {
		t.Fatal("enable kept them:", err, model())
	}
	if _, err = h.s.db.Exec(`UPDATE dispatcher_policies SET model=? WHERE project_id=?`, armed, pid); err != nil {
		t.Fatal(err)
	}
	if _, err = h.switchCall("dispatcher_disable", map[string]any{"expected_revision": 3}); err != nil || model() != `{"chat_leads":true}` {
		t.Fatal("disable kept them:", err, model())
	}
}

// One agent serves at most maxModelProjects projects: enable and advise on
// refuse one more. Past it anyway, the first by project ID are served:
// renew issues no more tokens, and doctor says which project is left out.
func TestModelProjectCap(t *testing.T) {
	h := newModelHarness(t)
	h.setup(nil)
	cred, cfg := h.start()
	if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 1, "active", ""); err != nil {
		t.Fatal(err)
	}
	var pid, wid string
	if err := h.s.db.QueryRow(`SELECT id,workspace_id FROM projects WHERE slug='p'`).Scan(&pid, &wid); err != nil {
		t.Fatal(err)
	}
	// IDs that sort before the harness's project, which comes 101st.
	for i := range maxModelProjects {
		id := fmt.Sprintf("a%03d", i)
		if _, err := h.s.db.Exec(`INSERT INTO projects(id,workspace_id,slug,name,created_at) VALUES(?,?,?,?,'2026-01-01T00:00:00Z')`, id, wid, id, id); err != nil {
			t.Fatal(err)
		}
		if _, err := h.s.db.Exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES(?,1,1,'model','{}','2026-01-01T00:00:00Z','x')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.s.db.Exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES(?,1,1,'rules','{"domains":[]}','2026-01-01T00:00:00Z','x')`, pid); err != nil {
		t.Fatal(err)
	}
	// Advice and model mode both preview the 101st as blocked, and
	// refuse it at --confirm.
	preview := h.call("dispatcher_model_advise", "", map[string]any{"on": true, "expected_revision": 1}).(map[string]any)
	if over, _ := preview["model_projects_over_cap"].(map[string]any); over == nil || over["model_projects"] != maxModelProjects+1 || preview["blocked"] != "model_projects_over_cap" {
		t.Fatal("the advise preview:", preview)
	}
	if _, err := h.s.Execute(h.ctx, h.request("dispatcher_model_advise", "", map[string]any{"on": true, "expected_revision": 1, "confirm": true})); code(err) != "invalid" || !strings.Contains(err.Error(), "turn it off in one of them first") {
		t.Fatal("advice past the cap:", err)
	}
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0, projectOne))
	if preview = h.call("dispatcher_enable", "", map[string]any{"mode": "model", "expected_revision": 1}).(map[string]any); preview["blocked"] != "model_projects_over_cap" {
		t.Fatal("the enable preview:", preview)
	}
	if _, err := h.s.Execute(h.ctx, h.request("dispatcher_enable", "", map[string]any{"mode": "model", "expected_revision": 1, "confirm": true})); code(err) != "invalid" || !strings.Contains(err.Error(), "turn it off in one of them first") {
		t.Fatal("model mode past the cap:", err)
	}
	var enabledMode string
	if err := h.s.db.QueryRow(`SELECT mode||model FROM dispatcher_policies WHERE project_id=?`, pid).Scan(&enabledMode); err != nil || enabledMode != "rules{}" {
		t.Fatal("a refusal wrote:", enabledMode, err)
	}
	// An owner can still pass the cap by gaining projects: the first 100
	// by project ID are served, and doctor says so.
	if _, err := h.s.db.Exec(`UPDATE dispatcher_policies SET model='{"advise":true}' WHERE project_id=?`, pid); err != nil {
		t.Fatal(err)
	}
	tokens, err := h.s.RenewAgentTokens(h.ctx, cred, cfg.BindingSecret)
	if err != nil || len(tokens) != maxModelProjects {
		t.Fatal(len(tokens), err)
	}
	for _, tok := range tokens {
		if tok.ProjectID == pid {
			t.Fatal("the 101st project got a token")
		}
	}
	doctor := h.call("dispatcher_model_doctor", "", map[string]any{}).(map[string]any)
	holder := doctor["holders"].([]any)[0].(map[string]any)
	if holder["model_projects"] != maxModelProjects+1 || holder["project_served"] != false {
		t.Fatal("doctor:", holder)
	}
	// Under the cap, neither says anything.
	if _, err = h.s.db.Exec(`DELETE FROM dispatcher_policies WHERE project_id='a000'`); err != nil {
		t.Fatal(err)
	}
	doctor = h.call("dispatcher_model_doctor", "", map[string]any{}).(map[string]any)
	if holder = doctor["holders"].([]any)[0].(map[string]any); holder["model_projects"] != nil {
		t.Fatal("doctor under the cap:", holder)
	}
}
