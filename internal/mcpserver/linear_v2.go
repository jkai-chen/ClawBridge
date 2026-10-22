package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TaskboardToolsV2 is optional; old injected adapters retain their seven tools.
// Native route, profile and actor selection remain process-owned.
type TaskboardToolsV2 interface {
	ExecuteV2(context.Context, bus.Request, string, map[string]any) (any, error)
}

func boardSchemaV2(operation string) map[string]any {
	uuid := func(nullable bool) map[string]any {
		var kind any = "string"
		if nullable {
			kind = []string{"string", "null"}
		}
		return map[string]any{"type": kind, "pattern": `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`}
	}
	text := func(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
	props := map[string]any{}
	required := []string{}
	page := func() {
		props["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
		props["cursor"] = text(8192)
	}
	write := func() {
		props["operation_id"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "pattern": `^[A-Za-z0-9._-]+$`}
		required = append(required, "operation_id")
	}
	switch operation {
	case "issues":
		page()
		props["label_id"] = uuid(false)
	case "labels", "members":
		page()
		props["team_id"] = uuid(false)
		required = append(required, "team_id")
	case "issue":
		props["id"] = uuid(false)
		required = append(required, "id")
	case "create", "update":
		write()
		props["title"] = text(512)
		props["description"] = text(65536)
		props["state_id"] = uuid(false)
		props["project_id"] = uuid(true)
		props["parent_id"] = uuid(true)
		props["assignee_id"] = uuid(true)
		props["label_ids"] = map[string]any{"type": "array", "maxItems": 100, "uniqueItems": true, "items": uuid(false)}
		if operation == "create" {
			props["team_id"] = uuid(false)
			required = append(required, "title")
		} else {
			props["id"] = uuid(false)
			props["expected_label_ids"] = map[string]any{"type": "array", "maxItems": 100, "uniqueItems": true, "items": uuid(false)}
			required = append(required, "id")
		}
	case "comment":
		write()
		props["id"] = uuid(false)
		props["body"] = text(65536)
		required = append(required, "id", "body")
	case "relation_add", "relation_remove":
		write()
		props["from_id"], props["to_id"] = uuid(false), uuid(false)
		props["type"] = map[string]any{"type": "string", "enum": []string{"blocks", "blocked-by", "related"}}
		required = append(required, "from_id", "to_id", "type")
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": required}
}

func addTaskboardToolsV2(s *mcp.Server, b *binding, p TaskboardToolsV2) {
	for _, operation := range []string{"binding", "context", "issues", "issue", "create", "update", "comment", "labels", "members", "relation_add", "relation_remove"} {
		readOnly := operation != "create" && operation != "update" && operation != "comment" && operation != "relation_add" && operation != "relation_remove"
		closed := false
		mcp.AddTool(s, &mcp.Tool{
			Name: "taskboard_" + operation + "_v2", InputSchema: boardSchemaV2(operation),
			Description: "Use the native scoped Linear board. Explicit labels replace the label set; an update that sets label_ids must also send expected_label_ids, the complete label set you last read, and a mismatch is refused. Null clears only project, parent or assignee. Relation changes affect one edge. Missing native capabilities fail explicitly. Assignment is not a claim or human approval.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, IdempotentHint: readOnly, OpenWorldHint: &closed},
		}, func(ctx context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, bus.Result, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			scope := bus.Request{Workspace: b.workspace, Project: b.project, Session: b.session, Args: map[string]any{}}
			if b.instance != "" {
				scope.Args["instance"] = b.instance
			}
			bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			data, err := p.ExecuteV2(bounded, scope, operation, args)
			if err != nil {
				var receipt *linearboard.ReceiptError
				if data != nil && errors.As(err, &receipt) {
					out := bus.Result{Source: "linear", Data: map[string]any{"outcome": receipt.Outcome, "receipt": data}}
					raw, _ := json.Marshal(out)
					return &mcp.CallToolResult{IsError: true, StructuredContent: out, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, out, nil
				}
				return nil, bus.Result{}, err
			}
			return nil, bus.Result{Source: "linear", Data: data}, nil
		})
	}
}
