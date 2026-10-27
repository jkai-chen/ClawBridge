package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// testModelKey is a provider key every test checks never leaks.
const testModelKey = "sk-test-0123456789abcdefghijKLMNOPQRSTUVwxyz"

var testSealKey = bytes.Repeat([]byte{7}, 32)

type modelHarness struct {
	*harness
	ctx      context.Context
	launcher string
	pid      string
}

func newModelHarness(t *testing.T) *modelHarness {
	t.Helper()
	h := &modelHarness{harness: newHarness(t), ctx: context.Background()}
	seal, err := NewModelSeal(testSealKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.s.SetModelSeal(seal)
	if _, h.launcher, err = h.s.OperatorLauncherTokenCreate(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err = h.s.db.QueryRow(`SELECT id FROM projects`).Scan(&h.pid); err != nil {
		t.Fatal(err)
	}
	return h
}

func setupArgs() map[string]any {
	return map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-test", "key": testModelKey, "caps": map[string]any{"daily_tokens": 500000}, "egress_text_version": "v2", "egress_confirmed": true}
}

func (h *modelHarness) setup(over map[string]any) map[string]any {
	h.t.Helper()
	a := setupArgs()
	for k, v := range over {
		a[k] = v
	}
	return h.call("dispatcher_model_setup", "", a).(map[string]any)
}

// enableModel turns the project's dispatcher on in model mode.
func (h *modelHarness) enableModel() {
	h.t.Helper()
	if _, err := h.s.db.Exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES(?,1,1,'model','{}','2026-01-01T00:00:00Z','x')`, h.pid); err != nil {
		h.t.Fatal(err)
	}
}

func (h *modelHarness) wanted() []LauncherAgent {
	h.t.Helper()
	w, err := h.s.LauncherWanted(h.ctx, h.launcher)
	if err != nil {
		h.t.Fatal(err)
	}
	return w
}

// start mints and redeems a credential for the only wanted owner.
func (h *modelHarness) start() (string, AgentConfig) {
	h.t.Helper()
	w := h.wanted()
	if len(w) != 1 {
		h.t.Fatal("wanted:", w)
	}
	cred, _, err := h.s.LauncherCredential(h.ctx, h.launcher, w[0].OwnerRef)
	if err != nil {
		h.t.Fatal(err)
	}
	cfg, err := h.s.RedeemAgentCredential(h.ctx, cred, "instance-1", "10.0.0.5")
	if err != nil {
		h.t.Fatal(err)
	}
	return cred, cfg
}

// leaks reports whether text holds any 8-character run of the test key.
func leaks(text string) bool {
	for i := 0; i+8 <= len(testModelKey); i++ {
		if strings.Contains(text, testModelKey[i:i+8]) {
			return true
		}
	}
	return false
}

// dump is every stored row the key could hide in, but the sealed
// ciphertext itself.
func (h *modelHarness) dump() string {
	h.t.Helper()
	var b strings.Builder
	for _, q := range []string{`SELECT details FROM history`, `SELECT provider_id||model||base_url||key_fingerprint||caps FROM dispatcher_model_keys`, `SELECT status||launcher_report||test_failure FROM dispatcher_agent_status`, `SELECT COALESCE(bound_instance,'')||redeemed_from||flagged FROM dispatcher_agent_credentials`} {
		rows, err := h.s.db.Query(q)
		if err != nil {
			h.t.Fatal(q, err)
		}
		for rows.Next() {
			var v string
			if rows.Scan(&v) == nil {
				b.WriteString(v + "\n")
			}
		}
		rows.Close()
	}
	return b.String()
}

// Setup seals the owner's key as pending_test under its owner, provider and
// revision. It never stores, returns or reports the key, a refusal
// included, and needs the seal key.
func TestModelSetupSealsThePendingKey(t *testing.T) {
	h := newModelHarness(t)
	for name, c := range map[string]struct {
		over map[string]any
		want string
	}{
		"no confirmation":   {map[string]any{"egress_confirmed": false}, "egress"},
		"plain http":        {map[string]any{"base_url": "http://api.example.com"}, "base_url"},
		"no daily ceiling":  {map[string]any{"caps": map[string]any{}}, "daily_tokens"},
		"unknown cap":       {map[string]any{"caps": map[string]any{"daily_tokens": 5000, "x": 1}}, "caps fields"},
		"empty key":         {map[string]any{"key": "  "}, "key must be"},
		"oversized key":     {map[string]any{"key": strings.Repeat("k", maxModelKey+1)}, "key must be"},
		"bad provider":      {map[string]any{"provider_id": "Anthropic!"}, "provider_id"},
		"bad model":         {map[string]any{"model": "a b"}, "model"},
		"key in wrong type": {map[string]any{"key": 5}, "key"},
	} {
		a := setupArgs()
		for k, v := range c.over {
			a[k] = v
		}
		e := h.errorOf("dispatcher_model_setup", "", a)
		if !strings.Contains(e, c.want) || leaks(e) {
			t.Errorf("%s: %s", name, e)
		}
	}
	if e := h.errorOf("dispatcher_model_setup", "a", setupArgs()); !strings.Contains(e, "without a session") || leaks(e) {
		t.Fatal(e)
	}
	r := h.setup(nil)
	if r["config_revision"] != 1 || r["test"] != "pending" || leaks(mustJSON(t, r)) {
		t.Fatal(r)
	}
	var ciphertext, nonce, fingerprint string
	if err := h.s.db.QueryRow(`SELECT ciphertext,nonce,key_fingerprint FROM dispatcher_model_keys WHERE state='pending_test'`).Scan(&ciphertext, &nonce, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if leaks(ciphertext) || fingerprint != "wxyz" || leaks(h.dump()) {
		t.Fatal("the key is stored in the clear")
	}
	aad := func(owner, provider, api, baseURL, model string, revision int) []byte {
		return modelAAD(owner, provider, api, baseURL, model, revision)
	}
	const api, base, model = "anthropic-messages", "https://api.anthropic.com", "claude-test"
	if got, err := h.s.modelSeal.open(ciphertext, nonce, aad(h.principal(), "anthropic", api, base, model, 1)); err != nil || string(got) != testModelKey {
		t.Fatal("does not open", err)
	}
	// Moved to another row, or pointed at another host, API or model by
	// someone without the seal key: it doesn't open.
	for _, other := range [][]byte{
		aad("other@example.com", "anthropic", api, base, model, 1), aad(h.principal(), "openai", api, base, model, 1),
		aad(h.principal(), "anthropic", api, base, model, 2), aad(h.principal(), "anthropic", api, "https://evil.example", model, 1),
		aad(h.principal(), "anthropic", "openai-completions", base, model, 1), aad(h.principal(), "anthropic", api, base, "other-model", 1),
	} {
		if _, err := h.s.modelSeal.open(ciphertext, nonce, other); err == nil {
			t.Fatal("opened under another owner, provider, endpoint, model or revision")
		}
	}
	// A second setup replaces the pending one, at the next revision.
	if r := h.setup(nil); r["config_revision"] != 2 {
		t.Fatal(r)
	}
	var n int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM dispatcher_model_keys`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	h.s.SetModelSeal(nil)
	if e := h.errorOf("dispatcher_model_setup", "", setupArgs()); !strings.Contains(e, "unavailable") {
		t.Fatal(e)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The launcher token opens only its own calls, and its use is recorded;
// one answer for every failure.
func TestLauncherTokenAuth(t *testing.T) {
	h := newModelHarness(t)
	for _, token := range []string{"", "cbl_short", "cba_" + h.launcher[4:], h.launcher[:len(h.launcher)-1] + "x"} {
		if _, err := h.s.LauncherWanted(h.ctx, token); !errors.Is(err, ErrModelUnauthorized) {
			t.Fatalf("%q: %v", token, err)
		}
	}
	if w := h.wanted(); len(w) != 0 {
		t.Fatal(w)
	}
	var used string
	if err := h.s.db.QueryRow(`SELECT COALESCE(last_used_at,'') FROM dispatcher_launcher_tokens`).Scan(&used); err != nil || used == "" {
		t.Fatal("the launcher's poll is not recorded", err)
	}
	var id string
	if err := h.s.db.QueryRow(`SELECT id FROM dispatcher_launcher_tokens`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// F5: a token lives 90 days from its creation, then gets the same answer.
	for days, works := range map[int]bool{89: true, 91: false} {
		created := layout(time.Now().Add(-time.Duration(days) * 24 * time.Hour))
		if _, err := h.s.db.Exec(`UPDATE dispatcher_launcher_tokens SET created_at=? WHERE id=?`, created, id); err != nil {
			t.Fatal(err)
		}
		if _, err := h.s.LauncherWanted(h.ctx, h.launcher); (err == nil) != works || err != nil && !errors.Is(err, ErrModelUnauthorized) {
			t.Fatal(days, "days old:", err)
		}
	}
	if err := h.s.OperatorLauncherTokenRevoke(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := h.s.OperatorLauncherTokenRevoke(h.ctx, id); err == nil {
		t.Fatal("revoked twice")
	}
	if _, err := h.s.LauncherWanted(h.ctx, h.launcher); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal("a revoked launcher token still works", err)
	}
}

// A pending key always wants an agent (its test runs there); an active one
// only with a model-enabled project. The provider is an exact host:port,
// and the ref is stable, opaque and never the principal.
func TestLauncherWanted(t *testing.T) {
	h := newModelHarness(t)
	h.setup(map[string]any{"base_url": "https://API.Example.com/v1"})
	w := h.wanted()
	if len(w) != 1 || w[0].Provider != "api.example.com:443" || w[0].ConfigRevision != 1 || !ownerRefPattern.MatchString(w[0].OwnerRef) || strings.Contains(w[0].OwnerRef, "@") {
		t.Fatal(w)
	}
	ref := w[0].OwnerRef
	cred, cfg := h.start()
	if cfg.Status != "pending_test" || len(cfg.Projects) != 0 {
		t.Fatal(cfg.Status, cfg.Projects)
	}
	if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 1, "active", ""); err != nil {
		t.Fatal(err)
	}
	if w := h.wanted(); len(w) != 0 {
		t.Fatal("an active key with no model project wants an agent:", w)
	}
	h.enableModel()
	if w := h.wanted(); len(w) != 1 || w[0].OwnerRef != ref {
		t.Fatal(w)
	}
	// A rotation lists the pending key's provider and revision.
	h.setup(map[string]any{"base_url": "https://[2001:db8::1]:8443"})
	if w := h.wanted(); len(w) != 1 || w[0].Provider != "[2001:db8::1]:8443" || w[0].ConfigRevision != 2 || w[0].OwnerRef != ref {
		t.Fatal(w)
	}
}

// A credential is minted for a wanted ref only, revokes the owner's
// previous one, and is redeemed once, within its 5 minutes.
func TestAgentCredentialIsRedeemedOnce(t *testing.T) {
	h := newModelHarness(t)
	h.enableModel()
	h.setup(nil)
	ref := h.wanted()[0].OwnerRef
	for _, bad := range []string{"p_unknownunknown1", "p_x", "nope"} {
		if _, _, err := h.s.LauncherCredential(h.ctx, h.launcher, bad); err == nil || !strings.Contains(err.Error(), "owner") {
			t.Fatal(bad, err)
		}
	}
	first, expires, err := h.s.LauncherCredential(h.ctx, h.launcher, ref)
	if err != nil || !regexp.MustCompile(`^cba_[A-Za-z0-9_-]{16,256}$`).MatchString(first) || expires.Sub(time.Now()) > agentCredentialTTL+time.Minute {
		t.Fatal(first, expires, err)
	}
	cred, _, err := h.s.LauncherCredential(h.ctx, h.launcher, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.s.RedeemAgentCredential(h.ctx, first, "i-1", "10.0.0.5"); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal("a superseded credential redeemed:", err)
	}
	if _, err = h.s.RedeemAgentCredential(h.ctx, cred, "bad instance!", "10.0.0.5"); err == nil {
		t.Fatal("took an unreadable instance ID")
	}
	cfg, err := h.s.RedeemAgentCredential(h.ctx, cred, "i-2", "10.0.0.5")
	if err != nil || cfg.Provider.Key != testModelKey || cfg.ConfigRevision != 1 || cfg.Status != "pending_test" || !bindingSecretPattern.MatchString(cfg.BindingSecret) || len(cfg.Projects) != 1 || cfg.Projects[0].ProjectID != h.pid || !strings.HasPrefix(cfg.Projects[0].Token, "cbm_") || cfg.Limits.MaxInputTokens != 20000 || cfg.Limits.MaxOutputTokens != 2000 {
		t.Fatal(cfg, err)
	}
	if _, err = h.s.RedeemAgentCredential(h.ctx, cred, "i-3", "10.0.0.6"); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal("redeemed twice:", err)
	}
	var instance, from, flagged string
	if err = h.s.db.QueryRow(`SELECT bound_instance,redeemed_from,flagged FROM dispatcher_agent_credentials WHERE revoked_at IS NULL`).Scan(&instance, &from, &flagged); err != nil || instance != "i-2" || from != "10.0.0.5" || flagged != "second_redemption" {
		t.Fatal(instance, from, flagged, err)
	}
	// An expired credential is refused.
	cred, _, _ = h.s.LauncherCredential(h.ctx, h.launcher, ref)
	if _, err = h.s.db.Exec(`UPDATE dispatcher_agent_credentials SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE revoked_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err = h.s.RedeemAgentCredential(h.ctx, cred, "i-4", "10.0.0.5"); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal("an expired credential redeemed:", err)
	}
	if leaks(h.dump()) {
		t.Fatal("the key leaked into a row")
	}
}

// AG-M2: renew, test-result and status need the binding secret; renew
// returns project tokens only, never the key.
func TestAgentRenewNeedsTheBindingSecret(t *testing.T) {
	h := newModelHarness(t)
	h.enableModel()
	h.setup(nil)
	cred, cfg := h.start()
	tokens, err := h.s.RenewAgentTokens(h.ctx, cred, cfg.BindingSecret)
	if err != nil || len(tokens) != 1 || tokens[0].Token == cfg.Projects[0].Token || leaks(mustJSON(t, tokens)) {
		t.Fatal(tokens, err)
	}
	wrong := strings.Repeat("A", 43)
	for _, secret := range []string{wrong, "", "short"} {
		if _, err = h.s.RenewAgentTokens(h.ctx, cred, secret); !errors.Is(err, ErrModelUnauthorized) {
			t.Fatal(secret, err)
		}
		if err = h.s.AgentStatus(h.ctx, cred, secret, "ok"); !errors.Is(err, ErrModelUnauthorized) {
			t.Fatal(secret, err)
		}
		if err = h.s.AgentTestResult(h.ctx, cred, secret, 1, "active", ""); !errors.Is(err, ErrModelUnauthorized) {
			t.Fatal(secret, err)
		}
	}
	var flagged string
	if err = h.s.db.QueryRow(`SELECT flagged FROM dispatcher_agent_credentials WHERE revoked_at IS NULL`).Scan(&flagged); err != nil || flagged != "wrong_binding" {
		t.Fatal(flagged, err)
	}
	// Renew follows the owner's projects as they are now.
	if _, err = h.s.db.Exec(`UPDATE dispatcher_policies SET enabled=0`); err != nil {
		t.Fatal(err)
	}
	if tokens, err = h.s.RenewAgentTokens(h.ctx, cred, cfg.BindingSecret); err != nil || len(tokens) != 0 {
		t.Fatal(tokens, err)
	}
	if err = h.s.AgentStatus(h.ctx, cred, cfg.BindingSecret, "provider_auth"); err != nil {
		t.Fatal(err)
	}
	if err = h.s.AgentStatus(h.ctx, cred, cfg.BindingSecret, "bad"); err == nil {
		t.Fatal("took an unknown status")
	}
	var status string
	if err = h.s.db.QueryRow(`SELECT status FROM dispatcher_agent_status`).Scan(&status); err != nil || status != "provider_auth" {
		t.Fatal(status, err)
	}
	// An unredeemed credential has no binding: renew is refused.
	h.setup(nil)
	fresh, _, err := h.s.LauncherCredential(h.ctx, h.launcher, h.wanted()[0].OwnerRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.s.RenewAgentTokens(h.ctx, fresh, cfg.BindingSecret); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal("an unredeemed credential renewed:", err)
	}
}

// d4.1: a passing test makes the pending key the active one, deleting the
// old; a failing one deletes it and records the fixed reason. Both are
// idempotent per revision, any other revision is a conflict, and status
// reads them back.
func TestAgentTestResult(t *testing.T) {
	h := newModelHarness(t)
	h.enableModel()
	h.setup(nil)
	cred, cfg := h.start()
	status := func(rev int) any {
		t.Helper()
		return h.call("dispatcher_model_status", "", map[string]any{"config_revision": rev}).(map[string]any)["test"]
	}
	if status(1) != "pending" {
		t.Fatal(status(1))
	}
	for _, bad := range [][2]string{{"failed", ""}, {"failed", "free text"}, {"active", "quota"}, {"maybe", ""}} {
		if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 1, bad[0], bad[1]); err == nil {
			t.Fatal(bad)
		}
	}
	if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 2, "active", ""); !errors.Is(err, ErrModelConflict) {
		t.Fatal("a stale revision passed:", err)
	}
	for range 2 {
		if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 1, "active", ""); err != nil {
			t.Fatal(err)
		}
	}
	if status(1) != "active" {
		t.Fatal(status(1))
	}
	// A rotation that fails leaves the active key as it was.
	h.setup(map[string]any{"model": "claude-other"})
	cred, cfg = h.start()
	if cfg.ConfigRevision != 2 || cfg.Status != "pending_test" || cfg.Provider.Model != "claude-other" {
		t.Fatal(cfg)
	}
	for range 2 {
		if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 2, "failed", "invalid_key"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 2, "active", ""); !errors.Is(err, ErrModelConflict) {
		t.Fatal("a failed test turned active:", err)
	}
	if status(2) != "invalid_key" || status(1) != "active" {
		t.Fatal(status(2), status(1))
	}
	var model string
	if err := h.s.db.QueryRow(`SELECT model FROM dispatcher_model_keys WHERE state='active'`).Scan(&model); err != nil || model != "claude-test" {
		t.Fatal(model, err)
	}
	// A successful rotation deletes the old key.
	h.setup(map[string]any{"model": "claude-third"})
	cred, cfg = h.start()
	if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 3, "active", ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM dispatcher_model_keys`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	// A pending key no agent took up fails as agent_unavailable.
	h.setup(nil)
	if _, err := h.s.db.Exec(`UPDATE dispatcher_model_keys SET created_at='2000-01-01T00:00:00.000000000Z' WHERE state='pending_test'`); err != nil {
		t.Fatal(err)
	}
	if status(4) != "agent_unavailable" || status(3) != "active" {
		t.Fatal(status(4))
	}
	if e := h.errorOf("dispatcher_model_status", "", map[string]any{"config_revision": 9}); !strings.Contains(e, "no model setup") {
		t.Fatal(e)
	}
}

// Key delete removes the sealed keys and revokes the credential and its
// project tokens; the agent is no longer wanted.
func TestModelKeyDeleteRevokesEverything(t *testing.T) {
	h := newModelHarness(t)
	h.enableModel()
	h.setup(nil)
	cred, cfg := h.start()
	if r := h.call("dispatcher_model_key_delete", "", nil).(map[string]any); r["deleted"] != true {
		t.Fatal(r)
	}
	if _, err := h.s.RenewAgentTokens(h.ctx, cred, cfg.BindingSecret); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal("a deleted key's credential still renews", err)
	}
	var live int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM dispatcher_model_tokens WHERE revoked_at IS NULL`).Scan(&live); err != nil || live != 0 {
		t.Fatal(live, err)
	}
	if w := h.wanted(); len(w) != 0 {
		t.Fatal(w)
	}
	if leaks(h.dump()) {
		t.Fatal("the key leaked into a row")
	}
}

// One owner's credential never yields another owner's key or projects
// (A1): the key comes from the credential's owner, under its own AAD.
func TestAgentCredentialIsOwnerBound(t *testing.T) {
	h := newModelHarness(t)
	h.enableModel()
	other := "other@example.com"
	ciphertext, nonce, err := h.s.modelSeal.seal([]byte("sk-other-key"), modelAAD(other, "openai", "openai-responses", "https://api.openai.com", "gpt-test", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.s.db.Exec(`INSERT INTO dispatcher_model_keys(owner_principal,state,provider_id,api,base_url,model,ciphertext,nonce,key_fingerprint,config_revision,caps,egress_text_version,egress_confirmed_at,created_by,created_at) VALUES(?,'pending_test','openai','openai-responses','https://api.openai.com','gpt-test',?,?,'-key',1,'{"daily_tokens":5000}','v2','2026-01-01T00:00:00Z',?,'2999-01-01T00:00:00Z')`, other, ciphertext, nonce, other); err != nil {
		t.Fatal(err)
	}
	h.setup(nil)
	wanted := h.wanted()
	if len(wanted) != 2 {
		t.Fatal(wanted)
	}
	for _, w := range wanted {
		cred, _, err := h.s.LauncherCredential(h.ctx, h.launcher, w.OwnerRef)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := h.s.RedeemAgentCredential(h.ctx, cred, "i", "10.0.0.5")
		if err != nil {
			t.Fatal(err)
		}
		mine := cfg.Provider.ID == "anthropic"
		if mine != (cfg.Provider.Key == testModelKey) || !mine && (cfg.Provider.Key != "sk-other-key" || len(cfg.Projects) != 0) || mine && len(cfg.Projects) != 1 {
			t.Fatal(w, cfg.Provider.ID, len(cfg.Projects))
		}
	}
	// A row moved to another owner doesn't open.
	if _, err = h.s.db.Exec(`UPDATE dispatcher_model_keys SET ciphertext=?,nonce=? WHERE owner_principal<>?`, ciphertext, nonce, other); err != nil {
		t.Fatal(err)
	}
	for _, w := range h.wanted() {
		cred, _, _ := h.s.LauncherCredential(h.ctx, h.launcher, w.OwnerRef)
		if cfg, err := h.s.RedeemAgentCredential(h.ctx, cred, "i", "10.0.0.5"); err == nil && cfg.Provider.ID == "anthropic" {
			t.Fatal("another owner's ciphertext opened")
		}
	}
}

// Rotating the seal key: a row sealed under the previous key still opens
// while both are set.
func TestModelSealOpensUnderThePreviousKey(t *testing.T) {
	old, _ := NewModelSeal(testSealKey, nil)
	aad := modelAAD("o", "p", "a", "https://b.example", "m", 1)
	c, n, err := old.seal([]byte("secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewModelSeal(bytes.Repeat([]byte{9}, 32), testSealKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.open(c, n, aad); err != nil || string(got) != "secret" {
		t.Fatal(got, err)
	}
	fresh, _ := NewModelSeal(bytes.Repeat([]byte{9}, 32), nil)
	if _, err = fresh.open(c, n, aad); err == nil {
		t.Fatal("opened without the previous key")
	}
	if _, err = NewModelSeal([]byte("short"), nil); err == nil {
		t.Fatal("took a short seal key")
	}
}

// The launcher reports fixed codes only, for a known ref.
func TestLauncherReport(t *testing.T) {
	h := newModelHarness(t)
	h.setup(nil)
	ref := h.wanted()[0].OwnerRef
	if err := h.s.LauncherReport(h.ctx, h.launcher, ref, "something else"); err == nil {
		t.Fatal("took a free-text code")
	}
	if err := h.s.LauncherReport(h.ctx, h.launcher, "p_unknownunknown1", "agent_crashloop"); err == nil {
		t.Fatal("took an unknown ref")
	}
	if err := h.s.LauncherReport(h.ctx, "cbl_x", ref, "agent_crashloop"); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal(err)
	}
	if err := h.s.LauncherReport(h.ctx, h.launcher, ref, "agent_crashloop"); err != nil {
		t.Fatal(err)
	}
	var report string
	if err := h.s.db.QueryRow(`SELECT launcher_report FROM dispatcher_agent_status`).Scan(&report); err != nil || report != "agent_crashloop" {
		t.Fatal(report, err)
	}
}

// R100-M2: a pending key no agent took up within setupTestWait fails as
// agent_unavailable in wanted itself, so it holds no agent slot; one an
// agent redeemed for waits until setupTestLimit. Owners with an active
// key are listed first.
func TestPendingKeyExpiresInWanted(t *testing.T) {
	h := newModelHarness(t)
	age := func(d time.Duration) {
		t.Helper()
		if _, err := h.s.db.Exec(`UPDATE dispatcher_model_keys SET created_at=? WHERE state='pending_test'`, layout(time.Now().Add(-d))); err != nil {
			t.Fatal(err)
		}
	}
	h.setup(nil)
	age(setupTestWait + time.Minute)
	if w := h.wanted(); len(w) != 0 {
		t.Fatal("an abandoned setup still wants an agent:", w)
	}
	if r := h.call("dispatcher_model_status", "", map[string]any{"config_revision": 1}).(map[string]any); r["test"] != "agent_unavailable" {
		t.Fatal(r)
	}
	h.setup(nil)
	h.start()
	age(setupTestWait + time.Minute)
	if w := h.wanted(); len(w) != 1 {
		t.Fatal("a pending key under test was dropped:", w)
	}
	if r := h.call("dispatcher_model_status", "", map[string]any{"config_revision": 2}).(map[string]any); r["test"] != "pending" {
		t.Fatal("status expired a claimed key:", r)
	}
	age(setupTestLimit + time.Minute)
	if w := h.wanted(); len(w) != 0 {
		t.Fatal("a test that never answered kept its key:", w)
	}
	// Active owners first.
	h.setup(nil)
	cred, cfg := h.start()
	if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 3, "active", ""); err != nil {
		t.Fatal(err)
	}
	h.enableModel()
	mine := h.wanted()[0].OwnerRef
	other := "aaa-first@example.com"
	if _, err := h.s.db.Exec(`INSERT INTO dispatcher_model_keys(owner_principal,state,provider_id,api,base_url,model,ciphertext,nonce,key_fingerprint,config_revision,caps,egress_text_version,egress_confirmed_at,created_by,created_at) VALUES(?,'pending_test','openai','openai-responses','https://api.openai.com','gpt-test','x','y','-key',1,'{}','v2','2026-01-01T00:00:00Z',?,?)`, other, other, layout(time.Now())); err != nil {
		t.Fatal(err)
	}
	if w := h.wanted(); len(w) != 2 || w[0].OwnerRef != mine {
		t.Fatal("an active owner is not listed first:", w)
	}
}

// R100-L3, L4: a failed test that leaves the owner no key revokes its
// credential, so renew stops; and a credential keeps its first flag.
func TestNoKeyNoRenewAndFirstFlagKept(t *testing.T) {
	h := newModelHarness(t)
	h.enableModel()
	h.setup(nil)
	cred, cfg := h.start()
	if _, err := h.s.RedeemAgentCredential(h.ctx, cred, "again", "10.0.0.5"); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal(err)
	}
	if _, err := h.s.RenewAgentTokens(h.ctx, cred, strings.Repeat("A", 43)); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal(err)
	}
	var flagged string
	if err := h.s.db.QueryRow(`SELECT flagged FROM dispatcher_agent_credentials`).Scan(&flagged); err != nil || flagged != "second_redemption" {
		t.Fatal("the first flag was overwritten:", flagged, err)
	}
	if err := h.s.AgentTestResult(h.ctx, cred, cfg.BindingSecret, 1, "failed", "invalid_key"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.RenewAgentTokens(h.ctx, cred, cfg.BindingSecret); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal("renewed with no key:", err)
	}
}
