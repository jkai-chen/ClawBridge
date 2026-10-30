package mcpserver

import (
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FR21: the history tool is read-only, takes the issue (UUID or
// identifier), limit 1–50 and cursor, nothing else, and calls the board's
// issue_history with the read deadline and the session pinned. Its
// description says the entries are typed fields only.
func TestIssueHistoryToolSchema(t *testing.T) {
	p := &recordingBoardV2{recordingBoard: recordingBoard{data: map[string]any{"issue_uuid": "11111111-1111-4111-8111-111111111111", "entries": []any{}}}}
	ctx, cs := boardSession(t, p)
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, x := range listed.Tools {
		if x.Name == "taskboard_issue_history_v2" {
			tool = x
		}
	}
	if tool == nil || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
		t.Fatal("not a read-only tool:", tool)
	}
	schema := tool.InputSchema.(map[string]any)
	props := schema["properties"].(map[string]any)
	var keys []string
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"cursor", "id", "limit"}) || schema["additionalProperties"] != false {
		t.Fatal(keys, schema)
	}
	if required, _ := schema["required"].([]any); len(required) != 1 || required[0] != "id" {
		t.Fatal("required", schema["required"])
	}
	if limit := props["limit"].(map[string]any); limit["minimum"] != float64(1) || limit["maximum"] != float64(50) {
		t.Fatal("limit", limit)
	}
	for _, want := range []string{"typed fields only, never a title, description or comment text", "ClawBridge Linear app", "app_unavailable", "never an email", "before asking a person", "hourly Linear budget", "other: true"} {
		if !strings.Contains(tool.Description, want) {
			t.Fatal("description lacks", want)
		}
	}
	const id = "11111111-1111-4111-8111-111111111111"
	for _, bad := range []map[string]any{
		{},
		{"id": "eng-88"},
		{"id": id, "limit": 51},
		{"id": id, "limit": 0},
		{"id": id, "comments": true},
		{"id": id, "cursor": strings.Repeat("c", 8193)},
	} {
		if _, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_issue_history_v2", Arguments: bad}); err == nil && p.count() != 0 {
			t.Fatal("reached the board:", bad)
		}
	}
	for _, ref := range []string{id, "ENG-88"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_issue_history_v2", Arguments: map[string]any{"id": ref, "limit": 5, "cursor": "c1"}})
		if err != nil || res.IsError {
			t.Fatal(ref, res, err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 2 {
		t.Fatal("calls", len(p.calls))
	}
	for i, call := range p.calls {
		if call.operation != "v2/issue_history" || call.scope.Session != "pinned-actor" || call.scope.Args["instance"] != "pinned-instance" || !call.bounded || call.args["limit"] != float64(5) || call.args["cursor"] != "c1" {
			t.Fatal(i, call)
		}
	}
}
