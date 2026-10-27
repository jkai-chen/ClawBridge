package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The agent's MCP server end to end against the real server: it holds the
// consume stream's lease, inbox shows it, ack settles it; a hold leases
// nothing, and accept re-offers at once (the policy change wakes the
// stream: the periodic re-read is an hour here); reply ack=true settles in
// the reply's step.
func TestPostgresMCPConsumesAndAcks(t *testing.T) {
	f := newSTFixture(t, StreamLimits{Reread: time.Hour}, bus.QueueLimits{})
	client := streamClientFor(t, f.host.URL, f.member)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := filepath.Join(t.TempDir(), "context")
	server := mcpserver.NewServerWithOptions(client, f.workspace, "p", "m", "m-inst", mcpserver.Options{Context: ctx, ContextDir: dir, Server: f.host.URL})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || r.IsError {
			t.Fatalf("%s: %v %+v", name, err, r)
		}
		var out map[string]any
		raw, _ := json.Marshal(r.StructuredContent)
		_ = json.Unmarshal(raw, &out)
		return out["data"].(map[string]any)
	}
	held := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			r, ok, _ := ctxwatch.LoadQueueRecord(dir, "m")
			if ok && r.Stream == "stream" && len(r.Leases) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %+v", what, r)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	unacked := func() float64 {
		t.Helper()
		data := f.session("m", bus.Request{Operation: "notification_status", Args: map[string]any{"queue": true}}).(map[string]any)
		return float64(data["unacked_count"].(int64))
	}

	// The agent's first inbox call opens the consume stream.
	call("inbox", map[string]any{})
	first := f.send("m", "first")
	held(1, "the stream's offer wasn't held")
	messages := call("inbox", map[string]any{})["messages"].([]any)
	m := messages[0].(map[string]any)
	lease, _ := m["lease"].(map[string]any)
	if m["id"] != first || lease["lease_id"] == nil || lease["attempt"] != float64(1) {
		t.Fatal(m)
	}
	acks := call("ack", map[string]any{"acks": []any{map[string]any{"message_id": first}}, "operation_id": "msg:" + first + ":done"})
	if fmt.Sprint(acks["acks"]) != fmt.Sprintf("[map[message_id:%s outcome:acked]]", first) || unacked() != 0 {
		t.Fatal(acks)
	}
	held(0, "an acked lease stayed held")

	call("set_notification_policy", map[string]any{"policy": "hold"})
	second := f.send("m", "second")
	time.Sleep(500 * time.Millisecond)
	held(0, "a held session was leased")
	start := time.Now()
	call("set_notification_policy", map[string]any{"policy": "accept"})
	held(1, "accept didn't re-offer")
	if time.Since(start) > 3*time.Second {
		t.Fatal("accept waited for the re-read")
	}
	reply := call("reply", map[string]any{"message_id": second, "body": "done", "operation_id": "msg:" + second + ":reply", "ack": true})
	if a, _ := reply["ack"].(map[string]any); a["message_id"] != second || a["outcome"] != "acked" || unacked() != 0 {
		t.Fatal(reply)
	}
	held(0, "a reply's ack left the lease held")
}
