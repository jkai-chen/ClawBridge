package linearprovider

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// routed answers each tool with its own result.
type routed struct {
	*fake
	results map[string]any
}

func (f *routed) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	f.fake.result = f.results[p.Name]
	return f.fake.CallTool(ctx, p)
}

const commentID = "c0c0c0c0-c0c0-4c0c-8c0c-c0c0c0c0c0c0"

func commentTools() []*mcp.Tool {
	return []*mcp.Tool{
		tool("save_comment", map[string]string{"issueId": "string", "body": "string"}),
		tool("list_comments", map[string]string{"issueId": "string", "limit": "number", "cursor": "string"}),
	}
}

// A comment receipt is accepted only with positive evidence of its issue:
// the receipt names it, or a read of that issue's comments finds it.
func TestCommentReceiptNeedsEvidenceOfItsIssue(t *testing.T) {
	for name, c := range map[string]struct {
		receipt  map[string]any
		listed   []any
		ok       bool
		readback bool
	}{
		"flat issue":               {map[string]any{"id": commentID, "issueId": iid}, nil, true, false},
		"nested issue":             {map[string]any{"id": commentID, "issue": map[string]any{"id": "SYN-1", "uuid": iid}}, nil, true, false},
		"other issue":              {map[string]any{"id": commentID, "issueId": uid}, nil, false, false},
		"flat and nested disagree": {map[string]any{"id": commentID, "issueId": iid, "issue": map[string]any{"id": uid}}, nil, false, false},
		"silent, found on read":    {map[string]any{"id": commentID}, []any{map[string]any{"id": commentID}}, true, true},
		"identifier only, found":   {map[string]any{"id": commentID, "issue": map[string]any{"id": "SYN-1"}}, []any{map[string]any{"id": commentID}}, true, true},
		"silent, not found":        {map[string]any{"id": commentID}, []any{map[string]any{"id": uid}}, false, true},
	} {
		f := &routed{fake: &fake{tools: commentTools()}, results: map[string]any{"save_comment": c.receipt, "list_comments": map[string]any{"comments": c.listed, "hasNextPage": false}}}
		if c.listed == nil {
			f.results["list_comments"] = map[string]any{"comments": []any{}, "hasNextPage": false}
		}
		p, _ := NewWithCaller(context.Background(), f)
		r, e := p.Comment(context.Background(), iid, "body")
		if (e == nil) != c.ok || (c.ok && (r.ID != commentID || r.IssueID != iid)) {
			t.Errorf("%s: receipt %+v, %v; want accepted=%v", name, r, e, c.ok)
		}
		read := len(f.calls) == 2 && f.calls[1] == "list_comments" && f.args[1]["issueId"] == iid
		if read != c.readback {
			t.Errorf("%s: confirming read=%v, want %v (%v)", name, read, c.readback, f.calls)
		}
		if !c.ok && e != nil && name != "other issue" && name != "flat and nested disagree" {
			if pe, ok := e.(*Error); !ok || pe.Kind != "receipt_unverified" {
				t.Errorf("%s: unconfirmed comment error %v, want receipt_unverified", name, e)
			}
		}
	}
	// Without list_comments a silent receipt cannot be confirmed.
	f := &routed{fake: &fake{tools: commentTools()[:1]}, results: map[string]any{"save_comment": map[string]any{"id": commentID}}}
	p, _ := NewWithCaller(context.Background(), f)
	if _, e := p.Comment(context.Background(), iid, "body"); e == nil {
		t.Fatal("silent comment receipt accepted without a confirming read")
	}
}

func TestRelationReceiptIsConfirmedOnlyWhenItNamesTheEdge(t *testing.T) {
	edgeTool := tool("create_issue_relation", map[string]string{"issueId": "string", "relatedIssueId": "string", "type": "string"}, "issueId", "relatedIssueId", "type")
	in := RelationInput{Action: "add", Type: "blocks", FromID: iid, ToID: uid}
	for name, c := range map[string]struct {
		result    map[string]any
		confirmed bool
	}{
		"names the edge": {map[string]any{"issueId": iid, "relatedIssueId": uid, "type": "blocks"}, true},
		"no type":        {map[string]any{"issueId": iid, "relatedIssueId": uid}, false},
		"no target":      {map[string]any{"issueId": iid, "type": "blocks"}, false},
		"silent":         {map[string]any{"ok": true}, false},
	} {
		f := &fake{tools: []*mcp.Tool{v2MutationTool("update_issue", true), edgeTool}, result: c.result}
		p, _ := NewWithCaller(context.Background(), f)
		r, e := p.Relation(context.Background(), in)
		if e != nil || r.Confirmed != c.confirmed {
			t.Errorf("%s: confirmed=%v, %v; want %v", name, r.Confirmed, e, c.confirmed)
		}
	}
}

// Native list rows carry a display id beside the uuid; a row missing its
// scope is read again by the uuid.
func TestIssuesV2FallsBackByUUIDForDisplayIDRows(t *testing.T) {
	full := map[string]any{"id": "SYN-1", "uuid": iid, "teamId": tid, "projectId": uid, "labels": []any{}}
	f := &routed{fake: &fake{tools: []*mcp.Tool{nativeIssueList(), tool("get_issue", map[string]string{"id": "string"})}}, results: map[string]any{
		"list_issues": map[string]any{"issues": []any{map[string]any{"id": "SYN-1", "uuid": iid}}, "hasNextPage": false},
		"get_issue":   full,
	}}
	p, _ := NewWithCaller(context.Background(), f)
	page, e := p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid, ProjectID: uid}})
	if e != nil || len(page.Items) != 1 || page.Items[0].ID != iid {
		t.Fatal("display-id row without scope was not read back by uuid", page, e, f.calls)
	}
	if f.args[len(f.args)-1]["id"] != iid {
		t.Fatal("fallback read did not use the uuid", f.args)
	}
}
