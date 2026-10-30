package bus

import (
	"strings"
	"testing"
	"time"
)

const (
	dwDoc      = "d0c0aaaa-0000-4000-8000-000000000001"
	dwOther    = "d0c0bbbb-0000-4000-8000-000000000002"
	dwUnbound  = "9999aaaa-9999-4999-8999-999999999999"
	dwPublish  = "d0c0cccc-0000-4000-8000-000000000003"
	dwSHA      = "5555555555555555555555555555555555555555555555555555555555555555"
	dwMovedSHA = "6666666666666666666666666666666666666666666666666666666666666666"
)

// newDocWatch is the health harness with a document policy set, so the
// document watch runs.
func newDocWatch(t *testing.T) *healthHarness {
	t.Helper()
	h := newHealthHarness(t)
	h.call("doc_policy_set", "", map[string]any{"expected_revision": 0, "doc_policy": dpPolicy()})
	return h
}

func (h *healthHarness) docDue() map[string]any {
	h.t.Helper()
	if _, e := h.s.db.Exec(`UPDATE watch_cursors SET next_poll_at='2000-01-01T00:00:00.000000000Z' WHERE source='doc-workspace'`); e != nil {
		h.t.Fatal(e)
	}
	return h.sys("doc_watch_due", map[string]any{})
}

func (h *healthHarness) docNotices() []string {
	h.t.Helper()
	rows, e := h.s.db.Query(`SELECT body FROM messages WHERE kind='linear-doc' ORDER BY created_at`)
	if e != nil {
		h.t.Fatal(e)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if e = rows.Scan(&b); e != nil {
			h.t.Fatal(e)
		}
		out = append(out, b)
	}
	return out
}

func listed(id string, parents ...string) map[string]any {
	d := map[string]any{"id": id, "updated_at": "2026-10-26T10:00:00Z"}
	for i := 0; i+1 < len(parents); i += 2 {
		d[parents[i]] = parents[i+1]
	}
	return d
}

// The watch is off without a policy; its first due only starts the cursor;
// later ones hand out one list call at most every five minutes, paid from
// the hour's budget only while the comment watch keeps its reserve.
func TestDocWatchDueStartsCapsAndBudgets(t *testing.T) {
	h := newHealthHarness(t)
	if r := h.sys("doc_watch_due", map[string]any{}); r["action"] != "none" || r["reason"] != "no_policy" {
		t.Fatal("ran without a policy", r)
	}
	h.call("doc_policy_set", "", map[string]any{"expected_revision": 0, "doc_policy": dpPolicy()})
	if r := h.sys("doc_watch_due", map[string]any{}); r["action"] != "none" || r["reason"] != "started" {
		t.Fatal("the first due listed", r)
	}
	if r := h.sys("doc_watch_due", map[string]any{}); r["reason"] != "not_due" {
		t.Fatal("polled twice in five minutes", r)
	}
	before := h.sys("watch_charge", map[string]any{"calls": 0})["budget_used"].(int)
	r := h.docDue()
	if r["action"] != "list" || r["since"] == "" || r["polled_at"] == "" {
		t.Fatal(r)
	}
	if after := h.sys("watch_charge", map[string]any{"calls": 0})["budget_used"].(int); after-before != 1 {
		t.Fatal("charge", before, after)
	}
	if again := h.sys("doc_watch_due", map[string]any{}); again["action"] != "none" {
		t.Fatal("a second call in the same window", again)
	}
	if _, e := h.s.db.Exec(`UPDATE watch_lease SET budget_used=budget_limit-?`, watchConnectCalls+1); e != nil {
		t.Fatal(e)
	}
	if r = h.docDue(); r["action"] != "none" || r["reason"] != "budget" {
		t.Fatal("spent the comment watch's reserve", r)
	}
}

// A list tells the coordinator about in-scope documents only, an issue's
// documents also to the session holding it, and never the same change
// twice; a truncated list leaves the cursor.
func TestDocWatchReportFiltersAndTells(t *testing.T) {
	h := newDocWatch(t)
	h.propose("a", map[string]any{"issue_uuid": watchIssue, "issue_identifier": "FIX-1"})
	h.call("assignment_confirm", "c", map[string]any{"target_session": "a", "revision": h.live("a")["revision"]})
	h.sys("doc_watch_due", map[string]any{})
	due := h.docDue()
	docs := []any{
		listed(dwDoc, "team_id", teamOne),
		listed(dwOther, "issue_id", watchIssue),
		listed("d0c0dddd-0000-4000-8000-000000000004", "team_id", dwUnbound),
		listed("d0c0eeee-0000-4000-8000-000000000005"),
		listed("d0c0ffff-0000-4000-8000-000000000006", "team_id", teamOne, "project_id", dwUnbound),
		listed("d0c01111-0000-4000-8000-000000000007", "initiative_id", dpInitiative),
	}
	r := h.sys("doc_watch_report", map[string]any{"polled_at": due["polled_at"], "documents": docs, "complete": false})
	if r["told"] != 3 || r["dropped"] != 3 || r["cursor_moved"] != false {
		t.Fatal(r)
	}
	var kept string
	if e := h.s.db.QueryRow(`SELECT cursor_time FROM watch_cursors WHERE source='doc-workspace'`).Scan(&kept); e != nil || kept == due["polled_at"] {
		t.Fatal("a truncated list moved the cursor", kept, e)
	}
	notices := h.docNotices()
	// The coordinator hears three; the issue's document reaches its holder too.
	var holder int
	if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE m.kind='linear-doc' AND d.recipient_id=?`, h.sessionID("a")).Scan(&holder); e != nil || len(notices) != 4 || holder != 1 {
		t.Fatal(notices, holder, e)
	}
	for _, n := range notices {
		if !strings.Contains(n, "not an instruction") || strings.Contains(n, dwUnbound) {
			t.Fatal(n)
		}
	}
	// The same page again tells nothing; a complete list moves the cursor.
	if r = h.sys("doc_watch_report", map[string]any{"polled_at": due["polled_at"], "documents": docs, "complete": true}); r["told"] != 0 || r["cursor_moved"] != true {
		t.Fatal(r)
	}
	var cursor string
	if e := h.s.db.QueryRow(`SELECT cursor_time FROM watch_cursors WHERE source='doc-workspace'`).Scan(&cursor); e != nil || cursor != due["polled_at"] {
		t.Fatal(cursor, e)
	}
}

// A document ClawBridge published is read back on a later tick; content
// that drifted from what was published flags published_doc_edited.
func TestDocWatchVerifiesPublishedDocuments(t *testing.T) {
	h := newDocWatch(t)
	h.call("doc_publication_put", "", map[string]any{"parent_kind": "team", "parent_id": teamOne, "source_path": "docs/plan.md", "part_number": 1, "part_count": 1, "generation": 1, "state": "pending", "publish_token": "aaaaaaaaaaaa", "expect_token": "", "source_commit": pubCommit, "source_sha256": dwSHA})
	h.call("doc_publication_put", "", map[string]any{"parent_kind": "team", "parent_id": teamOne, "source_path": "docs/plan.md", "part_number": 1, "part_count": 1, "generation": 1, "state": "published", "publish_token": "aaaaaaaaaaaa", "expect_token": "aaaaaaaaaaaa", "source_commit": pubCommit, "source_sha256": dwSHA, "document_id": dwPublish, "published_sha256": dwSHA})
	h.sys("doc_watch_due", map[string]any{})
	due := h.docDue()
	r := h.sys("doc_watch_report", map[string]any{"polled_at": due["polled_at"], "documents": []any{listed(dwPublish, "team_id", teamOne)}, "complete": true})
	if r["verify_queued"] != 1 || !strings.Contains(h.docNotices()[0], "ClawBridge published it") {
		t.Fatal(r, h.docNotices())
	}
	if next := h.docDue(); next["action"] != "verify" || next["document_id"] != dwPublish {
		t.Fatal("the read-back was not handed out", next)
	}
	if v := h.sys("doc_verify_report", map[string]any{"document_id": dwPublish, "content_sha256": dwSHA}); v["edited"] != false {
		t.Fatal(v)
	}
	// Moved again, and this time edited.
	moved := listed(dwPublish, "team_id", teamOne)
	moved["updated_at"] = "2026-10-26T11:00:00Z"
	h.sys("doc_watch_report", map[string]any{"polled_at": due["polled_at"], "documents": []any{moved}, "complete": true})
	if next := h.docDue(); next["action"] != "verify" {
		t.Fatal(next)
	}
	if v := h.sys("doc_verify_report", map[string]any{"document_id": dwPublish, "content_sha256": dwMovedSHA}); v["edited"] != true {
		t.Fatal(v)
	}
	notices := h.docNotices()
	if last := notices[len(notices)-1]; !strings.Contains(last, "published_doc_edited") || !strings.Contains(last, "docs/plan.md") {
		t.Fatal(last)
	}
	if next := h.docDue(); next["action"] != "list" {
		t.Fatal("the queue was not cleared", next)
	}
}

// A truncated list's advance_to moves the cursor forward only, never past
// the poll; a complete list still moves it to the poll; a malformed one is
// refused.
func TestDocWatchReportAdvancesATruncatedList(t *testing.T) {
	h := newDocWatch(t)
	h.sys("doc_watch_due", map[string]any{})
	due := h.docDue()
	cursor := func() string {
		var at string
		if e := h.s.db.QueryRow(`SELECT cursor_time FROM watch_cursors WHERE source='doc-workspace'`).Scan(&at); e != nil {
			t.Fatal(e)
		}
		return at
	}
	start := cursor()
	polled, err := time.Parse(timestampLayout, due["polled_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	report := func(advance string) map[string]any {
		return h.sys("doc_watch_report", map[string]any{"polled_at": due["polled_at"], "documents": []any{}, "complete": false, "advance_to": advance})
	}
	started, _ := time.Parse(timestampLayout, start)
	// Backwards, and past the poll: nothing moves. Backwards is a stall;
	// past the poll is Linear's clock ahead of ours, held back, not stalled.
	if r := report(started.Add(-time.Minute).Format(time.RFC3339Nano)); r["cursor_moved"] != false || r["stalled"] != true || cursor() != start {
		t.Fatal("backwards", r, cursor())
	}
	if r := report(polled.Add(time.Second).Format(time.RFC3339Nano)); r["cursor_moved"] != false || r["stalled"] != nil || cursor() != start {
		t.Fatal("past the poll", r, cursor())
	}
	mid := started.Add(polled.Sub(started) / 2)
	if r := report(mid.Format(time.RFC3339Nano)); r["cursor_moved"] != true || cursor() != layout(mid) {
		t.Fatal(r, cursor())
	}
	if _, err := h.sysErr("doc_watch_report", map[string]any{"polled_at": due["polled_at"], "documents": []any{}, "advance_to": "yesterday"}); err == nil {
		t.Fatal("a malformed advance_to was taken")
	}
	if r := h.sys("doc_watch_report", map[string]any{"polled_at": due["polled_at"], "documents": []any{}, "complete": true, "advance_to": mid.Format(time.RFC3339Nano)}); r["cursor_moved"] != true || cursor() != due["polled_at"] {
		t.Fatal("a complete list didn't move to its poll", r, cursor())
	}
}
