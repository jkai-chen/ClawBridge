package linearprovider

import (
	"context"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	suProject = "f1000000-0000-4000-8000-000000000001"
	suUpdate  = "f2000000-0000-4000-8000-000000000002"
)

// The status-update tools as P7 saw them: save takes body, health, id,
// initiative, isDiffHidden, project and the required type.
func statusUpdateTool(name string, props map[string]any, required ...string) *mcp.Tool {
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object", "properties": props, "required": required}}
}

func fakeStatusTools(typeEnum, healthEnum []any) []*mcp.Tool {
	s := map[string]any{"type": "string"}
	save := map[string]any{"body": s, "id": s, "initiative": s, "isDiffHidden": map[string]any{"type": "boolean"}, "project": s,
		"type": map[string]any{"type": "string", "enum": typeEnum}, "health": map[string]any{"type": "string", "enum": healthEnum}}
	return []*mcp.Tool{
		statusUpdateTool("save_status_update", save, "type"),
		statusUpdateTool("get_status_updates", map[string]any{"project": s, "limit": map[string]any{"type": "number"}}),
		statusUpdateTool("delete_status_update", map[string]any{"id": s}, "id"),
	}
}

func statusProvider(t *testing.T, tools []*mcp.Tool, answers map[string][]any) (*Provider, *scripted) {
	t.Helper()
	f := &scripted{fake: &fake{tools: tools}, answers: answers}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	return p, f
}

// A save sends exactly the project, body, health and type, in the tool's
// own enum spelling; a list and a delete send the project and the ID.
func TestStatusUpdateRoundTripSendsOnlyWhatItSays(t *testing.T) {
	p, f := statusProvider(t, fakeStatusTools([]any{"Project", "Initiative"}, []any{"onTrack", "atRisk", "offTrack"}), map[string][]any{
		"save_status_update":   {map[string]any{"statusUpdate": map[string]any{"id": suUpdate}}},
		"get_status_updates":   {map[string]any{"statusUpdates": []any{map[string]any{"id": suUpdate, "health": "onTrack", "body": "ClawBridge probe, will be deleted"}, map[string]any{"id": "f3000000-0000-4000-8000-000000000003", "health": "atRisk", "body": "a person's update"}}}},
		"delete_status_update": {map[string]any{"success": true}},
	})
	id, e := p.SaveStatusUpdate(context.Background(), suProject, "ClawBridge probe, will be deleted", "onTrack")
	if e != nil || id != suUpdate {
		t.Fatal(id, e)
	}
	if want := map[string]any{"project": suProject, "body": "ClawBridge probe, will be deleted", "health": "onTrack", "type": "Project"}; !reflect.DeepEqual(f.args[0], want) {
		t.Fatal("save arguments", f.args[0])
	}
	list, e := p.StatusUpdates(context.Background(), suProject, "ClawBridge probe")
	if e != nil || len(list) != 2 || !list[0].Marked || list[1].Marked || list[0].Health != "onTrack" {
		t.Fatal(list, e)
	}
	if want := map[string]any{"project": suProject}; !reflect.DeepEqual(f.args[1], want) {
		t.Fatal("list arguments", f.args[1])
	}
	if e = p.DeleteStatusUpdate(context.Background(), suUpdate); e != nil || !reflect.DeepEqual(f.args[2], map[string]any{"id": suUpdate}) {
		t.Fatal("delete", e, f.args[2])
	}
}

// Nothing is sent when the tools could not create, list and delete a
// project update: a missing tool, a type enum without project, a health
// enum without the health asked for.
func TestStatusUpdatePreflightRefusesBeforeSending(t *testing.T) {
	full := fakeStatusTools([]any{"project"}, []any{"onTrack"})
	for name, tools := range map[string][]*mcp.Tool{
		"no delete":     full[:2],
		"no list":       {full[0], full[2]},
		"no project":    fakeStatusTools([]any{"initiative"}, []any{"onTrack"}),
		"no onTrack":    fakeStatusTools([]any{"project"}, []any{"atRisk"}),
		"delete by key": append(append([]*mcp.Tool{}, full[:2]...), statusUpdateTool("delete_status_update", map[string]any{"key": map[string]any{"type": "string"}}, "key")),
	} {
		// A save would be answered, so only the preflight can stop it.
		p, f := statusProvider(t, tools, map[string][]any{"save_status_update": {map[string]any{"id": suUpdate}}})
		if e := p.StatusUpdatePreflight("onTrack"); e == nil {
			t.Fatal(name, "passed preflight")
		}
		if _, e := p.SaveStatusUpdate(context.Background(), suProject, "probe", "onTrack"); e == nil || len(f.answers["save_status_update"]) != 1 {
			t.Fatal(name, "sent a save")
		}
	}
	p, _ := statusProvider(t, full, map[string][]any{})
	if e := p.StatusUpdatePreflight("onTrack"); e != nil {
		t.Fatal(e)
	}
	if s := p.StatusUpdateSchemas(); s["save_status_update"] == "absent" || s["delete_status_update"] == "absent" {
		t.Fatal(s)
	}
}

// Answers of unknown shape: an ID under a wrapper is read, a save with no
// readable ID reports "", and a list in no known key is refused.
func TestStatusUpdateAnswerShapes(t *testing.T) {
	tools := fakeStatusTools([]any{"project"}, []any{"onTrack"})
	p, _ := statusProvider(t, tools, map[string][]any{
		"save_status_update": {map[string]any{"id": suUpdate}, map[string]any{"success": true}},
		"get_status_updates": {map[string]any{"projectUpdates": []any{}}, map[string]any{"results": []any{}}},
	})
	if id, e := p.SaveStatusUpdate(context.Background(), suProject, "b", "onTrack"); e != nil || id != suUpdate {
		t.Fatal(id, e)
	}
	if id, e := p.SaveStatusUpdate(context.Background(), suProject, "b", "onTrack"); e != nil || id != "" {
		t.Fatal("an answer without an ID", id, e)
	}
	if list, e := p.StatusUpdates(context.Background(), suProject, "m"); e != nil || len(list) != 0 {
		t.Fatal(list, e)
	}
	if _, e := p.StatusUpdates(context.Background(), suProject, "m"); e == nil {
		t.Fatal("a list in an unknown key was read")
	}
}

// The three tools exactly as the app's tool list showed them in the P8
// dry run (26 Oct): every one requires type, list and delete included.
func liveStatusTools() []*mcp.Tool {
	s := map[string]any{"type": "string"}
	kind := map[string]any{"type": "string", "enum": []any{"project", "initiative"}}
	return []*mcp.Tool{
		statusUpdateTool("save_status_update", map[string]any{"body": s, "health": map[string]any{"type": "string", "enum": []any{"onTrack", "atRisk", "offTrack"}},
			"id": s, "initiative": s, "isDiffHidden": map[string]any{"type": "boolean"}, "project": s, "type": kind}, "type"),
		statusUpdateTool("get_status_updates", map[string]any{"createdAt": s, "cursor": s, "id": s, "includeArchived": map[string]any{"type": "boolean"},
			"initiative": s, "limit": map[string]any{"type": "number"}, "orderBy": map[string]any{"type": "string", "enum": []any{"createdAt", "updatedAt"}},
			"project": s, "type": kind, "updatedAt": s, "user": s}, "type"),
		statusUpdateTool("delete_status_update", map[string]any{"id": s, "type": kind}, "type", "id"),
	}
}

// Against the live schemas the preflight passes, and the list and the
// delete send type project beside the project and the ID.
func TestStatusUpdateLiveSchemasSendType(t *testing.T) {
	p, f := statusProvider(t, liveStatusTools(), map[string][]any{
		"save_status_update":   {map[string]any{"id": suUpdate}},
		"get_status_updates":   {map[string]any{"statusUpdates": []any{}}},
		"delete_status_update": {map[string]any{"success": true}},
	})
	if e := p.StatusUpdatePreflight("onTrack"); e != nil {
		t.Fatal(e)
	}
	if id, e := p.SaveStatusUpdate(context.Background(), suProject, "probe", "onTrack"); e != nil || id != suUpdate {
		t.Fatal(id, e)
	}
	if _, e := p.StatusUpdates(context.Background(), suProject, "probe"); e != nil {
		t.Fatal(e)
	}
	if e := p.DeleteStatusUpdate(context.Background(), suUpdate); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(f.args[1], map[string]any{"project": suProject, "type": "project"}) || !reflect.DeepEqual(f.args[2], map[string]any{"id": suUpdate, "type": "project"}) {
		t.Fatal("list and delete arguments", f.args[1], f.args[2])
	}
	// A list or delete whose type enum lacks project is refused before
	// anything is sent, even where type is optional.
	for i := 1; i <= 2; i++ {
		tools := liveStatusTools()
		in := tools[i].InputSchema.(map[string]any)
		in["properties"].(map[string]any)["type"] = map[string]any{"type": "string", "enum": []any{"initiative"}}
		in["required"] = []string{}
		p, _ := statusProvider(t, tools, map[string][]any{})
		if e := p.StatusUpdatePreflight("onTrack"); e == nil {
			t.Fatal(tools[i].Name, "passed preflight without a project type")
		}
	}
}
