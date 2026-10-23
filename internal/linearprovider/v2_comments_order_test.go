package linearprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// minuteRows are n comments created and last updated one minute apart from
// 2026-10-23T00:00Z; row i has id commentN(i).
func minuteRows(n int) []map[string]any {
	rows := []map[string]any{}
	for i := 0; i < n; i++ {
		ts := at("2026-10-23T00:00:00Z").Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano)
		rows = append(rows, commentRow(commentN(i), ts, ts))
	}
	return rows
}

func commentN(i int) string { return fmt.Sprintf("c%07d-0000-4000-8000-000000000000", i) }

func newestFirstProvider(t *testing.T, tool *mcp.Tool, rows []map[string]any) (*Provider, *pagedComments) {
	t.Helper()
	f := &pagedComments{tools: []*mcp.Tool{tool}, gen: newestFirst(rows...)}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	return p, f
}

// Linear walks newest first, so a read cut by the row bound lacks the oldest
// comments, never the newest; the output is still ascending.
func TestV2CommentsBoundDropsTheOldest(t *testing.T) {
	withCommentClock(t, "2026-10-23T12:00:00Z")
	p, f := newestFirstProvider(t, nativeCommentList(), minuteRows(600))
	out, e := readComments(t, p, "")
	if e != nil || len(f.args) != 10 || out.Complete || !out.Truncated || out.WatermarkReason != "bound_reached" || len(out.Items) != commentRowBudgetV2 {
		t.Fatal("bounded read", len(f.args), out.WatermarkReason, len(out.Items), e)
	}
	if out.WalkDirection != "desc" || out.OutputOrder != "asc" || out.Order != "createdAt" {
		t.Fatal("order fields", out.Order, out.WalkDirection, out.OutputOrder)
	}
	for n, c := range out.Items {
		if c.ID != commentN(100+n) {
			t.Fatal("not the newest 500 in ascending order", n, c.ID)
		}
	}
}

// A comments_since read walks in update order and stops after the first page
// wholly older than the mark less the lag.
func TestV2CommentsSinceStopsAtAPageOlderThanTheMark(t *testing.T) {
	withCommentClock(t, "2026-10-23T12:00:00Z")
	p, f := newestFirstProvider(t, nativeCommentList(), minuteRows(300))
	// The mark is row 240. Pages hold rows 299-250, 249-200 and 199-150;
	// page 2 reaches past the cutoff (row 238) but page 3 is wholly older.
	out, e := readComments(t, p, "2026-10-23T04:00:00Z")
	if e != nil || len(f.args) != 3 || !out.StoppedEarly || !out.Complete || out.Truncated || out.WatermarkReason != "" || len(out.Items) != 60 {
		t.Fatal("since read did not stop at the first older page", len(f.args), out.StoppedEarly, out.WatermarkReason, len(out.Items), e)
	}
	for _, args := range f.args {
		if args["orderBy"] != "updatedAt" {
			t.Fatal("since read not walked in update order", args)
		}
	}
	if out.Order != "updatedAt" || out.WalkDirection != "desc" || out.Items[0].ID != commentN(240) || out.Items[59].ID != commentN(299) {
		t.Fatal("since read output", out.Order, out.WalkDirection, out.Items[0].ID)
	}
	if mark := watermarkTime(t, out.Watermark); !mark.Equal(at("2026-10-23T04:57:00Z")) {
		t.Fatal("watermark", mark)
	}
	// A read without a mark walks every page.
	p, f = newestFirstProvider(t, nativeCommentList(), minuteRows(300))
	if out, e = readComments(t, p, ""); e != nil || len(f.args) != 6 || out.StoppedEarly || len(out.Items) != 300 {
		t.Fatal("full read", len(f.args), out.StoppedEarly, e)
	}
}

// createdAt can trail updatedAt by milliseconds, so a row updated just before
// the mark can still be at or after it. The lag keeps such a row's page in
// the walk: a page wholly before the mark, but within the lag, never stops it.
func TestV2CommentsSinceStopKeepsTheLagForTheMillisecondQuirk(t *testing.T) {
	withCommentClock(t, "2026-10-23T12:00:00Z")
	newer := commentRow(commentA, "2026-10-23T04:01:00Z", "2026-10-23T04:01:00Z")
	before := commentRow(commentB, "2026-10-23T03:59:59.990Z", "2026-10-23T03:59:59.990Z")
	quirk := commentRow(commentC, "2026-10-23T04:00:00.007Z", "2026-10-23T03:59:59.980Z")
	p, f := commentProvider(t, commentPage(commentA, newer), commentPage(commentB, before), commentPage("", quirk))
	out, e := readComments(t, p, "2026-10-23T04:00:00Z")
	if e != nil || len(f.args) != 3 || out.StoppedEarly || !out.Complete || len(out.Items) != 2 || out.Items[0].ID != commentC || out.Items[1].ID != commentA {
		t.Fatal("a comment at the mark was missed by stopping early", len(f.args), out.Items, e)
	}
}

// The drift guard: a row later on the walk's key than the row before it, on
// the same page or across pages, marks the walk not_desc and disables the
// early stop, so the rest of the walk is read in full.
func TestV2CommentsDriftGuardFallsBackToTheFullWalk(t *testing.T) {
	withCommentClock(t, "2026-10-23T12:00:00Z")
	row := func(id, ts string) map[string]any { return commentRow(id, ts, ts) }
	old := row(commentN(1), "2026-10-23T03:00:00Z")
	older := row(commentN(2), "2026-10-23T02:00:00Z")
	late := row(commentN(3), "2026-10-23T04:03:00Z")
	for name, pages := range map[string][]any{
		"within a page": {
			commentPage(commentB, row(commentA, "2026-10-23T04:01:00Z"), row(commentB, "2026-10-23T04:02:00Z")),
			commentPage(commentN(1), old),
			commentPage("", late),
		},
		"across pages": {
			commentPage(commentA, row(commentA, "2026-10-23T04:01:00Z")),
			commentPage(commentN(1), row(commentB, "2026-10-23T04:02:00Z"), old),
			commentPage(commentN(2), older),
			commentPage("", late),
		},
	} {
		p, f := commentProvider(t, pages...)
		out, e := readComments(t, p, "2026-10-23T04:00:00Z")
		if e != nil || len(f.args) != len(pages) || out.StoppedEarly || out.WalkDirection != "not_desc" || !out.Complete || len(out.Items) != 3 || out.Items[2].ID != commentN(3) {
			t.Fatal(name, "drift not marked or the full walk not read", len(f.args), out.WalkDirection, out.Items, e)
		}
	}
	// A creation-order walk is checked the same way.
	p, _ := commentProvider(t, commentPage(commentA, old), commentPage("", row(commentA, "2026-10-23T04:01:00Z")))
	if out, e := readComments(t, p, ""); e != nil || out.Order != "createdAt" || out.WalkDirection != "not_desc" {
		t.Fatal("ascending creation-order walk not marked", out.WalkDirection, e)
	}
}

// Without update-time order in the schema, a since read walks in creation
// order and reads every page, as before.
func TestV2CommentsSinceWithoutUpdateOrderReadsEveryPage(t *testing.T) {
	withCommentClock(t, "2026-10-23T12:00:00Z")
	tool := nativeCommentList()
	tool.InputSchema.(map[string]any)["properties"].(map[string]any)["orderBy"] = map[string]any{"type": "string", "enum": []any{"createdAt"}}
	p, f := newestFirstProvider(t, tool, minuteRows(300))
	out, e := readComments(t, p, "2026-10-23T04:00:00Z")
	if e != nil || len(f.args) != 6 || out.StoppedEarly || out.Order != "createdAt" || !out.Complete || len(out.Items) != 60 || f.args[0]["orderBy"] != "createdAt" {
		t.Fatal("since read without update order", len(f.args), out.Order, out.StoppedEarly, e)
	}
}

// Linear's default order is newest first, so the comment just written is on
// the first page of the readback, however many comments the issue has.
func TestCommentReadbackFindsTheNewCommentOnPageOne(t *testing.T) {
	rows := minuteRows(300)
	rows = append(rows, commentRow(commentID, "2026-10-23T11:59:59.990Z", "2026-10-23T11:59:59.970Z"))
	list := newestFirst(rows...)
	f := &pagedComments{tools: []*mcp.Tool{commentTools()[0], nativeCommentList()}, gen: func(n int, args map[string]any) any {
		if _, write := args["body"]; write {
			return map[string]any{"id": commentID}
		}
		return list(n, args)
	}}
	p, e := NewWithCaller(context.Background(), f)
	if e != nil {
		t.Fatal(e)
	}
	r, e := p.Comment(context.Background(), iid, "body")
	if e != nil || r.ID != commentID || len(f.args) != 2 || f.args[1]["limit"] != 50 || f.args[1]["cursor"] != nil {
		t.Fatal("new comment not confirmed from page 1", r, e, f.args)
	}
}

// comments_order_direction, the v0.2.3 field, stays as a deprecated alias
// equal to comments_walk_direction in both of its values.
func TestV2CommentsOrderDirectionIsADeprecatedAlias(t *testing.T) {
	withCommentClock(t, "2026-10-23T12:00:00Z")
	row := func(id, ts string) map[string]any { return commentRow(id, ts, ts) }
	for want, pages := range map[string][]any{
		"desc":     {commentPage(commentA, row(commentA, "2026-10-23T04:02:00Z")), commentPage("", row(commentB, "2026-10-23T04:01:00Z"))},
		"not_desc": {commentPage(commentA, row(commentA, "2026-10-23T04:01:00Z")), commentPage("", row(commentB, "2026-10-23T04:02:00Z"))},
	} {
		p, _ := commentProvider(t, pages...)
		out, e := readComments(t, p, "")
		if e != nil {
			t.Fatal(want, e)
		}
		raw, _ := json.Marshal(out)
		var fields map[string]any
		if e = json.Unmarshal(raw, &fields); e != nil {
			t.Fatal(e)
		}
		if fields["comments_walk_direction"] != want || fields["comments_order_direction"] != want {
			t.Fatal(want, "alias missing or different", fields["comments_walk_direction"], fields["comments_order_direction"])
		}
	}
}

// A page without hasNextPage never stops a since read early: even when it is
// wholly older than the mark, the read ends pagination_unknown, incomplete.
func TestV2CommentsSinceNeverStopsEarlyWithoutHasNextPage(t *testing.T) {
	withCommentClock(t, "2026-10-23T12:00:00Z")
	newer := commentRow(commentA, "2026-10-23T04:01:00Z", "2026-10-23T04:01:00Z")
	old := commentRow(commentB, "2026-10-23T03:00:00Z", "2026-10-23T03:00:00Z")
	p, f := commentProvider(t, commentPage(commentA, newer), map[string]any{"comments": []any{old}, "cursor": commentB})
	out, e := readComments(t, p, "2026-10-23T04:00:00Z")
	if e != nil || len(f.args) != 2 || out.StoppedEarly || out.Complete || !out.Truncated || out.WatermarkReason != "pagination_unknown" || out.Watermark != "" || len(out.Items) != 1 {
		t.Fatal("a page without hasNextPage stopped the read early", len(f.args), out.StoppedEarly, out.WatermarkReason, e)
	}
}
