package linearprovider

import (
	"context"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The official save_issue takes priority as a number, 0-4: the provider
// says so and sends it as a whole number, on create and on update; a value
// outside 0-4 is refused before anything is sent.
func TestV2PriorityOnTheOfficialSaveIssue(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{officialSaveIssue(t)}, result: issueResult()}
	p, err := NewWithCaller(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := p.Capabilities(context.Background())
	if err != nil || !caps.Priority || len(f.calls) != 0 {
		t.Fatal(caps, err, f.calls)
	}
	high, none := 2, 0
	if _, err := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Priority: &high}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.args[0], map[string]any{"id": iid, "priority": 2}) {
		t.Fatal(f.args)
	}
	// 0 clears it, and is sent rather than left out.
	if _, err := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Priority: &none}); err != nil || f.args[1]["priority"] != 0 {
		t.Fatal(f.args, err)
	}
	if _, err := p.CreateV2(context.Background(), CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "t"}, Priority: &high}); err != nil || f.args[2]["priority"] != 2 {
		t.Fatal(f.args, err)
	}
	for _, bad := range []int{-1, 5} {
		calls := len(f.calls)
		if _, err := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Priority: &bad}); err == nil || len(f.calls) != calls {
			t.Fatal(bad, err, f.calls)
		}
		if _, err := p.CreateV2(context.Background(), CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "t"}, Priority: &bad}); err == nil || len(f.calls) != calls {
			t.Fatal(bad, err, f.calls)
		}
	}
}

// A save_issue without priority, or with it on one side only, is no
// priority capability; asked anyway, nothing is sent.
func TestV2PriorityNeedsTheSchema(t *testing.T) {
	tool := officialSaveIssue(t)
	root := schemaProperty(tool.InputSchema)
	delete(root["properties"].(map[string]any), "priority")
	tool.InputSchema = root
	f := &fake{tools: []*mcp.Tool{tool}, result: issueResult()}
	p, err := NewWithCaller(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if caps, err := p.Capabilities(context.Background()); err != nil || caps.Priority {
		t.Fatal(caps, err)
	}
	high := 2
	if _, err := p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Priority: &high}); err == nil || len(f.calls) != 0 {
		t.Fatal(err, f.calls)
	}
	// priority typed as a string is no capability either.
	tool = officialSaveIssue(t)
	root = schemaProperty(tool.InputSchema)
	root["properties"].(map[string]any)["priority"] = map[string]any{"type": "string"}
	tool.InputSchema = root
	p, err = NewWithCaller(context.Background(), &fake{tools: []*mcp.Tool{tool}, result: issueResult()})
	if err != nil {
		t.Fatal(err)
	}
	if caps, err := p.Capabilities(context.Background()); err != nil || caps.Priority {
		t.Fatal(caps, err)
	}
}

// Priority on create only, or on update only, is no capability.
func TestV2PriorityNeedsCreateAndUpdate(t *testing.T) {
	create := tool("create_issue", map[string]string{"team": "string", "title": "string", "description": "string", "priority": "number"}, "team", "title")
	update := tool("update_issue", map[string]string{"id": "string", "title": "string", "priority": "number"}, "id")
	plainCreate := tool("create_issue", map[string]string{"team": "string", "title": "string", "description": "string"}, "team", "title")
	plainUpdate := tool("update_issue", map[string]string{"id": "string", "title": "string"}, "id")
	for want, tools := range map[bool][][]*mcp.Tool{true: {{create, update}}, false: {{create, plainUpdate}, {plainCreate, update}}} {
		for _, ts := range tools {
			p, err := NewWithCaller(context.Background(), &fake{tools: ts})
			if err != nil {
				t.Fatal(err)
			}
			if caps, err := p.Capabilities(context.Background()); err != nil || caps.Priority != want {
				t.Fatal(want, ts[0].Name, caps, err)
			}
		}
	}
}
