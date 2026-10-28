package integration

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCoordinationMCPToolSchemaAndActorPinning(t *testing.T) {
	a := newNotificationAuthority(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	executor := notificationExecutorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) { return a.execute(r) })
	server := mcpserver.NewServerWithOptions(executor, "w", "p", "worker", "worker-instance", mcpserver.Options{Context: ctx})
	client := notificationMCPConnect(t, ctx, server, make(chan string, 5))
	tools, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 20 {
		t.Fatalf("tool inventory=%d want19", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		if tool.Name == "host-status" || tool.Name == "host-wait" {
			t.Fatal("arbitrary host endpoint exposed through MCP")
		}
		if tool.Name == "notification_status" && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint) {
			t.Fatal("policy read annotations wrong")
		}
		if tool.Name == "set_notification_policy" && (tool.Annotations == nil || tool.Annotations.ReadOnlyHint) {
			t.Fatal("policy mutation advertised readonly")
		}
	}
	for _, args := range []map[string]any{
		{"to": "lead", "body": "bad metadata", "operation_id": "null-meta", "coordination": nil},
		{"to": "lead", "body": "bad metadata", "operation_id": "unknown-meta", "coordination": map[string]any{"issue_id": coordinationIssue, "assignment_revision": "r", "intent": "ready", "human_approval": true}},
		{"policy": "hold", "target_session": "lead"}, {"policy": "hold", "instance": "lead-instance"}, {"policy": "hold", "workspace": "other-workspace"},
	} {
		name := "send"
		if args["policy"] != nil {
			name = "set_notification_policy"
		}
		before := a.snapshot()
		r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err == nil && !r.IsError {
			t.Fatalf("MCP accepted invalid metadata/actor override: %v", args)
		}
		if before != a.snapshot() {
			t.Fatal("invalid MCP request changed persisted authority")
		}
	}
	c := coordinationObject("coordinator-typed-1", "ready")
	r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "send", Arguments: map[string]any{"to": "lead", "body": "typed context not authority", "operation_id": "typed-meta", "coordination": c}})
	if err != nil || r.IsError {
		t.Fatalf("typed send failed: %v %+v", err, r)
	}
	data := object(t, jsonData(t, r.StructuredContent))
	sent := object(t, data["data"])
	if !reflect.DeepEqual(sent["coordination"], c) {
		t.Fatal("MCP metadata lost")
	}
	r, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "set_notification_policy", Arguments: map[string]any{"policy": "hold"}})
	if err != nil || r.IsError {
		t.Fatal("own policy tool failed", err)
	}
	before := a.snapshot()
	r, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "notification_status", Arguments: map[string]any{}})
	if err != nil || r.IsError {
		t.Fatal("policy status failed", err)
	}
	data = object(t, object(t, jsonData(t, r.StructuredContent))["data"])
	if len(data) != 7 || data["policy"] != "hold" || before != a.snapshot() {
		t.Fatal("MCP new status shape/stamp wrong")
	}
}

func TestCoordinationCLIMessageAndPolicyRoundTrip(t *testing.T) {
	p := newProcessFixture(t)
	p.seed()
	for _, name := range []string{"alice", "bob"} {
		p.run(cliActor(name, "register")...)
	}
	c := coordinationObject("opaque-cli-1", "request")
	raw, _ := json.Marshal(c)
	sent := p.run(cliActor("alice", "send", "bob", "CLI participant context", "--operation-id", "cli-coordination", "--coordination", string(raw))...)
	if !reflect.DeepEqual(object(t, sent)["coordination"], c) {
		t.Fatal("CLI metadata lost")
	}
	id := stringField(t, sent, "id")
	replyMeta := coordinationObject("opaque-cli-1", "accept")
	replyMeta["request_id"] = id
	raw, _ = json.Marshal(replyMeta)
	reply := p.run(cliActor("bob", "reply", id, "bounded acknowledgement", "--operation-id", "cli-ack", "--coordination", string(raw))...)
	if !reflect.DeepEqual(object(t, reply)["coordination"], replyMeta) {
		t.Fatal("CLI reply metadata lost")
	}
	for _, policy := range []string{"hold", "refuse", "accept"} {
		setter := object(t, p.run(cliActor("bob", "notification-policy", "--policy", policy)...))
		if len(setter) != 4 || setter["policy"] != policy {
			t.Fatal("CLI setter shape")
		}
		current := object(t, p.run(cliActor("bob", "notification-status")...))
		if len(current) != 7 || current["policy"] != policy {
			t.Fatal("CLI new status shape")
		}
		old := object(t, p.run(cliActor("bob", "inbox-status")...))
		if len(old) != 6 {
			t.Fatal("CLI original status changed")
		}
	}
	p.fail(cliActor("bob", "notification-policy", "--policy", "stop")...)
}
