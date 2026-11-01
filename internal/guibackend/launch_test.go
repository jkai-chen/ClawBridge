package guibackend

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/launchcmd"
)

type launchExec struct{}

func (launchExec) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "whoami" {
		return bus.Result{Data: map[string]any{"principal": "mara@example.test"}}, nil
	}
	return bus.Result{Data: map[string]any{"sessions": []map[string]any{
		{"name": "mine", "instance": "inst_m", "owner_principal": "mara@example.test", "host": "mara-mbp"},
		{"name": "theirs", "instance": "inst_t", "owner_principal": "kofi@example.test", "host": "kofi-pc"},
	}}}, nil
}

func launchServer(ensured *[]string) *Server {
	s := testServer()
	s.Methods["sessions.launchCommand"] = LaunchCommand(launchcmd.Source{Exec: launchExec{}, StateDir: "/st",
		Prepare: func(_ context.Context, agent, session, _, _ string) (launchcmd.Prep, error) {
			*ensured = append(*ensured, agent+"|"+session)
			return launchcmd.Prep{CodexCommand: "/bin/clawbridge", CodexArgs: []string{"mcp", "--session", session}}, nil
		}})
	return s
}

func call(t *testing.T, s *Server, params string) map[string]any {
	t.Helper()
	v := rpc(t, s.Handler(), `{"jsonrpc":"2.0","id":1,"method":"sessions.launchCommand","params":`+params+`}`)
	return v.(map[string]any)
}

func TestLaunchCommandOwnSession(t *testing.T) {
	var ensured []string
	res := call(t, launchServer(&ensured), `{"session":"mine","agent":"claude-code"}`)["result"].(map[string]any)
	if res["line"] != "CLAWBRIDGE_SESSION=mine CLAWBRIDGE_INSTANCE=inst_m claude --mcp-config /st/mcp/mine.json" || res["host"] != "mara-mbp" {
		t.Fatalf("%v", res)
	}
	res = call(t, launchServer(&ensured), `{"session":"mine","agent":"codex"}`)["result"].(map[string]any)
	if res["line"] != `CLAWBRIDGE_SESSION=mine CLAWBRIDGE_INSTANCE=inst_m codex -c 'mcp_servers.clawbridge.command="/bin/clawbridge"' -c 'mcp_servers.clawbridge.args=["mcp", "--session", "mine"]'` {
		t.Fatalf("%v", res)
	}
	// The agent defaults to Claude Code.
	res = call(t, launchServer(&ensured), `{"session":"mine"}`)["result"].(map[string]any)
	if !strings.Contains(res["line"].(string), " claude --mcp-config ") {
		t.Fatalf("%v", res)
	}
}

func TestLaunchCommandRefusesAnotherPrincipalsSession(t *testing.T) {
	var ensured []string
	got := call(t, launchServer(&ensured), `{"session":"theirs","agent":"claude-code"}`)
	e, _ := got["error"].(map[string]any)
	if e == nil || e["message"] != "only kofi@example.test can start this agent" || got["result"] != nil {
		t.Fatalf("%v", got)
	}
	if strings.Contains(mustJSON(got), "CLAWBRIDGE_") || len(ensured) != 0 {
		t.Fatalf("a refusal leaked a line or wrote config: %v %v", got, ensured)
	}
}

func TestLaunchCommandBadParams(t *testing.T) {
	var ensured []string
	for _, p := range []string{`{}`, `{"session":"mine","agent":"gemini"}`, `[1]`, `{"session":""}`} {
		if code := errorCode(call(t, launchServer(&ensured), p)); code != CodeParams {
			t.Errorf("%s: code %v", p, code)
		}
	}
	if len(ensured) != 0 {
		t.Fatal("bad params wrote config")
	}
}

func TestLaunchCommandIsReadClassAndNeedsNoWrites(t *testing.T) {
	var ensured []string
	s := launchServer(&ensured)
	if s.Methods["sessions.launchCommand"].Class != Read || s.AllowWrites {
		t.Fatal("must be a Read method on a server with writes off")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestPushOutboxCountSendsTheCountEachInterval(t *testing.T) {
	s := testServer()
	srv := httptest.NewServer(s.Events())
	t.Cleanup(srv.Close)
	c := connectSSE(t, srv.URL)
	waitFor(t, "a client", func() bool { return s.Connected() == 1 })
	var n atomic.Int64
	n.Store(2)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go PushOutboxCount(ctx, s, func() int { return int(n.Load()) }, 10*time.Millisecond)
	if name, data := c.event(t); name != "outbox.count" || data != `{"waiting":2}` {
		t.Fatalf("%s %s", name, data)
	}
	n.Store(5)
	for i := 0; i < 20; i++ {
		if _, data := c.event(t); data == `{"waiting":5}` {
			return
		}
	}
	t.Fatal("the new count never arrived")
}

func TestPushOutboxCountWithoutAnOutboxDoesNothing(t *testing.T) {
	done := make(chan struct{})
	go func() { PushOutboxCount(context.Background(), testServer(), nil, time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("must return at once without a count source")
	}
}
