package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recordingBoardV2 struct{ recordingBoard }

func (p *recordingBoardV2) ExecuteV2(ctx context.Context, scope bus.Request, op string, args map[string]any) (any, error) {
	return p.Execute(ctx, scope, "v2/"+op, args)
}

func TestLinearMCPV2InitializationGuidanceIsAdditiveWithoutIO(t *testing.T) {
	_, defaultSession := boardSession(t, nil)
	v1 := &recordingBoard{}
	_, oldSession := boardSession(t, v1)
	v2 := &recordingBoardV2{}
	ctx, newSession := boardSession(t, v2)
	defaultText := defaultSession.InitializeResult().Instructions
	oldText := oldSession.InitializeResult().Instructions
	newText := newSession.InitializeResult().Instructions
	if strings.Contains(defaultText, "Native Linear V2") || strings.Contains(oldText, "Native Linear V2") || !strings.HasPrefix(newText, oldText+"\n\n") {
		t.Fatal("V2 guidance altered default/V1 instructions")
	}
	for _, required := range []string{"advertised *_v2 family", "project-set boards and new board calls", "exact operation IDs and payloads", "unsupported_mapping", "explicit capability limit"} {
		if !strings.Contains(newText[len(oldText):], required) {
			t.Fatal("missing V2 guidance", required)
		}
	}
	tools, err := newSession.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 32 || v1.count() != 0 || v2.count() != 0 {
		t.Fatal("initialization/list changed tools or performed provider IO", err)
	}
}

func TestLinearMCPV2ClosedNullablePinnedTools(t *testing.T) {
	p := &recordingBoardV2{recordingBoard: recordingBoard{data: map[string]any{"ok": true}}}
	ctx, cs := boardSession(t, p)
	listed, err := cs.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 32 || p.count() != 0 {
		t.Fatalf("native list: %v %d calls=%d", err, len(listed.Tools), p.count())
	}
	tools := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		tools[tool.Name] = tool
	}
	const id = "11111111-1111-4111-8111-111111111111"
	const to = "22222222-2222-4222-8222-222222222222"
	cases := []struct {
		op   string
		args map[string]any
		read bool
	}{
		{"binding", map[string]any{}, true}, {"context", map[string]any{}, true},
		{"issues", map[string]any{"limit": 2, "cursor": "opaque", "label_id": id}, true},
		{"issue", map[string]any{"id": id}, true},
		{"create", map[string]any{"title": "new", "operation_id": "create", "team_id": id, "parent_id": nil, "assignee_id": nil}, false},
		{"update", map[string]any{"id": id, "operation_id": "update", "project_id": nil, "parent_id": nil, "assignee_id": nil, "label_ids": []string{}, "expected_label_ids": []string{id}}, false},
		{"comment", map[string]any{"id": id, "body": "body", "operation_id": "comment"}, false},
		{"labels", map[string]any{"team_id": id, "limit": 1}, true},
		{"members", map[string]any{"team_id": id, "cursor": "opaque"}, true},
		{"relation_add", map[string]any{"from_id": id, "to_id": to, "type": "blocked-by", "operation_id": "edge-add"}, false},
		{"relation_remove", map[string]any{"from_id": id, "to_id": to, "type": "related", "operation_id": "edge-remove"}, false},
	}
	for _, tc := range cases {
		tool := tools["taskboard_"+tc.op+"_v2"]
		if tool == nil || tool.Annotations == nil || tool.Annotations.ReadOnlyHint != tc.read || tool.Annotations.IdempotentHint != tc.read {
			t.Fatal("missing or incorrectly annotated tool", tc.op)
		}
		result, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool.Name, Arguments: tc.args})
		if e != nil || result.IsError {
			t.Fatalf("%s: %+v %v", tc.op, result, e)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != len(cases) {
		t.Fatal("wrong calls", len(p.calls))
	}
	for i, call := range p.calls {
		if call.operation != "v2/"+cases[i].op || call.scope.Workspace != "pinned-workspace" || call.scope.Project != "pinned-project" || call.scope.Session != "pinned-actor" || call.scope.Args["instance"] != "pinned-instance" || !call.bounded {
			t.Fatal("route or deadline changed", call)
		}
	}
	update := p.calls[5].args
	for _, field := range []string{"project_id", "parent_id", "assignee_id"} {
		value, exists := update[field]
		if !exists || value != nil {
			t.Fatal("explicit null lost", field, update)
		}
	}
	if labels, ok := update["label_ids"].([]any); !ok || labels == nil || len(labels) != 0 {
		t.Fatal("explicit empty labels lost", update)
	}
}

func TestLinearMCPV2RejectsClosedSchemaBeforeAdapter(t *testing.T) {
	p := &recordingBoardV2{}
	ctx, cs := boardSession(t, p)
	const id = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		op   string
		args map[string]any
	}{
		{"context", map[string]any{"profile": "foreign"}},
		{"binding", map[string]any{"workspace": "foreign"}},
		{"issues", map[string]any{"limit": nil}},
		{"issues", map[string]any{"limit": 101}},
		{"issues", map[string]any{"cursor": nil}},
		{"labels", map[string]any{"team_id": id, "project": "foreign"}},
		{"members", map[string]any{"team_id": nil}},
		{"members", map[string]any{}},
		{"create", map[string]any{"title": "x", "operation_id": "op", "label_ids": nil}},
		{"create", map[string]any{"title": "x", "operation_id": "op", "label_ids": []string{id, id}}},
		{"update", map[string]any{"id": id, "operation_id": "op", "state_id": nil}},
		{"update", map[string]any{"id": id, "operation_id": "op", "team_id": id}},
		{"update", map[string]any{"id": id, "operation_id": "op", "description": nil}},
		{"update", map[string]any{"id": id, "operation_id": "op", "parent_id": "FJA-1"}},
		{"comment", map[string]any{"id": id, "body": "body", "operation_id": nil}},
		{"relation_add", map[string]any{"from_id": id, "to_id": id, "type": "related", "operation_id": "op", "edge_id": id}},
	} {
		t.Run(tc.op+stringJSON(tc.args), func(t *testing.T) {
			result, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_" + tc.op + "_v2", Arguments: tc.args})
			if e == nil && !result.IsError {
				t.Fatal("invalid arguments accepted", tc.args)
			}
			if p.count() != 0 {
				t.Fatal("invalid arguments reached adapter")
			}
		})
	}
}

func TestLinearMCPV2PreservesKnownReceipt(t *testing.T) {
	p := &recordingBoardV2{recordingBoard: recordingBoard{data: map[string]any{"kind": "edge", "from_id": "known-endpoint"}, err: &linearboard.ReceiptError{Outcome: "accepted_ledger_unrecorded"}}}
	ctx, cs := boardSession(t, p)
	const id = "11111111-1111-4111-8111-111111111111"
	const other = "22222222-2222-4222-8222-222222222222"
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_relation_add_v2", Arguments: map[string]any{"from_id": id, "to_id": other, "type": "blocks", "operation_id": "edge"}})
	if err != nil || !result.IsError {
		t.Fatal("known outcome lost", err, result)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if !strings.Contains(string(raw), "known-endpoint") || !strings.Contains(string(raw), "accepted_ledger_unrecorded") || !strings.Contains(string(raw), "linear") {
		t.Fatal("receipt dropped", string(raw))
	}
}

func TestLinearMCPV2ExpectedLabelsOnlyOnUpdate(t *testing.T) {
	p := &recordingBoardV2{recordingBoard: recordingBoard{data: map[string]any{"ok": true}}}
	ctx, cs := boardSession(t, p)
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		raw, _ := json.Marshal(tool.InputSchema)
		has := strings.Contains(string(raw), `"expected_label_ids"`)
		if has != (tool.Name == "taskboard_update_v2") {
			t.Fatalf("%s expected_label_ids presence = %v", tool.Name, has)
		}
		if tool.Name == "taskboard_update_v2" && !strings.Contains(tool.Description, "expected_label_ids") {
			t.Fatal("update tool does not advertise the expectation requirement")
		}
	}
	const id = "11111111-1111-4111-8111-111111111111"
	before := p.count()
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_create_v2", Arguments: map[string]any{"title": "t", "operation_id": "c", "expected_label_ids": []string{id}}})
	if (err == nil && !result.IsError) || p.count() != before {
		t.Fatal("create accepted expected_label_ids", result, err)
	}
}
