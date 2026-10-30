package linearprovider

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AGENT-CANCEL R8: a duplicate edge goes through save_issue's duplicateOf
// in today's native catalog, from the duplicate to the original; it is
// only ever added.
func TestDuplicateEdgeUsesSaveIssueDuplicateOf(t *testing.T) {
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
	r, err := p.Relation(context.Background(), RelationInput{Action: "add", Type: "duplicate", FromID: iid, ToID: uid})
	if err != nil {
		t.Fatal(err)
	}
	sent := n.asked[len(n.asked)-1]
	if sent["id"] != iid || sent["duplicateOf"] != uid || len(sent) != 2 {
		t.Fatalf("save_issue was not asked for duplicateOf alone: %v", sent)
	}
	if r.Type != "duplicate" || r.FromID != iid || r.ToID != uid || r.Confirmed {
		t.Fatalf("receipt: %+v", r)
	}
	before := len(n.asked)
	if _, err = p.Relation(context.Background(), RelationInput{Action: "remove", Type: "duplicate", FromID: iid, ToID: uid}); err == nil || len(n.asked) != before {
		t.Fatal("a duplicate edge was removed:", err)
	}
}

// A relation tool whose type enum names "duplicate" is preferred; one
// whose enum doesn't, and a save_issue without duplicateOf, leave the
// edge unavailable before anything is sent.
func TestDuplicateEdgeNegotiation(t *testing.T) {
	schema := func(props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props}
	}
	relation := func(kinds ...string) *mcp.Tool {
		return &mcp.Tool{Name: "create_issue_relation", InputSchema: schema(map[string]any{"issueId": map[string]any{"type": "string"}, "relatedIssueId": map[string]any{"type": "string"}, "type": map[string]any{"type": "string", "enum": kinds}})}
	}
	saveIssue := &mcp.Tool{Name: "save_issue", InputSchema: schema(map[string]any{"id": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}})}
	for name, c := range map[string]struct {
		tools []*mcp.Tool
		want  string
	}{
		"enum names duplicate": {[]*mcp.Tool{relation("blocks", "related", "duplicate"), saveIssue}, "create_issue_relation"},
		"enum lacks duplicate": {[]*mcp.Tool{relation("blocks", "related"), saveIssue}, ""},
		"no tool":              {[]*mcp.Tool{saveIssue}, ""},
	} {
		p := &Provider{tools: map[string]*mcp.Tool{}}
		for _, tool := range c.tools {
			p.tools[tool.Name] = tool
		}
		got, values, _, err := p.relationTool(RelationInput{Action: "add", Type: "duplicate", FromID: iid, ToID: uid})
		if c.want == "" {
			if err == nil {
				t.Fatal(name, "negotiated", got)
			}
			continue
		}
		if err != nil || got != c.want || values["type"] != "duplicate" || values["issueId"] != iid || values["relatedIssueId"] != uid {
			t.Fatal(name, got, values, err)
		}
	}
}
