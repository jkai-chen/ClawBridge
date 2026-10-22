package linearprovider

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func updatedRow(identifier, uuid, updated string) map[string]any {
	row := listRow(identifier, uuid)
	row["labels"] = []any{}
	if updated != "" {
		row["updatedAt"] = updated
	}
	return row
}

// The pinned native "Updated after" filter is sent as UTC and checked on
// every row, so a filter Linear ignored cannot pass older issues.
func TestV2UpdatedAfterIsPinnedSentAsUTCAndCheckedPerRow(t *testing.T) {
	query := IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid, ProjectID: uid}, UpdatedAfter: "2026-10-22T11:00:00+02:00"}
	f := &fake{tools: []*mcp.Tool{nativeIssueList()}, result: map[string]any{"issues": []any{updatedRow("SYN-5", uid, "2026-10-22T09:00:00.000Z"), updatedRow("SYN-6", iid, "2026-10-22T10:30:00Z")}, "hasNextPage": false}}
	p, _ := NewWithCaller(context.Background(), f)
	page, e := p.IssuesV2(context.Background(), query)
	if e != nil || len(page.Items) != 2 || page.Items[1].UpdatedAt != "2026-10-22T10:30:00Z" {
		t.Fatal("rows at or after the bound refused", page, e)
	}
	if args := f.args[len(f.args)-1]; args["updatedAt"] != "2026-10-22T09:00:00Z" {
		t.Fatal("updated-after bound not normalized to UTC", args)
	}
	for name, row := range map[string]map[string]any{
		"older row (filter ignored)": updatedRow("SYN-7", iid, "2026-10-22T08:59:59Z"),
		"row without updatedAt":      updatedRow("SYN-8", iid, ""),
	} {
		f.result = map[string]any{"issues": []any{row}, "hasNextPage": false}
		if _, e = p.IssuesV2(context.Background(), query); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	calls := len(f.calls)
	for _, bound := range []string{"-P1D", "2026-10-22", "yesterday"} {
		if _, e = p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid}, UpdatedAfter: bound}); e == nil || len(f.calls) != calls {
			t.Fatal("relative or partial bound accepted or sent", bound, e)
		}
	}
	drifted := nativeIssueList()
	drifted.InputSchema.(map[string]any)["properties"].(map[string]any)["updatedAt"] = map[string]any{"type": "string", "description": "Updated before"}
	f = &fake{tools: []*mcp.Tool{drifted}, result: map[string]any{"issues": []any{}, "hasNextPage": false}}
	p, _ = NewWithCaller(context.Background(), f)
	caps, _ := p.Capabilities(context.Background())
	if _, e = p.IssuesV2(context.Background(), query); e == nil || len(f.calls) != 0 || caps.UpdatedAfterFilter || caps.Reasons["updated_after_filter"] == "" {
		t.Fatal("drifted updated-after filter used or not explained", e, caps)
	}
}

func TestV2IssueProjectionRequestsUpdatedAt(t *testing.T) {
	tool := nativeIssueList()
	tool.InputSchema.(map[string]any)["properties"].(map[string]any)["fields"] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []any{"id", "uuid", "teamId", "projectId", "labels", "updatedAt"}}}
	f := &fake{tools: []*mcp.Tool{tool}, result: map[string]any{"issues": []any{updatedRow("SYN-5", uid, "2026-10-22T09:00:00Z")}, "hasNextPage": false}}
	p, _ := NewWithCaller(context.Background(), f)
	if _, e := p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid, ProjectID: uid}, UpdatedAfter: "2026-10-22T09:00:00Z"}); e != nil {
		t.Fatal(e)
	}
	fields, _ := f.args[0]["fields"].([]string)
	found := false
	for _, field := range fields {
		found = found || field == "updatedAt"
	}
	if !found {
		t.Fatal("projection omits updatedAt, so the filter cannot be checked", fields)
	}
}
