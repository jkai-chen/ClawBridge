package linearprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This is the exact save_issue tool from the owner's zero-CallTool official
// WRITE catalog c735d67583bb462a7433b79584aeae35934557c6300fff83be3e39f7ba776836.
// No live calls, credentials or private provider responses are used by these tests.
func officialSaveIssue(t *testing.T) *mcp.Tool {
	t.Helper()
	data, err := os.ReadFile("testdata/official-save-issue-20261022.json")
	if err != nil {
		t.Fatal(err)
	}
	var tool mcp.Tool
	if err := json.Unmarshal(data, &tool); err != nil {
		t.Fatal(err)
	}
	return &tool
}

func TestV2NativeSaveIssueSingleEdge(t *testing.T) {
	for _, action := range []string{"add", "remove"} {
		for _, kind := range []string{"blocks", "related"} {
			t.Run(action+"/"+kind, func(t *testing.T) {
				f := &fake{tools: []*mcp.Tool{officialSaveIssue(t)}, result: map[string]any{"id": "FJA-999", "uuid": iid}}
				p, err := NewWithCaller(context.Background(), f)
				if err != nil {
					t.Fatal(err)
				}
				caps, err := p.Capabilities(context.Background())
				if err != nil || !caps.RelationAdd || !caps.RelationRemove || caps.ParentSameDestination || len(f.calls) != 0 {
					t.Fatal("wrong capability or negotiation performed I/O", caps, err, f.calls)
				}
				receipt, err := p.Relation(context.Background(), RelationInput{Action: action, Type: kind, FromID: iid, ToID: uid})
				if err != nil || receipt != (RelationReceipt{Action: action, Type: kind, FromID: iid, ToID: uid}) {
					t.Fatal("issue UUID must not become an edge UUID", receipt, err)
				}
				key := map[string]string{"add/blocks": "blocks", "remove/blocks": "removeBlocks", "add/related": "relatedTo", "remove/related": "removeRelatedTo"}[action+"/"+kind]
				want := map[string]any{"id": iid, key: []string{uid}}
				if !reflect.DeepEqual(f.calls, []string{"save_issue"}) || !reflect.DeepEqual(f.args, []map[string]any{want}) {
					t.Fatal("must send exactly one edge and no unrelated fields", f.calls, f.args)
				}
			})
		}
	}
}

func TestV2NativeSaveIssueRejectsUnsafeEdgeSchemaBeforeDispatch(t *testing.T) {
	for _, change := range []string{"replacement", "missing-description", "wrong-items", "required-unrelated-field", "remove-replacement"} {
		t.Run(change, func(t *testing.T) {
			tool := officialSaveIssue(t)
			root := schemaProperty(tool.InputSchema)
			props := root["properties"].(map[string]any)
			action := "add"
			switch change {
			case "replacement":
				props["blocks"].(map[string]any)["description"] = "Replace the complete relation list"
			case "missing-description":
				delete(props["blocks"].(map[string]any), "description")
			case "wrong-items":
				props["blocks"].(map[string]any)["items"] = map[string]any{"type": "object"}
			case "required-unrelated-field":
				root["required"] = []string{"title"}
			case "remove-replacement":
				action = "remove"
				props["removeBlocks"].(map[string]any)["description"] = "Replace the complete relation list"
			}
			tool.InputSchema = root
			f := &fake{tools: []*mcp.Tool{tool}, result: map[string]any{"id": iid}}
			p, err := NewWithCaller(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Relation(context.Background(), RelationInput{Action: action, Type: "blocks", FromID: iid, ToID: uid}); err == nil || len(f.calls) != 0 {
				t.Fatal("unsafe schema dispatched", err, f.calls)
			}
		})
	}
}

func TestV2NativeSaveIssueEdgeNoRetryAndResponseIdentity(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{officialSaveIssue(t)}, err: errors.New("ambiguous response containing a private secret")}
	p, err := NewWithCaller(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	in := RelationInput{Action: "add", Type: "blocks", FromID: iid, ToID: uid}
	if _, err := p.Relation(context.Background(), in); err == nil || err.Error() != "Linear provider: unavailable" || len(f.calls) != 1 {
		t.Fatal("error leaked or mutation repeated", err, f.calls)
	}
	f.err = nil
	for _, result := range []any{map[string]any{"id": uid}, map[string]any{"id": iid, "uuid": uid}, map[string]any{"success": true}} {
		f.result = result
		before := len(f.calls)
		if receipt, err := p.Relation(context.Background(), in); err == nil || receipt != (RelationReceipt{}) || len(f.calls) != before+1 {
			t.Fatal("unproven response identity accepted/retried", receipt, err, f.calls)
		}
	}
	before := len(f.calls)
	for _, invalid := range []RelationInput{{Action: "add", Type: "blocks", FromID: iid, ToID: iid}, {Action: "add", Type: "blocks", FromID: "FJA-999", ToID: uid}, {Action: "add", Type: "related", FromID: uid, ToID: iid}} {
		if _, err := p.Relation(context.Background(), invalid); err == nil || len(f.calls) != before {
			t.Fatal("noncanonical edge dispatched", err, f.calls)
		}
	}
}

func TestV2NativeSaveIssueAliasesPresenceAndParentGate(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{officialSaveIssue(t)}, result: issueResult()}
	p, err := NewWithCaller(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	clear := UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Project: Field[*string]{Present: true}, Labels: Field[[]string]{Present: true, Value: []string{}}, Parent: Field[*string]{Present: true}, Assignee: Field[*string]{Present: true}}
	if err := p.ValidateUpdateV2(clear); err != nil || len(f.calls) != 0 {
		t.Fatal("official nullable aliases rejected during preflight", err, f.calls)
	}
	if _, err := p.UpdateV2(context.Background(), clear); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": iid, "project": nil, "labels": []string{}, "parentId": nil, "assignee": nil}
	if !reflect.DeepEqual(f.args[0], want) {
		t.Fatal("wrong aliases or omitted intent", f.args)
	}
	target := uid
	set := UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Project: Field[*string]{Present: true, Value: &target}, Labels: Field[[]string]{Present: true, Value: []string{tid}}, Assignee: Field[*string]{Present: true, Value: &target}}
	if _, err := p.UpdateV2(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.args[1], map[string]any{"id": iid, "project": uid, "labels": []string{tid}, "assignee": uid}) {
		t.Fatal("non-null aliases or parent omission changed", f.args)
	}
	set.Parent = Field[*string]{Present: true, Value: &target}
	if err := p.ValidateUpdateV2(set); err == nil || len(f.calls) != 2 {
		t.Fatal("parent destination semantics invented", err, f.calls)
	}
}
