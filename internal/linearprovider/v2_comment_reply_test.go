package linearprovider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nativeCommentSave is save_comment exactly as the live catalogs advertised
// it on 2026-10-22 (the same in three captures).
func nativeCommentSave() *mcp.Tool {
	raw, e := os.ReadFile("testdata/save-comment-20261022.json")
	if e != nil {
		panic(e)
	}
	var tool mcp.Tool
	if e = json.Unmarshal(raw, &tool); e != nil {
		panic(e)
	}
	return &tool
}

const replyID = "c9000000-0000-4000-8000-000000000009"

// replyProvider answers save_comment with receipt and list_comments newest
// first over rows; it records every call's arguments in order.
func replyProvider(t *testing.T, receipt map[string]any, rows ...map[string]any) (*Provider, *pagedComments) {
	t.Helper()
	list := newestFirst(rows...)
	f := &pagedComments{tools: []*mcp.Tool{nativeCommentSave(), nativeCommentList()}, gen: func(n int, args map[string]any) any {
		if _, write := args["body"]; write {
			return receipt
		}
		return list(n, args)
	}}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	return p, f
}

// replyRow is the new reply as list_comments shows it: newest, with parent.
func replyRow(parent any) map[string]any {
	row := commentRow(replyID, "2026-10-23T12:00:00.010Z", "2026-10-23T12:00:00.000Z")
	row["parentId"] = parent
	row["body"] = "answer"
	return row
}

// editedReply is the reply row with a body other than the one posted.
func editedReply(parent string) map[string]any {
	row := replyRow(parent)
	row["body"] = "answer, edited"
	return row
}

// The parent proof reads the issue newest first, up to the receipt proof's
// bound: a parent on page 2 is found, one not on the issue is not.
func TestIssueHasCommentFindsAParentOnPageTwo(t *testing.T) {
	rows := minuteRows(120)
	p, f := replyProvider(t, nil, rows...)
	// Row 60 is the 60th newest, so it is on the second page of 50.
	if found, repliesTo, e := p.IssueHasComment(context.Background(), iid, issueKey, commentN(60)); e != nil || !found || repliesTo != "" || len(f.args) != 2 || f.args[1]["cursor"] != commentN(70) {
		t.Fatal("parent on page 2 not found", found, e, f.args)
	}
	p, f = replyProvider(t, nil, rows...)
	if found, _, e := p.IssueHasComment(context.Background(), iid, issueKey, commentA); e != nil || found || len(f.args) != 3 {
		t.Fatal("a comment of another issue was found", found, e, len(f.args))
	}
	// A row that names another issue is not this issue's comment.
	foreign := commentRow(commentA, "2026-10-23T11:00:00Z", "2026-10-23T11:00:00Z")
	foreign["issueId"] = uid
	p, _ = replyProvider(t, nil, foreign)
	if found, _, e := p.IssueHasComment(context.Background(), iid, issueKey, commentA); found || e != nil {
		t.Fatal("a row naming another issue proved the parent", found, e)
	}
	// The proof reads at most 4 pages: the 220th newest comment is absent.
	p, f = replyProvider(t, nil, minuteRows(250)...)
	if found, _, e := p.IssueHasComment(context.Background(), iid, issueKey, commentN(30)); e != nil || found || len(f.args) != commentPages {
		t.Fatal("a parent past the bound was found", found, e, len(f.args))
	}
	// A parent that is itself a reply names the comment it answers.
	nested := replyRow(commentN(3))
	p, _ = replyProvider(t, nil, append(minuteRows(5), nested)...)
	if found, repliesTo, e := p.IssueHasComment(context.Background(), iid, issueKey, replyID); e != nil || !found || repliesTo != commentN(3) {
		t.Fatal("a nested parent not reported", found, repliesTo, e)
	}
}

func TestReplyOnIssueSendsParentAndVerifiesTheReceipt(t *testing.T) {
	parent := commentN(5)
	rows := append(minuteRows(10), replyRow(parent))
	for name, c := range map[string]struct {
		receipt  map[string]any
		rows     []map[string]any
		kind     string
		readback bool
	}{
		"receipt names issue and parent": {map[string]any{"id": replyID, "issueId": iid, "parentId": parent}, rows, "", false},
		"receipt names a nested parent":  {map[string]any{"id": replyID, "issue": map[string]any{"id": iid}, "parent": map[string]any{"id": parent}}, rows, "", false},
		"bare receipt, row has parent":   {map[string]any{"id": replyID}, rows, "", true},
		"receipt names another parent":   {map[string]any{"id": replyID, "issueId": iid, "parentId": commentN(6)}, rows, "receipt_mismatch", false},
		"receipt names no parent":        {map[string]any{"id": replyID, "issueId": iid, "parentId": nil}, rows, "receipt_mismatch", false},
		"row has another parent":         {map[string]any{"id": replyID}, append(minuteRows(10), replyRow(commentN(6))), "receipt_mismatch", true},
		"row is top-level":               {map[string]any{"id": replyID}, append(minuteRows(10), replyRow(nil)), "receipt_mismatch", true},
		"reply not found":                {map[string]any{"id": replyID}, minuteRows(10), "receipt_unverified", true},
		"receipt body differs":           {map[string]any{"id": replyID, "issueId": iid, "parentId": parent, "body": "answer?"}, rows, "receipt_mismatch", false},
		"row body differs":               {map[string]any{"id": replyID}, append(minuteRows(10), editedReply(parent)), "receipt_mismatch", true},
		"receipt names another issue":    {map[string]any{"id": replyID, "issueId": uid, "parentId": parent}, rows, "scope_mismatch", false},
	} {
		p, f := replyProvider(t, c.receipt, c.rows...)
		r, e := p.ReplyOnIssue(context.Background(), iid, issueKey, parent, "answer")
		var pe *Error
		switch {
		case c.kind == "" && (e != nil || r.ID != replyID || r.IssueID != iid || r.ParentID != parent || r.BodySHA256 != bodySHA256("answer")):
			t.Fatal(name, "reply not accepted", r, e)
		case c.kind != "" && (!errors.As(e, &pe) || pe.Kind != c.kind):
			t.Fatal(name, "want", c.kind, "got", r, e)
		}
		// Linear ignores issueId beside parentId, so it is not sent.
		if sent := f.args[0]; sent["parentId"] != parent || sent["body"] != "answer" || len(sent) != 2 {
			t.Fatal(name, "reply arguments", sent)
		}
		if read := len(f.args) > 1; read != c.readback {
			t.Fatal(name, "readback", read, f.args)
		}
	}
}

// A catalog whose comment write drops parentId switches the capability off
// with a reason, and neither the proof nor the reply calls Linear.
func TestCommentReplyRefusedWithoutParentID(t *testing.T) {
	save := nativeCommentSave()
	delete(save.InputSchema.(map[string]any)["properties"].(map[string]any), "parentId")
	undocumented := nativeCommentSave()
	undocumented.Description = "Create or update a comment."
	for name, tools := range map[string][]*mcp.Tool{
		"no parentId":          {save, nativeCommentList()},
		"replies undocumented": {undocumented, nativeCommentList()},
		"no comment read":      {nativeCommentSave()},
	} {
		f := &pagedComments{tools: tools}
		p, _ := NewWithCaller(context.Background(), f)
		if caps, _ := p.Capabilities(context.Background()); caps.CommentReply || caps.Reasons["comment_reply"] == "" {
			t.Fatal(name, "reply capability not switched off with a reason", caps)
		}
		var pe *Error
		if _, _, e := p.IssueHasComment(context.Background(), iid, issueKey, commentA); !errors.As(e, &pe) || pe.Kind != "capability_unavailable" {
			t.Fatal(name, "proof", e)
		}
		if _, e := p.ReplyOnIssue(context.Background(), iid, issueKey, commentA, "answer"); !errors.As(e, &pe) || pe.Kind != "capability_unavailable" {
			t.Fatal(name, "reply", e)
		}
		if len(f.args) != 0 {
			t.Fatal(name, "Linear was called", f.args)
		}
	}
}
