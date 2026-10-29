package linearprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nativeIssue mirrors a native get_issue/save_issue receipt: id is the display
// identifier, uuid the canonical ID, and parentId the parent's identifier.
func nativeIssue(identifier, uuid string, parent any) map[string]any {
	return map[string]any{"id": identifier, "uuid": uuid, "teamId": tid, "projectId": nil, "parentId": parent}
}

func TestV2ParentIdentifierIsKnownWithoutInventingAUUID(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{tool("get_issue", map[string]string{"id": "string"})}, result: nativeIssue("SYN-1", iid, "SYN-2")}
	p, _ := NewWithCaller(context.Background(), f)
	x, e := p.IssueV2(context.Background(), iid)
	if e != nil || !x.ParentKnown || x.ParentID != "" || x.ParentIdentifier != "SYN-2" {
		t.Fatal("identifier-form parent not read", x, e)
	}
	f.result = nativeIssue("SYN-1", iid, nil)
	if x, e = p.IssueV2(context.Background(), iid); e != nil || !x.ParentKnown || x.ParentID != "" || x.ParentIdentifier != "" {
		t.Fatal("explicit null parent not known-absent", x, e)
	}
	f.result = nativeIssue("SYN-1", iid, uid)
	if x, e = p.IssueV2(context.Background(), iid); e != nil || x.ParentID != uid {
		t.Fatal("canonical parent lost", x, e)
	}
	for _, bad := range []any{"not an id!", 7, "syn-2"} {
		f.result = nativeIssue("SYN-1", iid, bad)
		if _, e = p.IssueV2(context.Background(), iid); e == nil {
			t.Fatal("malformed parentId accepted", bad)
		}
	}
	// A parent is read back by its identifier; the read must return it.
	f.result = nativeIssue("SYN-2", uid, nil)
	if x, e = p.IssueV2(context.Background(), "SYN-2"); e != nil || x.ID != uid || x.Identifier != "SYN-2" || f.args[len(f.args)-1]["id"] != "SYN-2" {
		t.Fatal("read by identifier failed", x, e)
	}
	_, e = p.IssueV2(context.Background(), "SYN-3")
	var pe *Error
	if !errors.As(e, &pe) || pe.Kind != "scope_mismatch" || pe.Returned == nil || pe.Returned.Identifier != "SYN-2" || pe.Returned.ID != uid || pe.Returned.Title != "" || pe.Returned.Description != "" || pe.Returned.DescriptionKnown {
		t.Fatal("identifier read accepted a different issue, or did not name what it resolves to", e)
	}
	if _, e = p.IssueV2(context.Background(), "drop table"); e == nil {
		t.Fatal("malformed reference sent")
	}
}

func TestV2ParentSetNeedsTheBoardMarkerButClearDoesNot(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{v2MutationTool("update_issue", true), tool("get_issue", map[string]string{"id": "string"})}, result: nativeIssue("SYN-1", iid, "SYN-2")}
	p, _ := NewWithCaller(context.Background(), f)
	caps, e := p.Capabilities(context.Background())
	if e != nil || !caps.Parent || !caps.ParentSameDestination {
		t.Fatal("same-destination parent capability not advertised", caps, e)
	}
	parent := uid
	set := UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Parent: Field[*string]{Present: true, Value: &parent}}
	if e = p.ValidateUpdateV2(set); e == nil {
		t.Fatal("unmarked parent set passed preflight")
	}
	if _, e = p.UpdateV2(context.Background(), set); e == nil || len(f.calls) != 0 {
		t.Fatal("unmarked parent set dispatched", e)
	}
	receipt, e := p.UpdateV2(context.Background(), set.WithSameDestinationParent())
	if e != nil || len(f.calls) != 1 || receipt.ParentIdentifier != "SYN-2" {
		t.Fatal("marked parent set or identifier receipt failed", receipt, e)
	}
	f.result = nativeIssue("SYN-1", iid, nil)
	clear := UpdateInputV2{UpdateInput: UpdateInput{ID: iid}, Parent: Field[*string]{Present: true}}
	if receipt, e = p.UpdateV2(context.Background(), clear); e != nil || len(f.calls) != 2 || !receipt.ParentKnown || receipt.ParentIdentifier != "" {
		t.Fatal("parent clear failed", receipt, e)
	}
}
