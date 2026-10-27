package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

const modelTestKey = "sk-test-0123456789abcdefghijKLMNOPQRSTUVwxyz"

// modelLeaks reports whether text holds any 8-character run of the key.
func modelLeaks(text string) bool {
	for i := 0; i+8 <= len(modelTestKey); i++ {
		if strings.Contains(text, modelTestKey[i:i+8]) {
			return true
		}
	}
	return false
}

type modelEnv struct {
	t        *testing.T
	store    *bus.Store
	host     *httptest.Server
	launcher string
	// bodies is every response body but agent/config's.
	bodies strings.Builder
	// seen is each path's last response body, for the golden files.
	seen map[string][]byte
	// mu guards bodies and seen.
	mu sync.Mutex
	// dbPath is the store's file.
	dbPath string
}

// newModelEnv serves a local store with a model key pending its test and
// one project enabled in model mode.
func newModelEnv(t *testing.T) *modelEnv {
	t.Helper()
	store, err := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	seal, _ := bus.NewModelSeal(bytes.Repeat([]byte{3}, 32), nil)
	store.SetModelSeal(seal)
	ctx := context.Background()
	for _, r := range []bus.Request{
		{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}},
		{Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "p"}},
		{Operation: "dispatcher_model_setup", Workspace: "w", Project: "p", Args: map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-test", "key": modelTestKey, "caps": map[string]any{"daily_tokens": 500000}, "egress_text_version": "v2", "egress_confirmed": true}},
	} {
		if _, err = store.Execute(ctx, r); err != nil {
			t.Fatal(r.Operation, err)
		}
	}
	e := &modelEnv{t: t, store: store}
	if _, e.launcher, err = store.OperatorLauncherTokenCreate(ctx); err != nil {
		t.Fatal(err)
	}
	e.host = httptest.NewServer(NewHandlerWithOptions(store, nil, HandlerOptions{ModelMaxAgents: 3}))
	t.Cleanup(e.host.Close)
	return e
}

// do sends one request, records its answer, and returns the status and
// decoded body.
func (e *modelEnv) do(method, path, token string, body any, header ...string) (int, map[string]any) {
	e.t.Helper()
	status, raw := e.send(method, path, token, body, header...)
	e.mu.Lock()
	if path != AgentConfigPath {
		e.bodies.Write(raw)
	}
	if e.seen == nil {
		e.seen = map[string][]byte{}
	}
	e.seen[path] = raw
	e.mu.Unlock()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return status, out
}

// last is path's last recorded answer.
func (e *modelEnv) last(path string) []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seen[path]
}

// send sends one request and returns the status and raw body, recording
// nothing: what a second goroutine uses.
func (e *modelEnv) send(method, path, token string, body any, header ...string) (int, []byte) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.host.URL+path, reader)
	req.Header.Set("ClawBridge-Model-API", "1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] == "" {
			req.Header.Del(header[i])
		} else {
			req.Header.Set(header[i], header[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

// The launcher's and the agent's endpoints end to end: wanted, credential,
// one config with the key and binding secret, then renew, test-result,
// status and report with tokens only. No other answer carries the key.
func TestModelEndpointsEndToEnd(t *testing.T) {
	e := newModelEnv(t)
	status, wanted := e.do("GET", LauncherWantedPath, e.launcher, nil)
	agents, _ := wanted["agents"].([]any)
	if status != 200 || len(agents) != 1 || wanted["max_agents"] != float64(3) {
		t.Fatal(status, wanted)
	}
	agent := agents[0].(map[string]any)
	ref := agent["owner_ref"].(string)
	if agent["provider"] != "api.anthropic.com:443" || agent["config_revision"] != float64(1) {
		t.Fatal(agent)
	}
	status, minted := e.do("POST", LauncherCredentialPath, e.launcher, map[string]any{"owner_ref": ref})
	credential, _ := minted["credential"].(string)
	if status != 200 || !strings.HasPrefix(credential, "cba_") || minted["expires_at"] == nil {
		t.Fatal(status, minted)
	}
	if status, _ := e.do("POST", AgentConfigPath, credential, map[string]any{"instance_id": "i-1", "extra": 1}); status != 400 {
		t.Fatal("an unknown field was taken", status)
	}
	status, cfg := e.do("POST", AgentConfigPath, credential, map[string]any{"instance_id": "i-1"})
	provider, _ := cfg["provider"].(map[string]any)
	secret, _ := cfg["binding_secret"].(string)
	if status != 200 || provider["key"] != modelTestKey || cfg["status"] != "pending_test" || len(secret) != 43 || cfg["config_revision"] != float64(1) {
		t.Fatal(status, cfg)
	}
	if status, again := e.do("POST", AgentConfigPath, credential, map[string]any{"instance_id": "i-2"}); status != 401 || again["error"] != "unauthorized" {
		t.Fatal("redeemed twice", status, again)
	}
	if status, renewed := e.do("POST", AgentRenewPath, credential, map[string]any{"binding_secret": secret}); status != 200 || renewed["projects"] == nil {
		t.Fatal(status, renewed)
	}
	if status, body := e.do("POST", AgentRenewPath, credential, map[string]any{"binding_secret": strings.Repeat("B", 43)}); status != 401 || body["error"] != "unauthorized" {
		t.Fatal("renewed with a wrong secret", status, body)
	}
	if status, body := e.do("POST", AgentTestResultPath, credential, map[string]any{"binding_secret": secret, "config_revision": 1, "result": "failed", "reason": "made up"}); status != 400 {
		t.Fatal(status, body)
	}
	if status, body := e.do("POST", AgentTestResultPath, credential, map[string]any{"binding_secret": secret, "config_revision": 2, "result": "active"}); status != 409 || body["error"] != "conflict" {
		t.Fatal(status, body)
	}
	if status, body := e.do("POST", AgentTestResultPath, credential, map[string]any{"binding_secret": secret, "config_revision": 1, "result": "active"}); status != 200 || body["ok"] != true {
		t.Fatal(status, body)
	}
	if status, body := e.do("POST", AgentStatusPath, credential, map[string]any{"binding_secret": secret, "status": "provider_auth"}); status != 200 || body["ok"] != true {
		t.Fatal(status, body)
	}
	if status, body := e.do("POST", LauncherReportPath, e.launcher, map[string]any{"owner_ref": ref, "code": "agent_crashloop"}); status != 200 || body["ok"] != true {
		t.Fatal(status, body)
	}
	if status, body := e.do("POST", LauncherReportPath, e.launcher, map[string]any{"owner_ref": ref, "code": "free text"}); status != 400 {
		t.Fatal(status, body)
	}
	if status, body := e.do("POST", LauncherCredentialPath, e.launcher, map[string]any{"owner_ref": "p_unknownunknown1"}); status != 404 || body["error"] != "not_found" {
		t.Fatal(status, body)
	}
	if modelLeaks(e.bodies.String()) {
		t.Fatal("the key is in an answer other than agent/config")
	}
}

// Every authentication failure is the same 401; the version header, the
// method and the token kind are checked first; failed authentication is
// limited per address.
func TestModelEndpointsRefuse(t *testing.T) {
	e := newModelEnv(t)
	if status, body := e.do("GET", LauncherWantedPath, e.launcher, nil, "ClawBridge-Model-API", ""); status != 400 || body["error"] != "unsupported_version" {
		t.Fatal(status, body)
	}
	if status, _ := e.do("POST", LauncherWantedPath, e.launcher, map[string]any{}); status != 405 {
		t.Fatal(status)
	}
	var unauthorized []string
	for _, c := range []struct{ path, token string }{
		{LauncherWantedPath, ""},
		{LauncherWantedPath, "cbl_" + strings.Repeat("x", 43)},
		{LauncherWantedPath, "cba_" + e.launcher[4:]},
		{AgentConfigPath, e.launcher},
		{AgentRenewPath, "cba_" + strings.Repeat("y", 43)},
	} {
		method, body := "GET", any(nil)
		if c.path != LauncherWantedPath {
			method, body = "POST", map[string]any{"instance_id": "i"}
			if c.path == AgentRenewPath {
				body = map[string]any{"binding_secret": strings.Repeat("C", 43)}
			}
		}
		status, out := e.do(method, c.path, c.token, body)
		raw, _ := json.Marshal(out)
		if status != 401 {
			t.Fatal(c, status, out)
		}
		unauthorized = append(unauthorized, string(raw))
	}
	for _, u := range unauthorized {
		if u != unauthorized[0] {
			t.Fatal("authentication failures differ:", unauthorized)
		}
	}
	bad := "cbl_" + strings.Repeat("z", 43)
	for range modelAuthFailures {
		e.do("GET", LauncherWantedPath, bad, nil)
	}
	if status, body := e.do("GET", LauncherWantedPath, bad, nil); status != 429 || body["error"] != "rate_limited" {
		t.Fatal("not limited after repeated failures:", status, body)
	}
	// A valid token from the locked-out address still gets through (R100-M1).
	if status, body := e.do("GET", LauncherWantedPath, e.launcher, nil); status != 200 {
		t.Fatal("a valid token was locked out:", status, body)
	}
}

// The lockout is 10 failures within a minute, then 429 for 10 minutes
// (§1): failures spread wider never lock out, and a lockout ends on time.
func TestModelLockout(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := &modelLockouts{now: func() time.Time { return now }, keys: map[string]*lockoutState{}}
	for range modelAuthFailures - 1 {
		l.fail("a")
		now = now.Add(5 * time.Second)
	}
	now = now.Add(20 * time.Second)
	l.fail("a")
	if l.locked("a") {
		t.Fatal("locked by failures over more than a minute")
	}
	for range modelAuthFailures - 1 {
		l.fail("a")
	}
	if !l.locked("a") || l.locked("b") {
		t.Fatal("ten failures in a minute did not lock out only their key")
	}
	now = now.Add(modelLockout - time.Second)
	if !l.locked("a") {
		t.Fatal("the lockout ended early")
	}
	now = now.Add(time.Second)
	if l.locked("a") {
		t.Fatal("the lockout did not end")
	}
	// A full table still locks out: new keys share the overflow (R100-L2).
	for i := range modelLockoutKeys {
		l.keys[fmt.Sprint("k", i)] = &lockoutState{start: now}
	}
	for range modelAuthFailures {
		l.fail("new")
	}
	if !l.locked("another") {
		t.Fatal("a full table disabled the lockout")
	}
}

// Only authenticated calls count against a token's rate, so random
// tokens can't fill the table or use up a real one's (R100-L2).
func TestTokenRate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &tokenRate{now: func() time.Time { return now }, perMinute: 2, seen: map[string]window{}}
	if r.over("x") || len(r.seen) != 0 {
		t.Fatal("a check counted")
	}
	r.record("x")
	r.record("x")
	if !r.over("x") || r.over("y") {
		t.Fatal("not over after the limit")
	}
	now = now.Add(time.Minute)
	if r.over("x") {
		t.Fatal("the window did not end")
	}
}

// The seal keys and the agent cap come from the environment; a bad value
// is refused without echoing it.
func TestReadModelSettings(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	other := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if seal, n, err := readModelSettings(env(nil)); err != nil || seal != nil || n != defaultMaxAgents {
		t.Fatal(seal, n, err)
	}
	if seal, n, err := readModelSettings(env(map[string]string{"CLAWBRIDGE_MODEL_KEY_SEAL_KEY": key, "CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS": other, "CLAWBRIDGE_DISPATCHER_MODEL_MAX_AGENTS": "1"})); err != nil || seal == nil || n != 1 {
		t.Fatal(seal, n, err)
	}
	for name, m := range map[string]map[string]string{
		"short key":           {"CLAWBRIDGE_MODEL_KEY_SEAL_KEY": base64.StdEncoding.EncodeToString([]byte("short-secret-value"))},
		"not base64":          {"CLAWBRIDGE_MODEL_KEY_SEAL_KEY": "not base64 secret!"},
		"previous alone":      {"CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS": key},
		"previous is current": {"CLAWBRIDGE_MODEL_KEY_SEAL_KEY": key, "CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS": key},
		"cap zero":            {"CLAWBRIDGE_DISPATCHER_MODEL_MAX_AGENTS": "0"},
		"cap too high":        {"CLAWBRIDGE_DISPATCHER_MODEL_MAX_AGENTS": "21"},
	} {
		_, _, err := readModelSettings(env(m))
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), key) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// fakeLauncherTokens records the operator's token calls.
type fakeLauncherTokens struct {
	created, revoked []string
	revokeErr        error
}

func (f *fakeLauncherTokens) OperatorLauncherTokenCreate(context.Context) (string, string, error) {
	f.created = append(f.created, "lt_1")
	return "lt_1", "cbl_" + strings.Repeat("t", 43), nil
}
func (f *fakeLauncherTokens) OperatorLauncherTokenRevoke(_ context.Context, id string) error {
	f.revoked = append(f.revoked, id)
	return f.revokeErr
}
func (f *fakeLauncherTokens) Close() error { return nil }

// ON-M1: create writes the token only into a new 0600 file and prints its
// ID; --print needs a terminal; every refusal comes before a token exists.
func TestOperatorLauncherToken(t *testing.T) {
	f := &fakeLauncherTokens{}
	saved := openLauncherTokenStore
	t.Cleanup(func() { openLauncherTokenStore = saved })
	openLauncherTokenStore = func(context.Context, string) (launcherTokenStore, error) { return f, nil }
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "postgres://test")
	dir := t.TempDir()
	path := filepath.Join(dir, "agentd.env")
	var out bytes.Buffer
	if err := RunOperator(context.Background(), []string{"launcher-token", "create", "--out", path}, &out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	info, _ := os.Stat(path)
	if err != nil || string(raw) != "CLAWBRIDGE_LAUNCHER_TOKEN=cbl_"+strings.Repeat("t", 43)+"\n" || info.Mode().Perm() != 0o600 {
		t.Fatal(string(raw), info.Mode(), err)
	}
	if strings.Contains(out.String(), "cbl_") || !strings.Contains(out.String(), "lt_1") {
		t.Fatal("the token reached stdout:", out.String())
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"launcher-token", "create", "--out", path}, "must not exist"},
		{[]string{"launcher-token", "create"}, "usage"},
		{[]string{"launcher-token", "create", "--out", filepath.Join(dir, "b"), "--print"}, "usage"},
		{[]string{"launcher-token", "create", "--print"}, "own terminal"},
		{[]string{"launcher-token", "revoke"}, "usage"},
		{[]string{"launcher-token", "list"}, "usage"},
	} {
		out.Reset()
		if err := RunOperator(context.Background(), c.args, &out); err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(out.String(), "cbl_") {
			t.Errorf("%v: %v", c.args, err)
		}
	}
	if len(f.created) != 1 {
		t.Fatal("a refused run minted a token:", f.created)
	}
	saveTerm := stdoutIsTerminal
	t.Cleanup(func() { stdoutIsTerminal = saveTerm })
	stdoutIsTerminal = func(io.Writer) bool { return true }
	out.Reset()
	if err := RunOperator(context.Background(), []string{"launcher-token", "create", "--print"}, &out); err != nil || !strings.Contains(out.String(), "cbl_") {
		t.Fatal(err, out.String())
	}
	if err := RunOperator(context.Background(), []string{"launcher-token", "revoke", "lt_1"}, &out); err != nil || len(f.revoked) != 1 {
		t.Fatal(err, f.revoked)
	}
	f.revokeErr = errors.New("no live launcher token")
	if err := RunOperator(context.Background(), []string{"launcher-token", "revoke", "lt_9"}, &out); err == nil {
		t.Fatal("a failed revoke succeeded")
	}
}

// fakeModelSeal records the operator's rotation.
type fakeModelSeal struct {
	seal                      *bus.ModelSeal
	confirm, deleteUnreadable bool
}

func (f *fakeModelSeal) OperatorModelSealRotate(_ context.Context, seal *bus.ModelSeal, confirm, deleteUnreadable bool) (bus.SealRotation, error) {
	f.seal, f.confirm, f.deleteUnreadable = seal, confirm, deleteUnreadable
	if !confirm {
		return bus.SealRotation{Keys: 4, UnderCurrent: 1, UnderPrevious: 2, Unreadable: 1, NeedPrevious: 2}, nil
	}
	return bus.SealRotation{Keys: 4, Resealed: 3, Deleted: 1}, nil
}
func (f *fakeModelSeal) Close() error { return nil }

// AG-L1: rotate reads the seal keys from the environment only, refuses
// before Postgres without the current one, and prints counts, never a key.
func TestOperatorModelSealKeyRotate(t *testing.T) {
	f := &fakeModelSeal{}
	saved := openModelSealStore
	t.Cleanup(func() { openModelSealStore = saved })
	openModelSealStore = func(context.Context, string) (modelSealStore, error) { return f, nil }
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "postgres://test")
	t.Setenv("CLAWBRIDGE_MODEL_KEY_SEAL_KEY", "")
	t.Setenv("CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS", "")
	current, previous := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	var out bytes.Buffer
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"model-seal-key"}, "usage"},
		{[]string{"model-seal-key", "rotate", current}, "usage"},
		{[]string{"model-seal-key", "rotate"}, "CLAWBRIDGE_MODEL_KEY_SEAL_KEY is not set"},
		{[]string{"model-seal-key", "rotate", "--delete-unreadable"}, "usage"},
	} {
		if err := RunOperator(context.Background(), c.args, &out); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v", c.args, err)
		}
	}
	if f.seal != nil {
		t.Fatal("a refused run reached the store")
	}
	t.Setenv("CLAWBRIDGE_MODEL_KEY_SEAL_KEY", current)
	t.Setenv("CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS", previous)
	if err := RunOperator(context.Background(), []string{"model-seal-key", "rotate"}, &out); err != nil || f.seal == nil || f.confirm || !strings.Contains(out.String(), "Dry run: 4 stored model keys; 1 under") || !strings.Contains(out.String(), "--confirm") {
		t.Fatal("the dry run:", err, out.String())
	}
	out.Reset()
	if err := RunOperator(context.Background(), []string{"model-seal-key", "rotate", "--confirm", "--delete-unreadable"}, &out); err != nil || !f.confirm || !f.deleteUnreadable {
		t.Fatal(err, f)
	}
	if s := out.String(); !strings.Contains(s, "Re-sealed 3") || !strings.Contains(s, "deleted 1") || !strings.Contains(s, "None needs") || strings.Contains(s, current) || strings.Contains(s, previous) {
		t.Fatal(s)
	}
}
