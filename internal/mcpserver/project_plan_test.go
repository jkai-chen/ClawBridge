package mcpserver

import (
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FR13: the plan tool takes the project by UUID, the three plan fields,
// an operation ID, confirm and the preview's digest, nothing else; it
// calls the board's project_plan with the write deadline and the session
// pinned, so the server decides who confirms. Its description names
// FR13's confirmers and says everyone else previews and asks.
func TestProjectPlanToolSchema(t *testing.T) {
	p := &recordingBoardV2{recordingBoard: recordingBoard{data: map[string]any{"outcome": "preview", "ask": true}}}
	ctx, cs := boardSession(t, p)
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, x := range listed.Tools {
		if x.Name == "taskboard_project_plan_v2" {
			tool = x
		}
	}
	if tool == nil || tool.Annotations == nil || tool.Annotations.ReadOnlyHint {
		t.Fatal("no write tool:", tool)
	}
	schema := tool.InputSchema.(map[string]any)
	var keys []string
	props := schema["properties"].(map[string]any)
	for k := range props {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"confirm", "expected_plan_sha256", "linear_project", "operation_id", "priority", "start_date", "target_date"}) || schema["additionalProperties"] != false {
		t.Fatal(keys, schema)
	}
	required, _ := schema["required"].([]any)
	if len(required) != 2 || required[0] != "linear_project" || required[1] != "operation_id" {
		t.Fatal("required", schema["required"])
	}
	for _, want := range []string{"the project owner, a project lead or a team's lead in the team roles, each in their own terminal", "the coordinator session inside its lane", "a session owned by a team's lead inside a lane holding that team", "Everyone else previews and asks", "project_plan_unproven", "never sent again"} {
		if !strings.Contains(tool.Description, want) {
			t.Fatal("description lacks", want)
		}
	}
	const id = "11111111-1111-4111-8111-111111111111"
	for _, bad := range []map[string]any{
		{"linear_project": "Roadmap", "priority": 2, "operation_id": "p-1"},
		{"linear_project": id, "priority": 2},
		{"linear_project": id, "target_date": "next week", "operation_id": "p-1"},
		{"linear_project": id, "priority": 2, "operation_id": "p-1", "add_team": id},
	} {
		if _, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_project_plan_v2", Arguments: bad}); err == nil && p.count() != 0 {
			t.Fatal("reached the board:", bad)
		}
	}
	// A session's confirm reaches the board with its pinned session: the
	// server answers a non-confirmer with the preview only.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_project_plan_v2", Arguments: map[string]any{"linear_project": id, "priority": "high", "target_date": nil, "operation_id": "p-1"}})
	if err != nil || res.IsError || p.count() != 1 {
		t.Fatal(err, res)
	}
	call := p.calls[0]
	if call.operation != "v2/project_plan" || !call.bounded || call.scope.Session != "pinned-actor" || call.args["priority"] != "high" || call.args["target_date"] != nil {
		t.Fatal(call)
	}
	if _, present := call.args["target_date"]; !present {
		t.Fatal("a null date must reach the board to clear it")
	}
}
