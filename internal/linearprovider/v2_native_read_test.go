package linearprovider

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nativeGetIssue mirrors a live get_issue response for the fields ClawBridge
// requests: display id, canonical uuid, identifier-form parentId, label NAMES
// and a relations object without any completeness flag.
func nativeGetIssue(labels []any, relations map[string]any) map[string]any {
	return map[string]any{"id": "SYN-6", "uuid": iid, "teamId": tid, "projectId": uid, "title": "synthetic", "status": "Backlog", "statusType": "backlog", "assignee": nil, "url": "https://linear.example/issue/SYN-6", "parentId": nil, "relations": relations, "labels": labels, "assigneeId": nil}
}
func emptyNativeRelations() map[string]any {
	return map[string]any{"blocks": []any{}, "blockedBy": []any{}, "relatedTo": []any{}, "duplicateOf": nil}
}

func TestV2NativeGetIssueShapes(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{tool("get_issue", map[string]string{"id": "string"})}, result: nativeGetIssue([]any{}, emptyNativeRelations())}
	p, _ := NewWithCaller(context.Background(), f)
	x, e := p.IssueV2(context.Background(), iid)
	if e != nil || !x.LabelsKnown || len(x.LabelIDs) != 0 || len(x.Relations) != 0 || x.RelationsKnown || !x.ParentKnown || x.Identifier != "SYN-6" {
		t.Fatal("native shape with no labels or edges misread", x, e)
	}
	// Label names are not IDs: a labelled issue's set stays unknown.
	f.result = nativeGetIssue([]any{"Synthetic label"}, emptyNativeRelations())
	if x, e = p.IssueV2(context.Background(), iid); e != nil || x.LabelsKnown {
		t.Fatal("label names treated as canonical IDs", x, e)
	}
	relations := map[string]any{
		"blocks":      []any{"SYN-7"},
		"blockedBy":   []any{map[string]any{"id": "SYN-8", "uuid": tid}},
		"relatedTo":   []any{uid, 42},
		"duplicateOf": map[string]any{"id": "SYN-9"},
	}
	f.result = nativeGetIssue([]any{}, relations)
	if x, e = p.IssueV2(context.Background(), iid); e != nil || x.RelationsKnown || len(x.Relations) != 3 {
		t.Fatal("native edges not visible, or presented as complete", x, e)
	}
	want := []Relation{
		{Type: "blocks", FromID: iid, FromIdentifier: "SYN-6", ToIdentifier: "SYN-7"},
		{Type: "blocks", FromID: tid, ToID: iid, ToIdentifier: "SYN-6"},
		{Type: "related", FromID: iid, FromIdentifier: "SYN-6", ToID: uid},
	}
	for i, edge := range want {
		got := x.Relations[i]
		if got.Type != edge.Type || got.FromID != edge.FromID || got.ToID != edge.ToID || got.ToIdentifier != edge.ToIdentifier {
			t.Fatalf("edge %d = %+v, want %+v", i, got, edge)
		}
	}
	if x.Relations[1].FromIdentifier != "SYN-8" {
		t.Fatal("object endpoint identifier lost", x.Relations[1])
	}
}
