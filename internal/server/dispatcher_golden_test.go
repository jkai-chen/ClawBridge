package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// The shared contract test's server half: api/dispatcher-model/golden holds
// the server's answers as it sends them, which the agent's CI checks
// against its schema. This test drives the real handlers, normalizes what
// changes from run to run, and fails on any drift; -update rewrites the
// files and index.json.
var updateGolden = flag.Bool("update", false, "rewrite api/dispatcher-model/golden from the server's answers")

const goldenDir = "../../api/dispatcher-model/golden"

// goldenIndex is each golden file's schema in the agent's contract;
// "agentd" marks the launcher's, which agentd checks.
var goldenIndex = map[string]string{
	"agent-config.json": "AgentConfigResponse", "agent-renew.json": "AgentRenewResponse", "ok.json": "OkResponse", "hello.json": "HelloResponse",
	"cases-next-case.json": "CasesNextResponse", "cases-next-case-no-text.json": "CasesNextResponse", "cases-next-null.json": "CasesNextResponse",
	"tool-read_issue_facts.json": "ToolResponse", "tool-list_domain.json": "ToolResponse", "tool-list_sessions.json": "ToolResponse", "tool-session_activity.json": "ToolResponse",
	"tool-decide.json": "ToolResponse:DecideResult", "tool-decide-superseded.json": "ToolResponse:DecideResult", "tool-abstain.json": "ToolResponse",
	"cases-next-chat.json": "CasesNextResponse", "tool-list_decisions.json": "ToolResponse", "tool-reply.json": "ToolResponse",
	"launcher-wanted.json": "agentd", "launcher-credential.json": "agentd", "launcher-report.json": "agentd",
}

func init() {
	for _, code := range []string{"unauthorized", "rate_limited", "unsupported_version", "agent_cap", "server_busy", "tool_refused", "invalid_argument", "case_not_found", "case_closed", "case_leased", "deadline_passed", "not_in_case", "cap_reached"} {
		goldenIndex["error-"+code+".json"] = "ErrorResponse"
	}
}

var (
	goldenToken = regexp.MustCompile(`^(cbm|cba|cbl)_[A-Za-z0-9_-]{43}$`)
	goldenID    = regexp.MustCompile(`^([a-z]+)_[0-9a-f]{32}$`)
	goldenRef   = regexp.MustCompile(`^p_[a-z0-9]{16}$`)
	goldenTime  = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)$`)
	// goldenIDs are the fixed IDs a normalized answer carries, by prefix.
	goldenIDs = map[string]string{"prj": "0b9f6c5e3a4d4e8b9c1f2d3e4f5a6b7c", "case": "0123456789abcdef0123456789abcdef", "asg": strings.Repeat("c", 32), "msg": strings.Repeat("f", 32)}
)

// goldenNormal replaces what changes from run to run with fixed values:
// tokens and secrets, times, IDs, principal refs, the key and the time left.
func goldenNormal(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, item := range x {
			out[k] = goldenNormal(k, item)
		}
		return out
	case []any:
		out := []any{}
		for _, item := range x {
			out = append(out, goldenNormal(key, item))
		}
		return out
	case float64:
		if key == "deadline_ms" {
			return 90000
		}
	case string:
		switch {
		case key == "key":
			return "sk-test-not-a-real-key"
		case key == "binding_secret":
			return strings.Repeat("x", 43)
		case goldenToken.MatchString(x):
			return x[:4] + strings.Repeat("x", 43)
		case goldenTime.MatchString(x):
			return "2026-01-01T00:00:00Z"
		case key == "owner_ref":
			return "p_dddddddddddddddd"
		case goldenRef.MatchString(x):
			return "p_bbbbbbbbbbbbbbbb"
		case goldenID.MatchString(x):
			prefix := goldenID.FindStringSubmatch(x)[1]
			return prefix + "_" + cmpOr(goldenIDs[prefix], strings.Repeat("a", 32))
		}
	}
	return v
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestModelGolden(t *testing.T) {
	e, token, pid, _ := newCaseEnv(t)
	got := map[string][]byte{}
	keep := func(name string, raw []byte) {
		t.Helper()
		var v any
		if json.Unmarshal(raw, &v) != nil {
			t.Fatal(name, "is not JSON:", string(raw))
		}
		out, _ := json.MarshalIndent(goldenNormal("", v), "", "  ")
		got[name] = append(out, '\n')
	}
	call := func(name string, want int, method, path, token string, body any, header ...string) map[string]any {
		t.Helper()
		status, out := e.do(method, path, token, body, header...)
		if status != want {
			t.Fatal(name, status, out)
		}
		keep(name, e.last(path))
		return out
	}
	// The launcher's and the agent's answers, from newCaseEnv's start.
	for name, path := range map[string]string{"launcher-wanted.json": LauncherWantedPath, "launcher-credential.json": LauncherCredentialPath, "agent-config.json": AgentConfigPath, "ok.json": AgentTestResultPath, "agent-renew.json": AgentRenewPath} {
		keep(name, e.last(path))
	}
	_, wanted := e.do("GET", LauncherWantedPath, e.launcher, nil)
	ref := wanted["agents"].([]any)[0].(map[string]any)["owner_ref"].(string)
	call("launcher-report.json", 200, "POST", LauncherReportPath, e.launcher, map[string]any{"owner_ref": ref, "code": "agent_crashloop"})
	call("hello.json", 200, "GET", ModelHelloPath, token, nil)
	call("error-unauthorized.json", 401, "GET", ModelHelloPath, "cbm_"+strings.Repeat("q", 43), nil)
	call("error-unsupported_version.json", 400, "GET", ModelHelloPath, token, nil, "ClawBridge-Model-API", "")
	call("cases-next-null.json", 200, "POST", ModelCasesNextPath, token, map[string]any{"wait_seconds": 0})

	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(q, err)
		}
	}
	exec(`UPDATE dispatcher_policies SET domains=? WHERE project_id=?`, `{"domains":[{"id":"payments","name":"Payments","match":{"linear_projects":[],"labels":["payments"]},"owner_principal":"owner@example.test","trusted_sessions":[],"stale_after_hours":24}]}`, pid)
	// open opens a decide case for session's claim on issue, as the tick
	// would, with the issue's untrusted text when given.
	open := func(session, issue string, untrusted map[string]any) (subject string) {
		t.Helper()
		ctx := context.Background()
		reg, err := e.store.Execute(ctx, bus.Request{Operation: "register_session", Workspace: "w", Project: "p", Session: session, Args: map[string]any{"instance": session + "-i"}})
		if err != nil {
			t.Fatal(err)
		}
		instance := reg.Data.(map[string]any)["instance"].(string)
		if _, err = e.store.Execute(ctx, bus.Request{Operation: "assignment_propose", Workspace: "w", Project: "p", Session: session, Args: map[string]any{"instance": instance, "issue_uuid": issue, "issue_identifier": "SYN-12"}}); err != nil {
			t.Fatal(err)
		}
		var revision string
		if err = db.QueryRow(`SELECT id,revision FROM session_assignments WHERE issue_uuid=? AND state='proposed'`, issue).Scan(&subject, &revision); err != nil {
			t.Fatal(err)
		}
		a := map[string]any{
			"subject_id": subject, "subject_revision": revision, "policy_revision": 1, "permitted": []any{"wait", "escalate"},
			"issue":          map[string]any{"id": issue, "state_type": "started", "project_id": nil, "labels": []string{"payments"}, "authoritative": true},
			"domain_matches": []any{map[string]any{"domain": "payments", "by": "label"}},
		}
		if untrusted != nil {
			a["untrusted"] = untrusted
		}
		if out, err := e.store.ExecuteDispatcher(ctx, pid, bus.DispatcherSessionName, bus.Request{Operation: "dispatcher_case_open", Args: a}); err != nil || out.Data.(map[string]any)["opened"] != true {
			t.Fatal("open:", out.Data, err)
		}
		return subject
	}
	// next takes the open case, kept as name unless name is "".
	next := func(name string) (map[string]any, string) {
		t.Helper()
		status, out := e.do("POST", ModelCasesNextPath, token, map[string]any{"wait_seconds": 0})
		c, _ := out["case"].(map[string]any)
		if status != 200 || c == nil {
			t.Fatal(name, "served no case:", status, out)
		}
		if name != "" {
			keep(name, e.last(ModelCasesNextPath))
		}
		return c, modelCasesPrefix + c["case_id"].(string) + "/tools/"
	}
	usage := map[string]any{"input_tokens": 10, "output_tokens": 2, "tool_calls": 1, "duration_ms": 50, "provider": "anthropic", "model": "claude-test"}

	// A case without the issue's text: each read, the refusals, a decide.
	first := "5f0c2b8e-7a1d-4c3e-9b2a-6d4e8f1a2b3c"
	open("worker-1", first, nil)
	c, tools := next("cases-next-case-no-text.json")
	call("tool-read_issue_facts.json", 200, "POST", tools+"read_issue_facts", token, map[string]any{"issue_id": first})
	call("tool-list_domain.json", 200, "POST", tools+"list_domain", token, map[string]any{"domain": "payments"})
	call("tool-list_sessions.json", 200, "POST", tools+"list_sessions", token, map[string]any{"active_only": true})
	session := c["facts"].(map[string]any)["session"].(map[string]any)["id"].(string)
	call("tool-session_activity.json", 200, "POST", tools+"session_activity", token, map[string]any{"session_id": session, "since_hours": 24})
	call("error-not_in_case.json", 400, "POST", tools+"list_domain", token, map[string]any{"domain": "elsewhere"})
	call("error-tool_refused.json", 400, "POST", tools+"recommend", token, map[string]any{"action": "wait", "reason_code": "uncertain", "explanation": ""})
	call("error-invalid_argument.json", 400, "POST", tools+"read_issue_facts", token, map[string]any{"issue_id": first, "extra": true})
	call("error-case_not_found.json", 404, "POST", modelCasesPrefix+"case_"+strings.Repeat("0", 32)+"/tools/list_sessions", token, map[string]any{"active_only": true})
	call("tool-decide.json", 200, "POST", tools+"decide", token, map[string]any{"action": "wait", "reason_code": "uncertain", "explanation": "The session was just seen.", "usage": usage})
	call("error-case_closed.json", 409, "POST", tools+"list_sessions", token, map[string]any{"active_only": true})

	// A case with the issue's text: reads to the cap, then an abstain.
	open("worker-2", "6a1d2c3b-4e5f-4a6b-8c7d-9e0f1a2b3c4d", map[string]any{"title": "Fix the ledger", "description_excerpt": "Ledger rounding is off.", "truncated": false})
	_, tools = next("cases-next-case.json")
	for range 8 {
		if status, out := e.do("POST", tools+"list_sessions", token, map[string]any{"active_only": true}); status != 200 {
			t.Fatal(status, out)
		}
	}
	call("error-cap_reached.json", 429, "POST", tools+"list_sessions", token, map[string]any{"active_only": true})
	call("tool-abstain.json", 200, "POST", tools+"abstain", token, map[string]any{"reason_code": "uncertain", "usage": usage})

	// A case another instance holds, then one past its deadline.
	open("worker-3", "7b2e3d4c-5f6a-4b7c-9d8e-0f1a2b3c4d5e", nil)
	c, tools = next("")
	var holder string
	if err = db.QueryRow(`SELECT lease_instance FROM dispatcher_cases WHERE id=?`, c["case_id"]).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE dispatcher_cases SET lease_instance='another' WHERE id=?`, c["case_id"])
	call("error-case_leased.json", 409, "POST", tools+"list_sessions", token, map[string]any{"active_only": true})
	exec(`UPDATE dispatcher_cases SET lease_instance=?,deadline='2000-01-01T00:00:00.000000000Z' WHERE id=?`, holder, c["case_id"])
	call("error-deadline_passed.json", 409, "POST", tools+"list_sessions", token, map[string]any{"active_only": true})

	// A decide on a claim that changed meanwhile: superseded.
	subject := open("worker-4", "8c3f4e5d-6a7b-4c8d-8e9f-1a2b3c4d5e6f", nil)
	_, tools = next("")
	exec(`UPDATE session_assignments SET state='released' WHERE id=?`, subject)
	call("tool-decide-superseded.json", 200, "POST", tools+"decide", token, map[string]any{"action": "wait", "reason_code": "uncertain", "explanation": "", "usage": usage})

	// A chat: the question, the decision log, a reply.
	if _, err = e.store.Execute(context.Background(), bus.Request{Operation: "dispatcher_chat_ask", Workspace: "w", Project: "p", Args: map[string]any{"text": "Why did worker-1 wait?"}}); err != nil {
		t.Fatal(err)
	}
	_, tools = next("cases-next-chat.json")
	call("tool-list_decisions.json", 200, "POST", tools+"list_decisions", token, map[string]any{"since_hours": 1, "limit": 5})
	call("tool-reply.json", 200, "POST", tools+"reply", token, map[string]any{"text": "It waited: the session was just seen.", "usage": usage})

	// A second poll while one waits.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.send("POST", ModelCasesNextPath, token, map[string]any{"wait_seconds": 2}) }()
	time.Sleep(300 * time.Millisecond)
	call("error-server_busy.json", 503, "POST", ModelCasesNextPath, token, map[string]any{"wait_seconds": 0})
	wg.Wait()

	// Repeated failures from one address are limited.
	bad := "cbl_" + strings.Repeat("z", 43)
	for range modelAuthFailures {
		e.do("GET", LauncherWantedPath, bad, nil)
	}
	call("error-rate_limited.json", 429, "GET", LauncherWantedPath, bad, nil)
	// agent_cap needs a second owner's agent, which a local store can't
	// have: it is the same writer's answer.
	rec := httptest.NewRecorder()
	modelFail(rec, 503, "agent_cap")
	keep("error-agent_cap.json", rec.Body.Bytes())

	index, _ := json.MarshalIndent(goldenIndex, "", "  ")
	got["index.json"] = append(index, '\n')
	if *updateGolden {
		for name, raw := range got {
			if err := os.WriteFile(filepath.Join(goldenDir, name), raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Both ways: every file emitted is indexed and on disk as emitted, and
	// every file on disk was emitted.
	for name := range goldenIndex {
		if got[name] == nil {
			t.Error("not emitted:", name)
		}
	}
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatal(err)
	}
	onDisk := []string{}
	for _, entry := range entries {
		onDisk = append(onDisk, entry.Name())
		if _, ok := got[entry.Name()]; !ok {
			t.Error("on disk, not emitted:", entry.Name())
		}
	}
	for name, raw := range got {
		if !slices.Contains(onDisk, name) {
			t.Error("missing on disk:", name, "(run with -update)")
			continue
		}
		disk, _ := os.ReadFile(filepath.Join(goldenDir, name))
		if !bytes.Equal(disk, raw) {
			t.Errorf("%s drifted from the server's answer (run go test ./internal/server -run TestModelGolden -update):\n%s", name, raw)
		}
	}
}
