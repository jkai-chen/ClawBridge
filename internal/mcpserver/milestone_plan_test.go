package mcpserver

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FR55: the milestone plan tool takes the project by UUID, the milestone by
// UUID to update (none creates), a name, a target date (null clears), an
// operation ID, confirm and the preview's digest, nothing else, in neutral
// names; it calls the board's milestone_plan with the write deadline and the
// session pinned, so the server decides who confirms. Its description names
// the confirmers and says everyone else previews and asks.
func TestMilestonePlanToolSchema(t *testing.T) {
	p := &recordingBoardV2{recordingBoard: recordingBoard{data: map[string]any{"outcome": "preview", "ask": true}}}
	ctx, cs := boardSession(t, p)
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, x := range listed.Tools {
		if x.Name == "taskboard_milestone_plan_v2" {
			tool = x
		}
	}
	if tool == nil || tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.IdempotentHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
		t.Fatal("no write tool:", tool)
	}
	schema := tool.InputSchema.(map[string]any)
	var keys []string
	props := schema["properties"].(map[string]any)
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"confirm", "expected_plan_sha256", "milestone", "name", "operation_id", "project", "target_date"}) || schema["additionalProperties"] != false {
		t.Fatal(keys, schema)
	}
	required, _ := schema["required"].([]any)
	if len(required) != 2 || required[0] != "project" || required[1] != "operation_id" {
		t.Fatal("required", schema["required"])
	}
	if name := props["name"].(map[string]any); name["type"] != "string" || name["minLength"] != float64(1) || name["maxLength"] != float64(80) {
		t.Fatal(name)
	}
	if date := props["target_date"].(map[string]any); !slices.Equal(anyStrings(date["type"]), []string{"string", "null"}) {
		t.Fatal(date)
	}
	// Neutral names only: no Linear spelling in the schema or description.
	raw, _ := json.Marshal(schema)
	for _, spelling := range []string{"targetDate", "projectMilestone", "save_milestone", "list_milestones"} {
		if strings.Contains(string(raw), spelling) || strings.Contains(tool.Description, spelling) {
			t.Fatal("a Linear spelling leaked:", spelling)
		}
	}
	for _, want := range []string{"the project owner, a project lead or a team's lead in the team roles, each in their own terminal", "the coordinator session inside its lane", "a session owned by a team's lead inside a lane holding that team", "Everyone else previews and asks", "milestone_plan_unproven", "never sent again", "flagged in the preview", "never deleted", "written by others"} {
		if !strings.Contains(tool.Description, want) {
			t.Fatal("description lacks", want)
		}
	}
	const id = "11111111-1111-4111-8111-111111111111"
	for _, bad := range []map[string]any{
		{"project": "Roadmap", "name": "Beta", "operation_id": "m-1"},
		{"project": id, "name": "Beta"},
		{"project": id, "milestone": "Beta", "name": "Beta 2", "operation_id": "m-1"},
		{"project": id, "name": "", "operation_id": "m-1"},
		{"project": id, "name": strings.Repeat("x", 81), "operation_id": "m-1"},
		{"project": id, "target_date": "next week", "name": "Beta", "operation_id": "m-1"},
		{"project": id, "name": "Beta", "operation_id": "m-1", "description": "x"},
		{"project": id, "name": "Beta", "operation_id": "m-1", "linear_project": id},
	} {
		if _, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_milestone_plan_v2", Arguments: bad}); err == nil && p.count() != 0 {
			t.Fatal("reached the board:", bad)
		}
	}
	// A session's call reaches the board with its pinned session: the server
	// answers a non-confirmer with the preview only. A null date reaches it
	// to clear the date; no name leaves the name.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_milestone_plan_v2", Arguments: map[string]any{"project": id, "milestone": id, "target_date": nil, "operation_id": "m-1"}})
	if err != nil || res.IsError || p.count() != 1 {
		t.Fatal(err, res)
	}
	call := p.calls[0]
	if call.operation != "v2/milestone_plan" || !call.bounded || call.scope.Session != "pinned-actor" || call.args["milestone"] != id || call.args["target_date"] != nil {
		t.Fatal(call)
	}
	if _, present := call.args["target_date"]; !present {
		t.Fatal("a null date must reach the board to clear it")
	}
	if _, present := call.args["name"]; present {
		t.Fatal("a name nobody gave reached the board")
	}
	text, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(text), `"ask":true`) || strings.Contains(string(text), `"outcome":"accepted"`) {
		t.Fatal("a non-confirmer got more than the preview:", string(text))
	}
}

func anyStrings(v any) []string {
	var out []string
	switch list := v.(type) {
	case []any:
		for _, x := range list {
			s, _ := x.(string)
			out = append(out, s)
		}
	case []string:
		out = list
	}
	return out
}
