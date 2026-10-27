package server

import (
	"context"
	"testing"

	"clawbridge/internal/bus"
)

// Chat over the case API (d6): a decide case's chat tools are refused and
// a chat's decide is; a chat reads the decision log and ends with a reply
// the asker reads back as it was sent, which nudges no fallback.
func TestModelChatCase(t *testing.T) {
	e, token, pid, nudged := newCaseEnv(t)
	if status, _ := e.do("GET", ModelHelloPath, token, nil); status != 200 {
		t.Fatal(status)
	}
	ctx := context.Background()
	asked, err := e.store.Execute(ctx, bus.Request{Operation: "dispatcher_chat_ask", Workspace: "w", Project: "p", Args: map[string]any{"text": "Why did FIX-1 wait?"}})
	if err != nil {
		t.Fatal(err)
	}
	chatID := asked.Data.(map[string]any)["case_id"].(string)
	if err = openCase(t, e.store, pid); err != nil {
		t.Fatal(err)
	}
	usage := map[string]any{"input_tokens": 10, "output_tokens": 2, "tool_calls": 1, "duration_ms": 50, "provider": "anthropic", "model": "claude-test"}
	// The decide case first, though the chat is older.
	status, next := e.do("POST", ModelCasesNextPath, token, map[string]any{"wait_seconds": 0})
	c, _ := next["case"].(map[string]any)
	if status != 200 || c == nil || c["kind"] != "decide" {
		t.Fatal(status, next)
	}
	tools := modelCasesPrefix + c["case_id"].(string) + "/tools/"
	for tool, body := range map[string]map[string]any{"reply": {"text": "hi", "usage": usage}, "list_decisions": {"since_hours": 1, "limit": 5}} {
		if status, out := e.do("POST", tools+tool, token, body); status != 400 || out["error"] != "tool_refused" {
			t.Fatal(tool, "in a decide case:", status, out)
		}
	}
	if status, out := e.do("POST", tools+"abstain", token, map[string]any{"reason_code": "uncertain", "usage": usage}); status != 200 || out["ok"] != true {
		t.Fatal(status, out)
	}
	status, next = e.do("POST", ModelCasesNextPath, token, map[string]any{"wait_seconds": 0})
	c, _ = next["case"].(map[string]any)
	message, _ := c["message"].(map[string]any)
	if status != 200 || c["case_id"] != chatID || c["kind"] != "chat" || message["text"] != "Why did FIX-1 wait?" {
		t.Fatal(status, next)
	}
	tools = modelCasesPrefix + chatID + "/tools/"
	if status, out := e.do("POST", tools+"decide", token, map[string]any{"action": "wait", "reason_code": "uncertain", "explanation": "", "usage": usage}); status != 400 || out["error"] != "tool_refused" {
		t.Fatal("decide in a chat:", status, out)
	}
	if status, out := e.do("POST", tools+"list_decisions", token, map[string]any{"since_hours": 1, "limit": 51}); status != 400 || out["error"] != "invalid_argument" {
		t.Fatal(status, out)
	}
	status, out := e.do("POST", tools+"list_decisions", token, map[string]any{"since_hours": 1, "limit": 5})
	if list, ok := out["result"].([]any); status != 200 || !ok || len(list) != 0 {
		t.Fatal("the log, as an array:", status, out)
	}
	if status, out := e.do("POST", tools+"reply", token, map[string]any{"text": "", "usage": usage}); status != 400 || out["error"] != "invalid_argument" {
		t.Fatal("an empty reply:", status, out)
	}
	reply := map[string]any{"text": "It waited.\x1b]52;c;ZXZpbA==\x07 \u202edone", "usage": usage}
	if status, out := e.do("POST", tools+"reply", token, reply); status != 200 || out["ok"] != true || len(out["result"].(map[string]any)) != 0 {
		t.Fatal(status, out)
	}
	if status, out := e.do("POST", tools+"reply", token, reply); status != 200 || out["ok"] != true {
		t.Fatal("a retried reply:", status, out)
	}
	if status, out := e.do("POST", tools+"reply", token, map[string]any{"text": "other", "usage": usage}); status != 409 || out["error"] != "case_closed" {
		t.Fatal("a different reply:", status, out)
	}
	if got := nudged(); len(got) != 1 {
		t.Fatal("only the decide abstain nudges the rules:", got)
	}
	shown, err := e.store.Execute(ctx, bus.Request{Operation: "dispatcher_chat_show", Workspace: "w", Project: "p", Args: map[string]any{}})
	if v := shown.Data.(map[string]any); err != nil || v["state"] != "replied" || v["reply"] != reply["text"] {
		t.Fatal(shown.Data, err)
	}
}
