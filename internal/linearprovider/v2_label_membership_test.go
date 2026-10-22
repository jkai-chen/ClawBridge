package linearprovider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nativeIssueList mirrors the documented native list_issues filters.
func nativeIssueList() *mcp.Tool {
	return &mcp.Tool{Name: "list_issues", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
		"team":            map[string]any{"type": "string", "description": "Team name or ID"},
		"project":         map[string]any{"type": "string", "description": "Project name, ID, identifier (e.g., P-ENG-123), or slug"},
		"label":           map[string]any{"type": "string", "description": "Label name or ID"},
		"includeArchived": map[string]any{"type": "boolean", "default": false, "description": "Include archived items"},
		"limit":           map[string]any{"type": "number", "default": 50, "maximum": 250},
		"cursor":          map[string]any{"type": "string"},
		"updatedAt":       map[string]any{"type": "string", "description": "Updated after: ISO-8601 date/duration (e.g., -P1D)"},
	}}}
}

// listRow mirrors a default native list_issues row: display id, uuid and
// label names, with no assignee or parent keys.
func listRow(identifier, uuid string) map[string]any {
	return map[string]any{"id": identifier, "uuid": uuid, "title": "synthetic", "status": "Backlog", "labels": []any{"Synthetic label"}, "teamId": tid, "projectId": uid}
}

func TestV2LabelMembershipProofSendsOnlyUUIDsAndIsBounded(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{nativeLabelList(), nativeIssueList()}, result: map[string]any{"issues": []any{listRow("SYN-5", uid), listRow("SYN-6", iid)}, "hasNextPage": false}}
	p, _ := NewWithCaller(context.Background(), f)
	found, complete, err := p.IssueHasLabel(context.Background(), iid, tid, uid, tid)
	if err != nil || !found || !complete {
		t.Fatal("labelled issue not found", found, complete, err)
	}
	args := f.args[len(f.args)-1]
	if args["label"] != tid || args["team"] != tid || args["project"] != uid || args["includeArchived"] != true || args["limit"] != 250 {
		t.Fatal("membership query not pinned to canonical IDs", args)
	}
	if _, _, err = p.IssueHasLabel(context.Background(), iid, tid, "", "Synthetic label"); err == nil {
		t.Fatal("label name sent as a filter")
	}
	f.result = map[string]any{"issues": []any{}, "hasNextPage": false}
	if found, complete, err = p.IssueHasLabel(context.Background(), iid, tid, "", tid); err != nil || found || !complete {
		t.Fatal("absent label not reported as proven absent", found, complete, err)
	}
	if _, present := f.args[len(f.args)-1]["project"]; present {
		t.Fatal("team-board check sent a project filter")
	}
	pages := 0
	f2 := &pagingFake{fake: f, pages: &pages}
	p, _ = NewWithCaller(context.Background(), f2)
	if found, complete, err = p.IssueHasLabel(context.Background(), iid, tid, uid, tid); err != nil || found || complete || pages != labelMembershipPages {
		t.Fatal("bound exceeded must be unknown after 4 pages", found, complete, err, pages)
	}
}

// pagingFake returns a fresh cursor on every page so the bound is reached.
type pagingFake struct {
	*fake
	pages *int
}

func (f *pagingFake) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	*f.pages++
	f.fake.result = map[string]any{"issues": []any{listRow("SYN-5", uid)}, "hasNextPage": true, "nextCursor": "page-" + strings.Repeat("x", *f.pages)}
	return f.fake.CallTool(ctx, params)
}

func TestV2LabelNamesAndCatalog(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{tool("get_issue", map[string]string{"id": "string"}), nativeLabelList(), nativeIssueList()}, result: nativeGetIssue([]any{"Bug", "Ops"}, emptyNativeRelations())}
	p, _ := NewWithCaller(context.Background(), f)
	x, e := p.IssueV2(context.Background(), iid)
	if e != nil || x.LabelsKnown || len(x.LabelNames) != 2 || x.LabelNames[0] != "Bug" {
		t.Fatal("label names not kept as names", x, e)
	}
	f.result = map[string]any{"labels": []any{map[string]any{"id": tid, "name": "Bug", "archivedAt": "2026-10-01T00:00:00Z"}, map[string]any{"id": uid, "name": "Area", "isGroup": true}}, "hasNextPage": false}
	page, e := p.LabelCatalog(context.Background(), tid, PageOptions{})
	args := f.args[len(f.args)-1]
	if e != nil || len(page.Items) != 2 || args["includeArchived"] != true || args["includeGroups"] != true {
		t.Fatal("catalog did not include archived labels and groups", page, args, e)
	}
	filtered, _ := json.Marshal(Label{ID: tid, FilterScoped: true, EligibilityKnown: true})
	explicit, _ := json.Marshal(Label{ID: tid, TeamID: tid, EligibilityKnown: true})
	if strings.Contains(string(filtered), `"global"`) || !strings.Contains(string(explicit), `"global":false`) {
		t.Fatal("global must be omitted only for filter-scoped rows", string(filtered), string(explicit))
	}
}

func TestV2LabelMembershipCapabilityPinsTheLabelFilter(t *testing.T) {
	drifted := nativeIssueList()
	drifted.InputSchema.(map[string]any)["properties"].(map[string]any)["label"] = map[string]any{"type": "string", "description": "Label name"}
	p, _ := NewWithCaller(context.Background(), &fake{tools: []*mcp.Tool{nativeLabelList(), drifted}})
	caps, _ := p.Capabilities(context.Background())
	if caps.LabelMembershipProof || !strings.Contains(caps.Reasons["label_membership_proof"], "label filter") {
		t.Fatal("drifted label filter still trusted", caps)
	}
	if _, _, err := p.IssueHasLabel(context.Background(), iid, tid, "", tid); err == nil {
		t.Fatal("membership check ran without the pinned filter")
	}
}

// Native list rows name their labels, so the pinned server-side label filter
// is the membership proof for the page; a drifted filter refuses.
func TestV2LabelFilterOnNativeRowsUsesTheServerFilterAsProof(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{nativeIssueList()}, result: map[string]any{"issues": []any{listRow("SYN-5", uid)}, "hasNextPage": false}}
	p, _ := NewWithCaller(context.Background(), f)
	page, e := p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid, ProjectID: uid}, LabelID: iid})
	if e != nil || len(page.Items) != 1 || page.Items[0].LabelsKnown {
		t.Fatal("native label filter refused or overclaimed", page, e)
	}
	if args := f.args[len(f.args)-1]; args["label"] != iid {
		t.Fatal("label filter not sent as the canonical UUID", args)
	}
	// A row whose label set is known and lacks the label contradicts the filter.
	known := listRow("SYN-6", uid)
	known["labels"] = []any{}
	f.result = map[string]any{"issues": []any{known}, "hasNextPage": false}
	if _, e = p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid, ProjectID: uid}, LabelID: iid}); e == nil {
		t.Fatal("row without the label accepted")
	}
	drifted := nativeIssueList()
	drifted.InputSchema.(map[string]any)["properties"].(map[string]any)["label"] = map[string]any{"type": "string", "description": "Label name"}
	f = &fake{tools: []*mcp.Tool{drifted}, result: map[string]any{"issues": []any{listRow("SYN-5", uid)}, "hasNextPage": false}}
	p, _ = NewWithCaller(context.Background(), f)
	if _, e = p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid, ProjectID: uid}, LabelID: iid}); e == nil {
		t.Fatal("drifted label filter accepted as proof")
	}
}
