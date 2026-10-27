package server

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

const caseIssue = "aaaa1111-1111-4111-8111-111111111111"

// newCaseEnv serves a local store whose owner has an active model key, a
// project in model mode with one pending claim, and a project token from
// the agent's renew. It returns the env, the token, the project ID and the
// projects the server was asked to nudge.
func newCaseEnv(t *testing.T) (*modelEnv, string, string, func() []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bus.db")
	store, err := bus.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	seal, _ := bus.NewModelSeal(bytes.Repeat([]byte{4}, 32), nil)
	store.SetModelSeal(seal)
	ctx := context.Background()
	var instance string
	for _, r := range []bus.Request{
		{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}},
		{Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "p"}},
		{Operation: "dispatcher_model_setup", Workspace: "w", Project: "p", Args: map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-test", "key": modelTestKey, "caps": map[string]any{"daily_tokens": 500000}, "egress_text_version": "v2", "egress_confirmed": true}},
		{Operation: "register_session", Workspace: "w", Project: "p", Session: "eng", Args: map[string]any{"instance": "eng-i"}},
	} {
		out, err := store.Execute(ctx, r)
		if err != nil {
			t.Fatal(r.Operation, err)
		}
		if r.Operation == "register_session" {
			instance = out.Data.(map[string]any)["instance"].(string)
		}
	}
	if _, err = store.Execute(ctx, bus.Request{Operation: "assignment_propose", Workspace: "w", Project: "p", Session: "eng", Args: map[string]any{"instance": instance, "issue_uuid": caseIssue, "issue_identifier": "FIX-1"}}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var pid string
	if err = db.QueryRow(`SELECT id FROM projects`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES(?,1,1,'model','{"domains":[]}','2026-01-01T00:00:00Z','x')`, pid); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	nudged := []string{}
	e := &modelEnv{t: t, store: store, dbPath: path}
	if _, e.launcher, err = store.OperatorLauncherTokenCreate(ctx); err != nil {
		t.Fatal(err)
	}
	e.host = httptest.NewServer(NewHandlerWithOptions(store, nil, HandlerOptions{ModelMaxAgents: 3, NudgeProject: func(p string) {
		mu.Lock()
		defer mu.Unlock()
		nudged = append(nudged, p)
	}}))
	t.Cleanup(e.host.Close)
	_, wanted := e.do("GET", LauncherWantedPath, e.launcher, nil)
	ref := wanted["agents"].([]any)[0].(map[string]any)["owner_ref"].(string)
	_, minted := e.do("POST", LauncherCredentialPath, e.launcher, map[string]any{"owner_ref": ref})
	credential := minted["credential"].(string)
	_, cfg := e.do("POST", AgentConfigPath, credential, map[string]any{"instance_id": "i-1"})
	secret := cfg["binding_secret"].(string)
	if status, _ := e.do("POST", AgentTestResultPath, credential, map[string]any{"binding_secret": secret, "config_revision": 1, "result": "active"}); status != 200 {
		t.Fatal(status)
	}
	status, renewed := e.do("POST", AgentRenewPath, credential, map[string]any{"binding_secret": secret})
	projects, _ := renewed["projects"].([]any)
	if status != 200 || len(projects) != 1 {
		t.Fatal(status, renewed)
	}
	token := projects[0].(map[string]any)["token"].(string)
	return e, token, pid, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, nudged...)
	}
}

// openCase opens the claim's decide case as the tick would.
func openCase(t *testing.T, store *bus.Store, pid string) error {
	db := store
	var subject, revision string
	view, err := db.ExecuteDispatcher(context.Background(), pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_cases_due"})
	if err != nil {
		return err
	}
	cases, _ := view.Data.(map[string]any)["cases"].([]any)
	if len(cases) != 1 {
		return fmt.Errorf("cases due: %v", view.Data)
	}
	c := cases[0].(map[string]any)
	subject, revision = c["subject_id"].(string), c["subject_revision"].(string)
	out, err := db.ExecuteDispatcher(context.Background(), pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_case_open", Args: map[string]any{
		"subject_id": subject, "subject_revision": revision, "policy_revision": 1, "permitted": []any{"wait", "escalate"},
		"issue": map[string]any{"id": caseIssue, "state_type": "unknown", "project_id": nil, "labels": []string{}},
	}})
	if err != nil || out.Data.(map[string]any)["opened"] != true {
		return fmt.Errorf("open: %v %v", out.Data, err)
	}
	return nil
}

// The case API over HTTP: hello, the long-poll, a read, an abstain that
// nudges the rules' fallback, the closed case, and the refusals' shapes.
func TestModelCaseEndpoints(t *testing.T) {
	e, token, pid, nudged := newCaseEnv(t)
	status, hello := e.do("GET", ModelHelloPath, token, nil)
	if status != 200 || hello["project_id"] != pid || hello["enabled"] != true || hello["provider"] != "anthropic" || hello["model"] != "claude-test" || hello["deadline_seconds"] != float64(90) {
		t.Fatal(status, hello)
	}
	if status, body := e.do("GET", ModelHelloPath, token, nil, "X-Forwarded-Proto", "http", "X-Forwarded-For", "203.0.113.9"); status != 403 || body["error"] != "unauthorized" {
		t.Fatal("a proxied plain-HTTP hello from elsewhere", status, body)
	}
	// Each endpoint takes its own kind of token.
	if status, _ := e.do("POST", AgentRenewPath, token, map[string]any{"binding_secret": strings.Repeat("A", 43)}); status != 401 {
		t.Fatal("a project token opened agent/renew", status)
	}
	if status, _ := e.do("GET", ModelHelloPath, e.launcher, nil); status != 401 {
		t.Fatal("a launcher token opened hello", status)
	}
	if status, body := e.do("POST", ModelCasesNextPath, token, map[string]any{"wait_seconds": 31}); status != 400 || body["error"] != "invalid_argument" {
		t.Fatal(status, body)
	}
	start := time.Now()
	if status, body := e.do("POST", ModelCasesNextPath, token, map[string]any{"wait_seconds": 1}); status != 200 || body["case"] != nil || time.Since(start) < time.Second {
		t.Fatal(status, body, time.Since(start))
	}
	// A case opened while the agent waits is served within the poll.
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := openCase(t, e.store, pid); err != nil {
			t.Error(err)
		}
	}()
	status, next := e.do("POST", ModelCasesNextPath, token, map[string]any{"wait_seconds": 5})
	c, _ := next["case"].(map[string]any)
	if status != 200 || c == nil || c["kind"] != "decide" || c["untrusted"] != nil {
		t.Fatal(status, next)
	}
	tools := ModelCasesNextPath[:len(ModelCasesNextPath)-len("next")] + c["case_id"].(string) + "/tools/"
	if status, body := e.do("POST", tools+"recommend", token, map[string]any{"action": "wait", "reason_code": "uncertain", "explanation": ""}); status != 400 || body["error"] != "tool_refused" || body["tool"] != "recommend" {
		t.Fatal(status, body)
	}
	if status, body := e.do("POST", tools+"read_issue_facts", token, map[string]any{"issue_id": caseIssue, "extra": true}); status != 400 || body["error"] != "invalid_argument" {
		t.Fatal(status, body)
	}
	if status, body := e.do("POST", tools+"read_issue_facts", token, map[string]any{"issue_id": caseIssue}); status != 200 || body["ok"] != true {
		t.Fatal(status, body)
	}
	if status, body := e.do("POST", "/v1/dispatcher-model/cases/case_00000000000000000000000000000000/tools/list_sessions", token, map[string]any{"active_only": true}); status != 404 || body["error"] != "case_not_found" {
		t.Fatal(status, body)
	}
	usage := map[string]any{"input_tokens": 10, "output_tokens": 2, "tool_calls": 1, "duration_ms": 50, "provider": "anthropic", "model": "claude-test"}
	// Closed as the schema: abstain's three codes, decide's seven actions.
	if status, body := e.do("POST", tools+"abstain", token, map[string]any{"reason_code": "bored", "usage": usage}); status != 400 || body["error"] != "invalid_argument" {
		t.Fatal(status, body)
	}
	if status, body := e.do("POST", tools+"decide", token, map[string]any{"action": "delete", "reason_code": "uncertain", "explanation": "", "usage": usage}); status != 400 || body["error"] != "invalid_argument" {
		t.Fatal(status, body)
	}
	abstain := map[string]any{"reason_code": "uncertain", "usage": usage}
	if status, body := e.do("POST", tools+"abstain", token, abstain); status != 200 || body["ok"] != true {
		t.Fatal(status, body)
	}
	if got := nudged(); len(got) != 1 || got[0] != pid {
		t.Fatal("the abstain didn't nudge the fallback", got)
	}
	if status, body := e.do("POST", tools+"abstain", token, abstain); status != 200 || body["ok"] != true {
		t.Fatal("a retried abstain", status, body)
	}
	if status, body := e.do("POST", tools+"list_sessions", token, map[string]any{"active_only": true}); status != 409 || body["error"] != "case_closed" {
		t.Fatal(status, body)
	}
}

// One cases/next poll per token at a time; a second gets server_busy.
func TestModelCasesOnePoll(t *testing.T) {
	e, token, _, _ := newCaseEnv(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Not e.do: its body log isn't shared across goroutines.
		req, _ := http.NewRequest("POST", e.host.URL+ModelCasesNextPath, strings.NewReader(`{"wait_seconds":2}`))
		req.Header.Set("ClawBridge-Model-API", "1")
		req.Header.Set("Authorization", "Bearer "+token)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
		}
	}()
	time.Sleep(300 * time.Millisecond)
	if status, body := e.do("POST", ModelCasesNextPath, token, map[string]any{"wait_seconds": 0}); status != 503 || body["error"] != "server_busy" || body["retry_after"] != float64(1) {
		t.Fatal(status, body)
	}
	<-done
}

// A project token or agent credential travels only over HTTPS or from this
// machine: a proxied plain-HTTP request from elsewhere is refused before
// the token is looked at.
func TestModelPlainHTTPRefused(t *testing.T) {
	for _, c := range []struct {
		proto, from string
		refused     bool
	}{{"http", "203.0.113.9", true}, {"https", "203.0.113.9", false}, {"http", "", false}} {
		req := httptest.NewRequest("GET", ModelHelloPath, nil)
		req.RemoteAddr = "10.0.0.2:4000"
		req.Header.Set("X-Forwarded-Proto", c.proto)
		if c.from != "" {
			req.Header.Set("X-Forwarded-For", c.from)
		} else {
			req.RemoteAddr = "127.0.0.1:4000"
		}
		if got := plainRemote(req); got != c.refused {
			t.Fatal(c, got)
		}
	}
	direct := httptest.NewRequest("GET", ModelHelloPath, nil)
	direct.RemoteAddr = "198.51.100.7:4000"
	if !plainRemote(direct) {
		t.Fatal("a direct plain-HTTP request from elsewhere passed")
	}
}
