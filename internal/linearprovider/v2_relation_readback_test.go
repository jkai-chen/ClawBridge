package linearprovider

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nativeLinear answers save_issue and get_issue like native Linear: an
// added blocks edge is stored, and get_issue returns relations only when
// asked with includeRelations.
type nativeLinear struct {
	tools  []*mcp.Tool
	blocks []any
	asked  []map[string]any
}

func (n *nativeLinear) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: n.tools}, nil
}

func (n *nativeLinear) CallTool(_ context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	args := p.Arguments.(map[string]any)
	n.asked = append(n.asked, args)
	var result map[string]any
	switch p.Name {
	case "save_issue":
		for _, to := range args["blocks"].([]string) {
			n.blocks = append(n.blocks, to)
		}
		result = map[string]any{"id": "SYN-6", "uuid": iid}
	case "get_issue":
		result = nativeGetIssue([]any{}, emptyNativeRelations())
		if args["includeRelations"] == true {
			result["relations"] = map[string]any{"blocks": n.blocks, "blockedBy": []any{}, "relatedTo": []any{}, "duplicateOf": nil}
		} else {
			delete(result, "relations")
		}
	}
	raw, _ := json.Marshal(result)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
}

// Against today's native catalog, a relation added through the save_issue
// fallback is visible when the issue is read back: the read asks for the
// relations.
func TestNativeRelationAddIsReadBack(t *testing.T) {
	raw, err := os.ReadFile("../doctor/testdata/linear-native-catalog-20261022.json")
	if err != nil {
		t.Fatal(err)
	}
	n := &nativeLinear{}
	if err = json.Unmarshal(raw, &n.tools); err != nil {
		t.Fatal(err)
	}
	p, err := NewWithCaller(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Relation(context.Background(), RelationInput{Action: "add", Type: "blocks", FromID: iid, ToID: uid}); err != nil {
		t.Fatal(err)
	}
	x, err := p.IssueV2(context.Background(), iid)
	if err != nil {
		t.Fatal(err)
	}
	last := n.asked[len(n.asked)-1]
	if last["includeRelations"] != true {
		t.Fatalf("get_issue was not asked for relations: %v", last)
	}
	if len(x.Relations) != 1 || x.Relations[0].Type != "blocks" || x.Relations[0].FromID != iid || x.Relations[0].ToID != uid {
		t.Fatalf("the added edge is not read back: %+v", x.Relations)
	}
}
