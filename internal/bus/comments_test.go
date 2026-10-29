package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	watchIssue = "00000000-0000-4000-8000-0000000000a1"
	otherIssue = "00000000-0000-4000-8000-0000000000a2"
	humanOne   = "00000000-0000-4000-8000-0000000000c1"
	humanTwo   = "00000000-0000-4000-8000-0000000000c2"
	humanThree = "00000000-0000-4000-8000-0000000000c3"
	ourComment = "00000000-0000-4000-8000-0000000000d1"
	bodyDigest = "0000000000000000000000000000000000000000000000000000000000000001"
)

// watchHarness has session a assigned to watchIssue, confirmed by the
// coordinator c, which also holds the comment watch lease.
type watchHarness struct {
	*harness
	token string
	gen   int
}

func newWatchHarness(t *testing.T) *watchHarness {
	t.Helper()
	h := newHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	w := &watchHarness{harness: h}
	w.assign("a", watchIssue)
	l := h.call("comment_watch_lease", "c", map[string]any{"action": "acquire"}).(map[string]any)
	w.token, w.gen = l["lease_token"].(string), l["generation"].(int)
	return w
}

func (w *watchHarness) assign(session, issue string) {
	w.t.Helper()
	w.propose(session, map[string]any{"issue_uuid": issue, "issue_identifier": "FIX-1"})
	w.call("assignment_confirm", "c", map[string]any{"target_session": session, "revision": w.live(session)["revision"]})
}

func (w *watchHarness) fenced(a map[string]any) map[string]any {
	a["lease_token"], a["generation"] = w.token, w.gen
	return a
}

func comment(id, parent string, created time.Time) map[string]any {
	return map[string]any{"comment_id": id, "parent_id": parent, "created_at": created.UTC().Format(time.RFC3339Nano), "author_kind": "unknown"}
}

// poll runs one due and one report for issue, as the watcher does.
func (w *watchHarness) poll(issue string, comments ...map[string]any) map[string]any {
	w.t.Helper()
	w.call("comment_watch_due", "c", w.fenced(map[string]any{}))
	list := []any{}
	for _, c := range comments {
		list = append(list, c)
	}
	return w.call("comment_watch_report", "c", w.fenced(map[string]any{"issue_uuid": issue, "comments": list})).(map[string]any)
}

func (w *watchHarness) reserve(session, op string, a map[string]any) (map[string]any, error) {
	a["operation_id"], a["body_sha256"] = op, bodyDigest
	if _, ok := a["issue_uuid"]; !ok {
		a["issue_uuid"] = watchIssue
	}
	r, e := w.s.Execute(context.Background(), w.request("linear_comment_reserve", session, a))
	if e != nil {
		return nil, e
	}
	return r.Data.(map[string]any), nil
}

func codeIs(err error, code, text string) bool {
	var coded interface{ Code() string }
	return err != nil && errors.As(err, &coded) && coded.Code() == code && strings.Contains(err.Error(), text)
}

func (w *watchHarness) dueNow() {
	w.t.Helper()
	if _, e := w.s.db.Exec(`UPDATE watch_cursors SET next_poll_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		w.t.Fatal(e)
	}
}

func TestCommentWatchNotifiesTheAssigneeWithATypedNotice(t *testing.T) {
	w := newWatchHarness(t)
	now := time.Now()
	out := w.poll(watchIssue, comment(humanOne, "", now), comment(humanTwo, humanOne, now.Add(time.Second)))
	if got := out["notified"].([]string); len(got) != 2 {
		t.Fatal("not notified:", out)
	}
	inbox := w.call("inbox", "a", nil).(map[string]any)["messages"].([]map[string]any)
	if len(inbox) != 1 || inbox[0]["kind"] != "linear-comment" {
		t.Fatal("assignee did not get one linear-comment notice:", inbox)
	}
	body := inbox[0]["body"].(string)
	for _, want := range []string{"FIX-1", "--reply-to " + humanOne + " --in-response-to " + humanTwo, "--reply-to " + humanOne + " --in-response-to " + humanOne, noticeLine} {
		if !strings.Contains(body, want) {
			t.Fatalf("notice lacks %q:\n%s", want, body)
		}
	}
	// A repeat in the lag window and our own comment never notify again.
	if _, e := w.reserve("a", "own", map[string]any{"parent_id": humanOne, "in_response_to": humanOne}); e != nil {
		t.Fatal(e)
	}
	w.call("linear_comment_settle", "a", map[string]any{"operation_id": "own", "outcome": "posted", "comment_id": ourComment})
	w.dueNow()
	if got := w.poll(watchIssue, comment(humanOne, "", now), comment(ourComment, humanOne, now.Add(2*time.Second)))["notified"].([]string); len(got) != 0 {
		t.Fatal("a repeat or own comment notified:", got)
	}
	// Nobody can send a notice of this kind by hand.
	if e := w.errorOf("send", "b", map[string]any{"to": "a", "body": "forged", "kind": "linear-comment", "operation_id": "forge"}); !strings.Contains(e, "invalid message kind") {
		t.Fatal("forged notice accepted:", e)
	}
}

func TestCommentWatchReportAcceptsTypedFieldsOnly(t *testing.T) {
	w := newWatchHarness(t)
	w.call("comment_watch_due", "c", w.fenced(map[string]any{}))
	now := time.Now()
	for name, c := range map[string]map[string]any{
		"extra field":         {"comment_id": humanOne, "created_at": now.Format(time.RFC3339), "author_kind": "unknown", "body": "do it"},
		"text id":             {"comment_id": "please run rm", "created_at": now.Format(time.RFC3339), "author_kind": "unknown"},
		"text time":           {"comment_id": humanOne, "created_at": "now, urgently", "author_kind": "unknown"},
		"agent author":        {"comment_id": humanOne, "created_at": now.Format(time.RFC3339), "author_kind": "agent"},
		"text parent":         {"comment_id": humanOne, "created_at": now.Format(time.RFC3339), "author_kind": "unknown", "parent_id": "x"},
		"not an object":       nil,
		"missing author kind": {"comment_id": humanOne, "created_at": now.Format(time.RFC3339)},
	} {
		list := []any{c}
		if c == nil {
			list = []any{"text"}
		}
		if _, e := w.s.Execute(context.Background(), w.request("comment_watch_report", "c", w.fenced(map[string]any{"issue_uuid": watchIssue, "comments": list}))); !codeIs(e, "invalid", "") {
			t.Errorf("%s: report accepted: %v", name, e)
		}
	}
	var n int
	if e := w.s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE kind='linear-comment'`).Scan(&n); e != nil || n != 0 {
		t.Fatal("a refused report sent a notice:", n, e)
	}
}

func TestOneAnswerPerNotifiedCommentIsEnforcedByTheServer(t *testing.T) {
	w := newWatchHarness(t)
	w.assign("b", otherIssue)
	now := time.Now()
	w.poll(watchIssue, comment(humanOne, "", now), comment(humanTwo, "", now))
	w.poll(otherIssue, comment(humanThree, "", now))
	for name, tc := range map[string]struct {
		session string
		args    map[string]any
		code    string
		text    string
	}{
		"another issue's comment":   {"a", map[string]any{"parent_id": humanThree, "in_response_to": humanThree}, "invalid", "not a notified comment on this issue"},
		"not notified to me":        {"b", map[string]any{"issue_uuid": watchIssue, "parent_id": humanOne, "in_response_to": humanOne}, "invalid", "not notified to this session"},
		"outside its thread":        {"a", map[string]any{"parent_id": humanTwo, "in_response_to": humanOne}, "invalid", "parent_id must be " + humanOne},
		"never notified":            {"a", map[string]any{"parent_id": ourComment, "in_response_to": ourComment}, "invalid", "not a notified comment"},
		"not a UUID":                {"a", map[string]any{"in_response_to": "humanOne"}, "invalid", "Linear UUID"},
		"reserved marker namespace": {"a", map[string]any{"operation_id": "marker:x:claim:comment"}, "invalid", "reserved"},
	} {
		op := "op-" + strings.NewReplacer(" ", "-", "'", "").Replace(name)
		if v, ok := tc.args["operation_id"].(string); ok {
			op = v
		}
		if _, e := w.reserve(tc.session, op, tc.args); !codeIs(e, tc.code, tc.text) {
			t.Errorf("%s: %v", name, e)
		}
	}
	// A threaded answer without in_response_to is tied to the thread's one
	// unanswered comment by the server.
	got, e := w.reserve("a", "answer-1", map[string]any{"parent_id": humanOne})
	if e != nil || got["in_response_to"] != humanOne {
		t.Fatal("server did not derive in_response_to:", got, e)
	}
	if _, e = w.reserve("a", "answer-2", map[string]any{"parent_id": humanOne, "in_response_to": humanOne}); !codeIs(e, "conflict", "already has an answer") {
		t.Fatal("second answer accepted:", e)
	}
	// A top-level post is allowed but told where answers go.
	got, e = w.reserve("a", "top", map[string]any{})
	if e != nil || !strings.Contains(str(got, "hint"), "1 unanswered comments") {
		t.Fatal("top-level post without hint:", got, e)
	}
}

func TestThreadWithTwoUnansweredCommentsNeedsAnExplicitAnswer(t *testing.T) {
	w := newWatchHarness(t)
	now := time.Now()
	w.poll(watchIssue, comment(humanOne, "", now), comment(humanTwo, humanOne, now))
	if _, e := w.reserve("a", "ambiguous", map[string]any{"parent_id": humanOne}); !codeIs(e, "invalid", "2 unanswered comments") {
		t.Fatal(e)
	}
	if got, e := w.reserve("a", "explicit", map[string]any{"parent_id": humanOne, "in_response_to": humanTwo}); e != nil || got["in_response_to"] != humanTwo {
		t.Fatal(got, e)
	}
}

func TestCommentCapCountsEveryCommentPerIssuePerHour(t *testing.T) {
	w := newWatchHarness(t)
	for i, op := range []string{"one", "two", "three"} {
		if _, e := w.reserve("a", op, map[string]any{}); e != nil {
			t.Fatal(i, e)
		}
	}
	if _, e := w.reserve("b", "four", map[string]any{}); !codeIs(e, "rate_limited", "next slot at") {
		t.Fatal("fourth comment in the hour accepted:", e)
	}
	// Another issue has its own cap; a failed attempt frees its slot.
	if _, e := w.reserve("a", "elsewhere", map[string]any{"issue_uuid": otherIssue}); e != nil {
		t.Fatal(e)
	}
	w.call("linear_comment_settle", "a", map[string]any{"operation_id": "three", "outcome": "failed"})
	if _, e := w.reserve("b", "four", map[string]any{}); e != nil {
		t.Fatal("a failed reservation still held its slot:", e)
	}
	// Retrying the failed operation is a new attempt and counts again.
	if _, e := w.reserve("a", "three", map[string]any{}); !codeIs(e, "rate_limited", "") {
		t.Fatal("retry of a failed reservation skipped the cap:", e)
	}
	// The window is the database's last hour.
	if _, e := w.s.db.Exec(`UPDATE linear_comment_ledger SET reserved_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		t.Fatal(e)
	}
	if _, e := w.reserve("a", "three", map[string]any{}); e != nil {
		t.Fatal("cap did not slide:", e)
	}
}

func TestCommentReservationRetriesAndSettles(t *testing.T) {
	w := newWatchHarness(t)
	first, e := w.reserve("a", "op", map[string]any{})
	if e != nil {
		t.Fatal(e)
	}
	again, e := w.reserve("a", "op", map[string]any{})
	if e != nil || again["replay"] != true || again["state"] != "pending" {
		t.Fatal("exact retry not replayed:", again, e)
	}
	other := map[string]any{"operation_id": "op", "body_sha256": strings.Repeat("2", 64), "issue_uuid": watchIssue}
	if _, e = w.s.Execute(context.Background(), w.request("linear_comment_reserve", "a", other)); !codeIs(e, "conflict", "different comment") {
		t.Fatal("a different body under one operation_id accepted:", e)
	}
	// Another session cannot settle it, so no one can claim a comment ID.
	if _, e = w.s.Execute(context.Background(), w.request("linear_comment_settle", "b", map[string]any{"operation_id": "op", "outcome": "posted", "comment_id": humanOne})); !codeIs(e, "not_found", "") {
		t.Fatal("another session settled the reservation:", e)
	}
	w.call("linear_comment_settle", "a", map[string]any{"operation_id": "op", "outcome": "posted", "comment_id": ourComment})
	w.call("linear_comment_settle", "a", map[string]any{"operation_id": "op", "outcome": "posted", "comment_id": ourComment})
	if e := w.errorOf("linear_comment_settle", "a", map[string]any{"operation_id": "op", "outcome": "posted", "comment_id": humanOne}); !strings.Contains(e, "another comment") {
		t.Fatal(e)
	}
	if e := w.errorOf("linear_comment_settle", "a", map[string]any{"operation_id": "op", "outcome": "failed"}); !strings.Contains(e, "already posted") {
		t.Fatal(e)
	}
	posted, e := w.reserve("a", "op", map[string]any{})
	if e != nil || posted["state"] != "posted" || posted["comment_id"] != ourComment || first["operation_digest"] != posted["operation_digest"] {
		t.Fatal("a posted reservation is not terminal:", posted, e)
	}
	flags := w.call("linear_comments_posted", "b", map[string]any{"issue_uuid": watchIssue, "comment_ids": []any{ourComment, humanOne}}).(map[string]any)
	mine := flags["posted"].(map[string]any)
	if len(mine) != 1 || mine[ourComment].(map[string]any)["session_name"] != "a" {
		t.Fatal("posted flags wrong:", flags)
	}
}

func TestYoungPendingHoldsOwnershipAndOldPendingMatchesOnlyInItsWindow(t *testing.T) {
	w := newWatchHarness(t)
	if _, e := w.reserve("a", "op", map[string]any{}); e != nil {
		t.Fatal(e)
	}
	flags := w.call("linear_comments_posted", "c", map[string]any{"issue_uuid": watchIssue, "comment_ids": []any{}}).(map[string]any)
	if flags["young_pending"] != true || len(flags["pending"].([]any)) != 0 {
		t.Fatal("a fresh reservation is not young:", flags)
	}
	reserved := time.Now().Add(-30 * time.Minute).UTC()
	if _, e := w.s.db.Exec(`UPDATE linear_comment_ledger SET reserved_at=?`, layout(reserved)); e != nil {
		t.Fatal(e)
	}
	flags = w.call("linear_comments_posted", "c", map[string]any{"issue_uuid": watchIssue, "comment_ids": []any{}}).(map[string]any)
	pending := flags["pending"].([]any)
	if flags["young_pending"] != false || len(pending) != 1 {
		t.Fatal(flags)
	}
	digest := pending[0].(map[string]any)["operation_digest"]
	match := func(created time.Time) error {
		_, e := w.s.Execute(context.Background(), w.request("linear_comment_settle_match", "c", w.fenced(map[string]any{"operation_digest": digest, "issue_uuid": watchIssue, "comment_id": ourComment, "created_at": created.Format(time.RFC3339Nano)})))
		return e
	}
	if _, e := w.s.Execute(context.Background(), w.request("linear_comment_settle_match", "c", w.fenced(map[string]any{"operation_digest": digest, "issue_uuid": otherIssue, "comment_id": ourComment, "created_at": reserved.Add(time.Minute).Format(time.RFC3339Nano)}))); !codeIs(e, "invalid", "another issue") {
		t.Fatal("a comment on another issue was matched:", e)
	}
	if e := match(reserved.Add(20 * time.Minute)); !codeIs(e, "invalid", "outside") {
		t.Fatal("a comment outside the window was matched:", e)
	}
	if e := match(reserved.Add(-5 * time.Minute)); !codeIs(e, "invalid", "outside") {
		t.Fatal("a comment before the window was matched:", e)
	}
	if e := match(reserved.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e := match(reserved.Add(time.Minute)); !codeIs(e, "conflict", "no longer pending") {
		t.Fatal("a reservation matched twice:", e)
	}
}

func TestCommentWatchLeaseIsFenced(t *testing.T) {
	w := newWatchHarness(t)
	if e := w.errorOf("comment_watch_lease", "b", map[string]any{"action": "acquire"}); !strings.Contains(e, "held by session") {
		t.Fatal(e)
	}
	stale := w.fenced(map[string]any{})
	if _, e := w.s.db.Exec(`UPDATE watch_lease SET expires_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		t.Fatal(e)
	}
	// The same session's second process mints a new token on takeover.
	l := w.call("comment_watch_lease", "c", map[string]any{"action": "acquire"}).(map[string]any)
	if l["generation"] != 2 || l["lease_token"] == w.token {
		t.Fatal("takeover kept the old fence:", l)
	}
	for _, op := range []string{"comment_watch_due", "comment_watch_report", "linear_comment_settle_match"} {
		a := map[string]any{"lease_token": stale["lease_token"], "generation": stale["generation"]}
		if _, e := w.s.Execute(context.Background(), w.request(op, "c", a)); !codeIs(e, "conflict", "lease was lost") {
			t.Errorf("%s passed with a stale lease: %v", op, e)
		}
	}
}

func TestCommentWatchBudgetIsChargedBeforeTheReads(t *testing.T) {
	w := newWatchHarness(t)
	w.assign("b", otherIssue)
	if _, e := w.s.db.Exec(`UPDATE watch_lease SET budget_limit=3`); e != nil {
		t.Fatal(e)
	}
	due := w.call("comment_watch_due", "c", w.fenced(map[string]any{})).(map[string]any)
	if len(due["issues"].([]any)) != 1 || due["budget_used"] != 3 {
		t.Fatal("budget not enforced up front:", due)
	}
	// The cycle's report is refused after a takeover; its calls stay charged.
	if _, e := w.s.db.Exec(`UPDATE watch_lease SET expires_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		t.Fatal(e)
	}
	l := w.call("comment_watch_lease", "b", map[string]any{"action": "acquire", "budget": 3}).(map[string]any)
	if l["budget_used"] != 3 {
		t.Fatal("takeover reset the hour's budget:", l)
	}
	again := w.call("comment_watch_due", "b", map[string]any{"lease_token": l["lease_token"], "generation": l["generation"]}).(map[string]any)
	if len(again["issues"].([]any)) != 0 || again["budget_exhausted"] != true {
		t.Fatal("polled past the hour's budget:", again)
	}
}

func TestCommentWatchResumesAcrossATransfer(t *testing.T) {
	w := newWatchHarness(t)
	w.poll(watchIssue)
	var cursor, started string
	if e := w.s.db.QueryRow(`SELECT cursor_time,watch_started_at FROM watch_cursors`).Scan(&cursor, &started); e != nil {
		t.Fatal(e)
	}
	w.call("assignment_release", "c", map[string]any{"target_session": "a", "revision": w.live("a")["revision"]})
	w.dueNow()
	if due := w.call("comment_watch_due", "c", w.fenced(map[string]any{})).(map[string]any); len(due["issues"].([]any)) != 0 {
		t.Fatal("a released issue is still polled:", due)
	}
	gap := time.Now()
	w.assign("b", watchIssue)
	var resumed, kept string
	if e := w.s.db.QueryRow(`SELECT cursor_time,watch_started_at FROM watch_cursors`).Scan(&resumed, &kept); e != nil || resumed != cursor || kept != started {
		t.Fatal("transfer reset the watch:", resumed, kept, e)
	}
	got := w.poll(watchIssue, comment(humanOne, "", gap))["notified"].([]string)
	inbox := w.call("inbox", "b", nil).(map[string]any)["messages"].([]map[string]any)
	if len(got) != 1 || len(inbox) != 1 {
		t.Fatal("a comment from the transfer gap was lost:", got, inbox)
	}
}

func TestCommentWatchFailedPollKeepsTheCursor(t *testing.T) {
	w := newWatchHarness(t)
	w.poll(watchIssue)
	var before string
	if e := w.s.db.QueryRow(`SELECT cursor_time FROM watch_cursors`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	for _, failure := range []string{"young_pending", "ownership_unavailable", "read_failed"} {
		w.dueNow()
		w.call("comment_watch_due", "c", w.fenced(map[string]any{}))
		w.call("comment_watch_report", "c", w.fenced(map[string]any{"issue_uuid": watchIssue, "error": failure}))
		var after string
		if e := w.s.db.QueryRow(`SELECT cursor_time FROM watch_cursors`).Scan(&after); e != nil || after != before {
			t.Fatal(failure, "advanced the cursor:", before, after, e)
		}
	}
	// A comment created before the cursor but after the watch started
	// (visible late) still notifies: the server never filters on the cursor.
	if _, e := w.s.db.Exec(`UPDATE watch_cursors SET watch_started_at=?`, layout(time.Now().Add(-time.Hour))); e != nil {
		t.Fatal(e)
	}
	w.dueNow()
	late := w.poll(watchIssue, comment(humanOne, "", time.Now().Add(-30*time.Minute)))["notified"].([]string)
	if len(late) != 1 {
		t.Fatal("late-visible comment dropped:", late)
	}
}

// A comment already recorded for one reservation cannot settle another: a
// conflict, never the unique index's raw error.
func TestACommentSettlesOnlyOneReservation(t *testing.T) {
	w := newWatchHarness(t)
	for _, op := range []string{"first", "second", "old"} {
		if _, e := w.reserve("a", op, map[string]any{}); e != nil {
			t.Fatal(e)
		}
	}
	w.call("linear_comment_settle", "a", map[string]any{"operation_id": "first", "outcome": "posted", "comment_id": ourComment})
	if e := w.errorOf("linear_comment_settle", "a", map[string]any{"operation_id": "second", "outcome": "posted", "comment_id": ourComment}); !strings.Contains(e, "already recorded for another reservation") {
		t.Fatal("a comment settled two reservations:", e)
	}
	reserved := time.Now().Add(-30 * time.Minute).UTC()
	if _, e := w.s.db.Exec(`UPDATE linear_comment_ledger SET reserved_at=? WHERE state='pending'`, layout(reserved)); e != nil {
		t.Fatal(e)
	}
	digest := digestOf(w.sessionID("a"), "old")
	_, e := w.s.Execute(context.Background(), w.request("linear_comment_settle_match", "c", w.fenced(map[string]any{"operation_digest": digest, "issue_uuid": watchIssue, "comment_id": ourComment, "created_at": reserved.Add(time.Minute).Format(time.RFC3339Nano)})))
	if !codeIs(e, "conflict", "already recorded for another reservation") {
		t.Fatal("a recorded comment was body-matched to another reservation:", e)
	}
}

// History never triggers: a comment from before the watch started is not
// news, even on the first poll.
func TestCommentWatchIgnoresCommentsFromBeforeTheWatch(t *testing.T) {
	w := newWatchHarness(t)
	out := w.poll(watchIssue, comment(humanOne, "", time.Now().Add(-time.Minute)))
	if got := out["notified"].([]string); len(got) != 0 {
		t.Fatal("a comment from before confirm notified:", got)
	}
	if inbox := w.call("inbox", "a", nil).(map[string]any)["messages"].([]map[string]any); len(inbox) != 0 {
		t.Fatal(inbox)
	}
}

// The watch stops only when the last confirmed assignment for the issue is
// released.
func TestCommentWatchOutlivesAnOverlappingRelease(t *testing.T) {
	w := newWatchHarness(t)
	b := w.propose("b", map[string]any{"issue_uuid": watchIssue, "issue_identifier": "FIX-1"})["assignment"].(map[string]any)
	w.call("assignment_confirm", "c", map[string]any{"target_session": "b", "revision": b["revision"], "allow_overlap": true})
	due := func() int {
		w.dueNow()
		return len(w.call("comment_watch_due", "c", w.fenced(map[string]any{})).(map[string]any)["issues"].([]any))
	}
	w.call("assignment_release", "c", map[string]any{"target_session": "a", "revision": w.live("a")["revision"]})
	if n := due(); n != 1 {
		t.Fatal("releasing one of two assignments stopped the watch:", n)
	}
	w.call("assignment_release", "c", map[string]any{"target_session": "b", "revision": w.live("b")["revision"]})
	if n := due(); n != 0 {
		t.Fatal("the watch outlived the last assignment:", n)
	}
}

// With no active assignee and no active coordinator, a poll records nothing
// and holds the cursor, so the comment is delivered once someone can hear it.
func TestCommentWatchHoldsCommentsWithoutARecipient(t *testing.T) {
	w := newWatchHarness(t)
	w.poll(watchIssue)
	var before string
	if e := w.s.db.QueryRow(`SELECT cursor_time FROM watch_cursors`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	if _, e := w.s.db.Exec(`UPDATE sessions SET status='retired' WHERE id=?`, w.sessionID("a")); e != nil {
		t.Fatal(e)
	}
	if _, e := w.s.db.Exec(`UPDATE projects SET coordinator_session=NULL`); e != nil {
		t.Fatal(e)
	}
	posted := time.Now()
	w.dueNow()
	held := w.poll(watchIssue, comment(humanOne, "", posted))
	if held["error"] != "no_recipient" || held["notified"] != nil {
		t.Fatal("no recipient was not reported:", held)
	}
	var after, last string
	var events int
	if e := w.s.db.QueryRow(`SELECT cursor_time,last_error,(SELECT COUNT(*) FROM watch_events) FROM watch_cursors`).Scan(&after, &last, &events); e != nil || after != before || last != "no_recipient" || events != 0 {
		t.Fatal("comments without a recipient were consumed:", after, last, events, e)
	}
	w.dueNow()
	if again := w.poll(watchIssue, comment(humanOne, "", posted)); again["failures"] != 2 {
		t.Fatal("no recipient does not back off:", again)
	}
	if _, e := w.s.db.Exec(`UPDATE sessions SET status='active' WHERE id=?`, w.sessionID("a")); e != nil {
		t.Fatal(e)
	}
	w.dueNow()
	if got := w.poll(watchIssue, comment(humanOne, "", posted))["notified"].([]string); len(got) != 1 {
		t.Fatal("the held comment was not delivered:", got)
	}
}

func TestCommentWatchReportRefusesNegativeCalls(t *testing.T) {
	w := newWatchHarness(t)
	w.call("comment_watch_due", "c", w.fenced(map[string]any{}))
	if e := w.errorOf("comment_watch_report", "c", w.fenced(map[string]any{"issue_uuid": watchIssue, "calls": -50})); !strings.Contains(e, "calls must be 0–1000") {
		t.Fatal("a negative call count refunded budget:", e)
	}
}

func TestOldClientCannotWriteToLinear(t *testing.T) {
	h := newHarness(t)
	for _, args := range []map[string]any{nil, {"ledger": 2}} {
		_, e := h.s.Execute(context.Background(), h.request("taskboard_access", "a", args))
		message, public := PublicMessage(e)
		if !codeIs(e, "invalid", "upgrade ClawBridge (v0.2.5 or later)") || !public || message != upgradeRequired {
			t.Fatalf("access without ledger:1 not refused with the public upgrade message: %v", e)
		}
	}
}

// The hour's budget counts every call: the due charge up front, then each
// report's reconciliation, which the report returns.
func TestCommentWatchReportReturnsTheReconciledBudget(t *testing.T) {
	w := newWatchHarness(t)
	due := w.call("comment_watch_due", "c", w.fenced(map[string]any{})).(map[string]any)
	if due["budget_used"] != 3 {
		t.Fatal("due did not charge one poll and a connection:", due)
	}
	out := w.call("comment_watch_report", "c", w.fenced(map[string]any{"issue_uuid": watchIssue, "calls": 4})).(map[string]any)
	if out["budget_used"] != 6 {
		t.Fatal("report did not charge its extra calls:", out)
	}
}

// F2: the precheck refuses what the reserve would, before any Linear read,
// and writes nothing.
func TestCommentPrecheckRefusesWhatTheReserveWould(t *testing.T) {
	w := newWatchHarness(t)
	w.poll(watchIssue, comment(humanOne, "", time.Now()))
	pre := func(a map[string]any) error {
		a["issue_uuid"] = watchIssue
		_, e := w.s.Execute(context.Background(), w.request("linear_comment_precheck", "a", a))
		return e
	}
	if e := pre(map[string]any{"operation_id": "first", "in_response_to": humanOne}); e != nil {
		t.Fatal("an unanswered notified comment was refused:", e)
	}
	if _, e := w.reserve("a", "first", map[string]any{"parent_id": humanOne, "in_response_to": humanOne}); e != nil {
		t.Fatal(e)
	}
	// A retry of the reserved operation is the reserve's to answer.
	if e := pre(map[string]any{"operation_id": "first", "in_response_to": humanOne}); e != nil {
		t.Fatal("the retry of a reserved answer was refused:", e)
	}
	if e := pre(map[string]any{"operation_id": "second", "in_response_to": humanOne}); !codeIs(e, "conflict", "already has an answer") {
		t.Fatal("a second answer passed the precheck:", e)
	}
	w.call("linear_comment_settle", "a", map[string]any{"operation_id": "first", "outcome": "posted", "comment_id": ourComment})
	if e := pre(map[string]any{"operation_id": "own", "in_response_to": ourComment}); !codeIs(e, "invalid", "") {
		t.Fatal("an answer to our own comment passed:", e)
	}
	for _, op := range []string{"two", "three"} {
		if _, e := w.reserve("a", op, map[string]any{}); e != nil {
			t.Fatal(e)
		}
	}
	if e := pre(map[string]any{"operation_id": "fourth"}); !codeIs(e, "rate_limited", "next slot at") {
		t.Fatal("the cap passed the precheck:", e)
	}
	var rows int
	if e := w.s.db.QueryRow(`SELECT COUNT(*) FROM linear_comment_ledger`).Scan(&rows); e != nil || rows != 3 {
		t.Fatal("the precheck wrote:", rows, e)
	}
}

// A claim or close note ClawBridge posted awaits no answer, so it doesn't
// hold the issue on the 5-minute cadence; a session's own comment, which
// does await one, still does.
func TestCommentWatchCadenceIgnoresMarkerNotes(t *testing.T) {
	w := newWatchHarness(t)
	next := func() time.Duration {
		w.t.Helper()
		w.dueNow()
		out := w.poll(watchIssue)
		return at(out["next_poll_at"].(string)).Sub(time.Now())
	}
	if d := next(); d < 50*time.Minute {
		t.Fatal("a quiet issue polls sooner than hourly:", d)
	}
	now := layout(time.Now())
	if _, e := w.s.db.Exec(`INSERT INTO linear_comment_ledger(workspace_id,project_id,operation_digest,marker_key,issue_uuid,body_sha256,session_id,principal,state,reserved_at) SELECT workspace_id,id,'marker-digest','asg_1:claim',?,'x','ses','p','posted',? FROM projects WHERE id=(SELECT project_id FROM watch_cursors WHERE source='comments' AND subject=? LIMIT 1)`, watchIssue, now, watchIssue); e != nil {
		t.Fatal(e)
	}
	if d := next(); d < 50*time.Minute {
		t.Fatal("a claim note held the issue on the short cadence:", d)
	}
	if _, e := w.s.db.Exec(`INSERT INTO linear_comment_ledger(workspace_id,project_id,operation_digest,issue_uuid,body_sha256,session_id,principal,state,reserved_at) SELECT workspace_id,id,'own-digest',?,'x','ses','p','posted',? FROM projects WHERE id=(SELECT project_id FROM watch_cursors WHERE source='comments' AND subject=? LIMIT 1)`, watchIssue, now, watchIssue); e != nil {
		t.Fatal(e)
	}
	if d := next(); d > 6*time.Minute {
		t.Fatal("a session's own comment didn't keep the issue on the short cadence:", d)
	}
}

// FR8 r3: with no cap (the default) a tick still polls at most
// watchTickIssues issues, so after a downtime the overdue ones are spread
// over ticks, never all at once; nothing says the budget is spent.
func TestCommentWatchTickIsBoundedWithoutACap(t *testing.T) {
	w := newWatchHarness(t)
	var pid string
	if e := w.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	past := layout(time.Now().Add(-time.Hour))
	for i := 0; i < 150; i++ {
		if _, e := w.s.db.Exec(`INSERT INTO watch_cursors(project_id,source,subject,cursor_time,watch_started_at,next_poll_at,updated_at) VALUES(?,'comments',?,?,?,?,?)`, pid, fmt.Sprintf("00000000-0000-4000-8000-%012d", i), past, past, past, past); e != nil {
			t.Fatal(e)
		}
	}
	due := w.call("comment_watch_due", "c", w.fenced(map[string]any{})).(map[string]any)
	if n := len(due["issues"].([]any)); n != watchTickIssues || due["budget_limit"] != 0 || due["budget_exhausted"] != false {
		t.Fatal(n, due["budget_limit"], due["budget_exhausted"])
	}
}
