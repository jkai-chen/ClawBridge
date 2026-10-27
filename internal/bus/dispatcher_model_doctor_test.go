package bus

import (
	"bytes"
	"testing"
	"time"
)

// M2: doctor's model fields, for a project owner and never through a
// session: the seal keys as set, each key-holder's key, test failure,
// agent, provider_auth, launcher report and credential flag, agentd's
// heartbeat and token expiry, and the live agents against the cap with
// the host's memory. None of it carries the key.
func TestModelDoctor(t *testing.T) {
	h := newModelHarness(t)
	h.s.SetModelHost(2, func() (uint64, bool) { return 100 << 20, true })
	doctor := func() map[string]any {
		t.Helper()
		d := h.call("dispatcher_model_doctor", "", nil).(map[string]any)
		if leaks(mustJSON(t, d)) {
			t.Fatal("the key leaked:", d)
		}
		return d
	}
	holder := func(d map[string]any) map[string]any {
		t.Helper()
		hs := d["holders"].([]any)
		if len(hs) != 1 {
			t.Fatal("holders:", hs)
		}
		return hs[0].(map[string]any)
	}
	d := doctor()
	l := d["launcher"].(map[string]any)
	if d["seal_configured"] != true || d["seal_previous"] != false || d["model_on"] != false || len(d["holders"].([]any)) != 0 || l["tokens"] != 1 || l["last_used_age"] != nil || l["reporting"] != false {
		t.Fatal("before setup:", d)
	}
	h.fail("dispatcher_model_doctor", "a", nil)
	// The project the requests name, in model mode.
	var pid string
	if err := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.db.Exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES(?,1,1,'model','{}','2026-01-01T00:00:00Z','x')`, pid); err != nil {
		t.Fatal(err)
	}
	h.setup(nil)
	if g := holder(doctor()); g["key"] != "pending_test" || g["pending_revision"] != 1 || g["you"] != true || g["agent"] != "offline" || g["last_seen_age"] != nil {
		t.Fatal("pending:", g)
	}
	cred, cfg := h.start()
	if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 1, "active", ""); err != nil {
		t.Fatal(err)
	}
	var token string
	for _, p := range cfg.Projects {
		if p.ProjectID == pid {
			token = p.Token
		}
	}
	if _, err := h.s.ModelAuth(h.ctx, token); err != nil {
		t.Fatal(token, err)
	}
	d = doctor()
	g, l, c := holder(d), d["launcher"].(map[string]any), d["capacity"].(map[string]any)
	if d["model_on"] != true || g["key"] != "active" || g["config_revision"] != 1 || g["agent"] != "running" || g["test_failure"] != nil || g["launcher_report"] != nil || g["credential_flag"] != nil {
		t.Fatal("running:", d)
	}
	if l["reporting"] != true || l["expiring"] != false || l["expires_in"].(int64) < int64(89*24*3600) {
		t.Fatal("launcher:", l)
	}
	if c["live_agents"] != 1 || c["max_agents"] != 2 || c["mem_available"] != uint64(100<<20) || c["agent_memory"] != ModelAgentMemory {
		t.Fatal("capacity:", c)
	}
	// The key stopped working; agentd saw a crash loop; the credential
	// was presented again; a newer setup's test failed.
	if err := h.s.AgentStatus(h.ctx, cred, cfg.BindingSecret, "provider_auth"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.LauncherReport(h.ctx, h.launcher, h.wanted()[0].OwnerRef, "agent_crashloop"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.RedeemAgentCredential(h.ctx, cred, "again", "10.0.0.6"); err == nil {
		t.Fatal("redeemed twice")
	}
	if _, err := h.s.db.Exec(`UPDATE dispatcher_agent_status SET test_revision=2,test_failure='quota'`); err != nil {
		t.Fatal(err)
	}
	if g = holder(doctor()); g["agent_status"] != "provider_auth" || g["launcher_report"] != "agent_crashloop" || g["credential_flag"] != "second_redemption" || g["test_failure"] != "quota" || g["test_revision"] != 2 {
		t.Fatal("problems:", g)
	}
	// Old news is not shown: a report an hour old, a failure no newer than
	// the active key, a flag a week old.
	old := func(d time.Duration) string { return layout(time.Now().UTC().Add(-d)) }
	if _, err := h.s.db.Exec(`UPDATE dispatcher_agent_status SET report_at=?,test_revision=1`, old(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.db.Exec(`UPDATE dispatcher_agent_credentials SET created_at=?`, old(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if g = holder(doctor()); g["launcher_report"] != nil || g["test_failure"] != nil || g["credential_flag"] != nil {
		t.Fatal("old news:", g)
	}
	// agentd quiet, and its token near its end.
	if _, err := h.s.db.Exec(`UPDATE dispatcher_launcher_tokens SET created_at=?,last_used_at=?`, old(80*24*time.Hour), old(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	l = doctor()["launcher"].(map[string]any)
	if l["reporting"] != false || l["expiring"] != true || l["last_used_age"].(int64) < 590 || l["expires_in"].(int64) > int64(10*24*3600) {
		t.Fatal("quiet launcher:", l)
	}
	// An expired token is no live token.
	if _, err := h.s.db.Exec(`UPDATE dispatcher_launcher_tokens SET created_at=?`, old(91*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if l = doctor()["launcher"].(map[string]any); l["tokens"] != 0 || l["last_used_age"] != nil {
		t.Fatal("expired launcher:", l)
	}
	// A rotation under way, and no seal key at all.
	seal, err := NewModelSeal(bytes.Repeat([]byte{8}, 32), testSealKey)
	if err != nil {
		t.Fatal(err)
	}
	h.s.SetModelSeal(seal)
	if d = doctor(); d["seal_configured"] != true || d["seal_previous"] != true {
		t.Fatal("rotation:", d)
	}
	h.s.SetModelSeal(nil)
	if d = doctor(); d["seal_configured"] != false || d["seal_previous"] != false {
		t.Fatal("no seal:", d)
	}
}
