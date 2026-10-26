package bus

import (
	"context"
	"strings"
	"testing"
)

// switchCall runs enable or disable as the person, with no session.
func (h *harness) switchCall(op string, a map[string]any) (map[string]any, error) {
	h.t.Helper()
	r, err := h.s.Execute(context.Background(), Request{Operation: op, Workspace: "w", Project: "p", Args: a})
	if err != nil {
		return nil, err
	}
	return r.Data.(map[string]any), nil
}

// The owner previews, then confirms: the reserved sessions, the dispatcher
// as coordinator, and an "All work" domain that trusts nobody, audited at
// its revision. A session can't switch it; a stale revision can't either.
// Disable retires the sessions and gives the coordinator back to the
// owner's session; a second enable reactivates the same sessions.
func TestDispatcherEnableDisable(t *testing.T) {
	h := dispatcherHarness(t)
	ctx := context.Background()
	var pid string
	if err := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Execute(ctx, h.request("dispatcher_enable", "a", map[string]any{"expected_revision": 0, "confirm": true})); err == nil || !strings.Contains(err.Error(), "not from a session") {
		t.Fatal("a session turned the dispatcher on:", err)
	}
	preview, err := h.switchCall("dispatcher_enable", map[string]any{"expected_revision": 0})
	if err != nil || preview["preview"] != true || preview["default_domain_created"] != true || len(preview["auto_confirm_domains"].([]string)) != 0 {
		t.Fatal(preview, err)
	}
	if n := count(t, h.s.db, "dispatcher_policies"); n != 0 {
		t.Fatal("the preview wrote", n)
	}
	if _, err = h.switchCall("dispatcher_enable", map[string]any{"expected_revision": 0, "mode": "model", "confirm": true}); err == nil || !strings.Contains(err.Error(), "model setup") {
		t.Fatal("model mode without a tested key:", err)
	}
	on, err := h.switchCall("dispatcher_enable", map[string]any{"expected_revision": 0, "confirm": true})
	if err != nil || on["enabled"] != true || on["revision"] != int64(2) || on["mode"] != "rules" {
		t.Fatal(on, err)
	}
	var coordinator, domains string
	var enabled int
	if err = h.s.db.QueryRow(`SELECT s.name FROM projects p JOIN sessions s ON s.id=p.coordinator_session WHERE p.id=?`, pid).Scan(&coordinator); err != nil || coordinator != DispatcherSessionName {
		t.Fatal(coordinator, err)
	}
	if err = h.s.db.QueryRow(`SELECT enabled,domains FROM dispatcher_policies WHERE project_id=?`, pid).Scan(&enabled, &domains); err != nil || enabled != 1 || !strings.Contains(domains, `"id":"all-work"`) || !strings.Contains(domains, projectOne) || !strings.Contains(domains, projectTwo) || !strings.Contains(domains, `"trusted_sessions":[]`) {
		t.Fatal(enabled, domains, err)
	}
	var sessionID string
	if err = h.s.db.QueryRow(`SELECT id FROM sessions WHERE project_id=? AND name=? AND status='active'`, pid, DispatcherSessionName).Scan(&sessionID); err != nil {
		t.Fatal("no active dispatcher session", err)
	}
	var audited int
	if err = h.s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE entity='dispatcher_policy' AND action='dispatcher_enable' AND details LIKE '%"revision":2%'`).Scan(&audited); err != nil || audited != 1 {
		t.Fatal("not audited with its revision", audited, err)
	}
	if _, err = h.switchCall("dispatcher_enable", map[string]any{"expected_revision": 2, "confirm": true}); err == nil || !strings.Contains(err.Error(), "already on") {
		t.Fatal(err)
	}
	if _, err = h.switchCall("dispatcher_disable", map[string]any{"expected_revision": 1}); err == nil {
		t.Fatal("a stale revision disabled it")
	}
	off, err := h.switchCall("dispatcher_disable", map[string]any{"expected_revision": 2})
	if err != nil || off["enabled"] != false || off["revision"] != int64(3) || off["coordinator"] == "" {
		t.Fatal(off, err)
	}
	var status string
	if err = h.s.db.QueryRow(`SELECT status FROM sessions WHERE id=?`, sessionID).Scan(&status); err != nil || status != "retired" {
		t.Fatal("not retired", status, err)
	}
	if err = h.s.db.QueryRow(`SELECT s.name FROM projects p JOIN sessions s ON s.id=p.coordinator_session WHERE p.id=?`, pid).Scan(&coordinator); err != nil || coordinator != off["coordinator"] {
		t.Fatal(coordinator, err)
	}
	// Model mode once the owner's key passed its test; the domains stay.
	if _, err = h.s.db.Exec(`INSERT INTO dispatcher_model_keys(owner_principal,state,provider_id,base_url,model,ciphertext,nonce,key_fingerprint,config_revision,caps,egress_text_version,egress_confirmed_at,created_by,created_at) VALUES(?,'active','anthropic','https://api.anthropic.com','claude-test','x','y','-key',1,'{"daily_tokens":5000}','v2','2026-01-01T00:00:00Z',?,'2026-01-01T00:00:00Z')`, h.principal(), h.principal()); err != nil {
		t.Fatal(err)
	}
	preview, err = h.switchCall("dispatcher_enable", map[string]any{"expected_revision": 3, "mode": "model"})
	if model, _ := preview["model"].(map[string]any); err != nil || model["issue_text_to_provider"] != true || model["agent_required"] != true || preview["default_domain_created"] != false {
		t.Fatal(preview, err)
	}
	if on, err = h.switchCall("dispatcher_enable", map[string]any{"expected_revision": 3, "mode": "model", "confirm": true}); err != nil || on["mode"] != "model" || on["revision"] != int64(4) {
		t.Fatal(on, err)
	}
	if err = h.s.db.QueryRow(`SELECT status FROM sessions WHERE id=?`, sessionID).Scan(&status); err != nil || status != "active" {
		t.Fatal("the same session was not reactivated", status, err)
	}
}
