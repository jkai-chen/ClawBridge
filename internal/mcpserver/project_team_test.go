package mcpserver

import (
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FR10: the team-add tool takes the project and the team by UUID, an
// operation ID, confirm and the preview's digest, nothing else (no remove,
// no team list, no name), and calls the board's project_add_team.
func TestProjectAddTeamToolSchema(t *testing.T) {
	p := &recordingBoardV2{recordingBoard: recordingBoard{data: map[string]any{"ok": true}}}
	ctx, cs := boardSession(t, p)
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, x := range listed.Tools {
		if x.Name == "taskboard_project_add_team_v2" {
			tool = x
		}
	}
	if tool == nil || tool.Annotations == nil || tool.Annotations.ReadOnlyHint {
		t.Fatal("no write tool:", tool)
	}
	schema := tool.InputSchema.(map[string]any)
	var keys []string
	for k := range schema["properties"].(map[string]any) {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"confirm", "expected_plan_sha256", "linear_project", "operation_id", "team_id"}) || schema["additionalProperties"] != false {
		t.Fatal(keys, schema)
	}
	const id, team = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	if _, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_project_add_team_v2", Arguments: map[string]any{"linear_project": id, "team_id": "Platform"}}); err == nil && p.count() != 0 {
		t.Fatal("a team name reached the board")
	}
	if _, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_project_add_team_v2", Arguments: map[string]any{"linear_project": id, "team_id": team, "remove_team_id": team}}); err == nil && p.count() != 0 {
		t.Fatal("a remove reached the board")
	}
	if _, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_project_add_team_v2", Arguments: map[string]any{"linear_project": id, "team_id": team}}); err != nil || p.count() != 1 || p.calls[0].operation != "v2/project_add_team" {
		t.Fatal(err, p.calls)
	}
}
