package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	markerComment = "00000000-0000-4000-8000-0000000000e1"
	closeComment  = "00000000-0000-4000-8000-0000000000e2"
)

func (w *watchHarness) assignmentID(session string) string {
	w.t.Helper()
	var id string
	if e := w.s.db.QueryRow(`SELECT id FROM session_assignments WHERE session_id=? AND state='confirmed'`, w.sessionID(session)).Scan(&id); e != nil {
		w.t.Fatal(e)
	}
	return id
}

func (w *watchHarness) markerGo(session, id, phase, part string) map[string]any {
	w.t.Helper()
	return w.call("marker_go", session, map[string]any{"assignment_id": id, "phase": phase, "part": part}).(map[string]any)
}

func (w *watchHarness) markerSettle(session, id, phase, part string, a map[string]any) map[string]any {
	w.t.Helper()
	a["assignment_id"], a["phase"], a["part"] = id, phase, part
	return w.call("marker_settle", session, a).(map[string]any)
}

func (w *watchHarness) markerState(id, phase string) (comment, label string) {
	w.t.Helper()
	if e := w.s.db.QueryRow(`SELECT comment_state,label_state FROM assignment_markers WHERE assignment_id=? AND phase=?`, id, phase).Scan(&comment, &label); e != nil {
		w.t.Fatal(phase, e)
	}
	return
}

// markersDue makes every marker row due now.
func (w *watchHarness) markersDue() {
	w.t.Helper()
	if _, e := w.s.db.Exec(`UPDATE assignment_markers SET next_attempt_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		w.t.Fatal(e)
	}
}

// age moves marker attempts and reservations past the stale window.
func (w *watchHarness) age() {
	w.t.Helper()
	old := layout(time.Now().Add(-11 * time.Minute))
	if _, e := w.s.db.Exec(`UPDATE linear_comment_ledger SET reserved_at=? WHERE state='pending'`, old); e != nil {
		w.t.Fatal(e)
	}
	if _, e := w.s.db.Exec(`UPDATE assignment_markers SET label_in_flight_at=? WHERE label_state='in_flight'`, old); e != nil {
		w.t.Fatal(e)
	}
	w.markersDue()
}

func (w *watchHarness) release(session string) {
	w.t.Helper()
	w.call("assignment_release", "c", map[string]any{"target_session": session, "revision": w.live(session)["revision"]})
}

func TestConfirmWritesAClaimAndReleaseAClose(t *testing.T) {
	w := newWatchHarness(t)
	id := w.assignmentID("a")
	if c, l := w.markerState(id, "claim"); c != "pending" || l != "pending" {
		t.Fatal("no pending claim at confirm:", c, l)
	}
	// An assignment without an issue has nothing to mark.
	w.propose("b", map[string]any{"scope_paths": []any{"docs"}})
	w.call("assignment_confirm", "c", map[string]any{"target_session": "b", "revision": w.live("b")["revision"]})
	var n int
	if e := w.s.db.QueryRow(`SELECT COUNT(*) FROM assignment_markers`).Scan(&n); e != nil || n != 1 {
		t.Fatal("markers:", n, e)
	}
	w.release("a")
	if c, l := w.markerState(id, "close"); c != "waiting_on_claim" || l != "pending" {
		t.Fatal("release wrote no waiting close:", c, l)
	}
}

// The claim note is the server's text, reserved in the ledger outside the
// conversational cap, posted once, and read back as ClawBridge's.
func TestClaimNoteIsTheServerBodyOutsideTheConversationalCap(t *testing.T) {
	w := newWatchHarness(t)
	id := w.assignmentID("a")
	g := w.markerGo("a", id, "claim", "comment")
	body, _ := g["body"].(string)
	if g["action"] != "post" || !strings.HasPrefix(body, "🤖 Claimed by ClawBridge agent session a · ref cb-") || strings.Count(body, "🤖") != 1 || strings.Contains(body, "\n") || strings.Contains(body, id) {
		t.Fatal("claim note:", g)
	}
	if again := w.markerGo("c", id, "claim", "comment"); again["action"] != "wait" {
		t.Fatal("a claim note handed out twice while the first may be posting:", again)
	}
	for _, op := range []string{"one", "two", "three"} {
		if _, e := w.reserve("a", op, map[string]any{}); e != nil {
			t.Fatal("the claim note took a conversational slot:", e)
		}
	}
	w.markerSettle("a", id, "claim", "comment", map[string]any{"outcome": "done", "comment_id": markerComment})
	if c, _ := w.markerState(id, "claim"); c != "done" {
		t.Fatal(c)
	}
	w.markersDue()
	if g = w.markerGo("a", id, "claim", "comment"); g["action"] != "none" {
		t.Fatal("a done claim note handed out again:", g)
	}
	flags := w.call("linear_comments_posted", "c", map[string]any{"issue_uuid": watchIssue, "comment_ids": []any{markerComment}}).(map[string]any)
	if f, _ := flags["posted"].(map[string]any)[markerComment].(map[string]any); f["kind"] != "marker" {
		t.Fatal("claim note not read as ClawBridge's:", flags)
	}
	// Nobody reserves a marker key by hand.
	if _, e := w.reserve("a", "marker:"+id+":claim:comment", map[string]any{}); !codeIs(e, "invalid", "reserved for assignment claim markers") {
		t.Fatal(e)
	}
}

func TestMarkerSettleNeedsAHandedOutPart(t *testing.T) {
	w := newWatchHarness(t)
	id := w.assignmentID("a")
	if e := w.errorOf("marker_settle", "a", map[string]any{"assignment_id": id, "phase": "claim", "part": "label", "outcome": "done"}); !strings.Contains(e, "not handed out") {
		t.Fatal("settled a part never handed out:", e)
	}
	w.markerGo("a", id, "claim", "comment")
	w.markerSettle("a", id, "claim", "comment", map[string]any{"outcome": "done", "comment_id": markerComment})
	w.release("a")
	w.markersDue()
	g := w.markerGo("c", id, "close", "comment")
	if g["action"] != "post" {
		t.Fatal(g)
	}
	// A comment already recorded cannot settle another marker.
	if e := w.errorOf("marker_settle", "c", map[string]any{"assignment_id": id, "phase": "close", "part": "comment", "outcome": "done", "comment_id": markerComment}); !strings.Contains(e, "already recorded") {
		t.Fatal(e)
	}
}

func TestMarkerCommentsHaveTheirOwnCapPerIssue(t *testing.T) {
	w := newWatchHarness(t)
	now := layout(time.Now())
	for i := 0; i < markerCapPerHour; i++ {
		if _, e := w.s.db.Exec(`INSERT INTO linear_comment_ledger(workspace_id,project_id,operation_digest,marker_key,issue_uuid,body_sha256,session_id,principal,state,reserved_at) SELECT p.workspace_id,p.id,?,?,?,?,'s','p','posted',? FROM projects p JOIN sessions s ON s.project_id=p.id WHERE s.id=?`, digestOf("x", string(rune('a'+i))), "marker:x:claim:comment", watchIssue, bodyDigest, now, w.sessionID("a")); e != nil {
			t.Fatal(e)
		}
	}
	id := w.assignmentID("a")
	if g := w.markerGo("a", id, "claim", "comment"); g["action"] != "none" || g["state"] != "skipped" {
		t.Fatal("the marker cap let a seventh note through:", g)
	}
	// The label is still applied.
	if g := w.markerGo("a", id, "claim", "label"); g["action"] != "add" || g["label"] != MarkerLabel {
		t.Fatal(g)
	}
}

// A claim that never reached Linear leaves no close note and no removal.
func TestCloseOfAClaimThatNeverLandedIsSkipped(t *testing.T) {
	w := newWatchHarness(t)
	id := w.assignmentID("a")
	w.release("a")
	for _, part := range []string{"comment", "label"} {
		if g := w.markerGo("c", id, "close", part); g["action"] != "none" || g["state"] != "skipped" {
			t.Fatal(part, "close not skipped:", g)
		}
	}
	if c, l := w.markerState(id, "claim"); c != "superseded" || l != "superseded" {
		t.Fatal("claim not superseded:", c, l)
	}
	// The superseded claim can never land later.
	for _, part := range []string{"comment", "label"} {
		if g := w.markerGo("a", id, "claim", part); g["action"] != "none" {
			t.Fatal("claim applied after its close:", g)
		}
	}
}

// MK-H1 case 1: a release racing a claim note that may have been accepted
// still closes, once the claim is no longer young.
func TestReleaseRacingAPendingClaimStillCloses(t *testing.T) {
	w := newWatchHarness(t)
	id := w.assignmentID("a")
	w.markerGo("a", id, "claim", "comment")
	w.release("a")
	if g := w.markerGo("c", id, "close", "comment"); g["action"] != "wait" {
		t.Fatal("close posted while the claim may still be posting:", g)
	}
	w.age()
	if g := w.markerGo("c", id, "close", "comment"); g["action"] != "post" || !strings.Contains(g["body"].(string), "Released by ClawBridge agent session a (released by the coordinator)") {
		t.Fatal("close not posted after a may-be-done claim:", g)
	}
}

// LOW: a claim whose attempt failed is superseded by its close.
func TestFailedClaimIsNotRetriedAfterItsClose(t *testing.T) {
	w := newWatchHarness(t)
	id := w.assignmentID("a")
	w.markerGo("a", id, "claim", "comment")
	w.markerSettle("a", id, "claim", "comment", map[string]any{"outcome": "failed", "error": "unavailable"})
	w.release("a")
	w.markersDue()
	if g := w.markerGo("a", id, "claim", "comment"); g["action"] != "none" {
		t.Fatal("failed claim retried after its close:", g)
	}
	if g := w.markerGo("c", id, "close", "comment"); g["action"] != "none" || g["state"] != "skipped" {
		t.Fatal("close note for a claim that never landed:", g)
	}
}

// R2-H2: close A removes the label while B's claim waits, then B's add
// lands: B ends confirmed with the label.
func TestClaimLabelWaitsForAnInFlightRemovalOnTheIssue(t *testing.T) {
	w := newWatchHarness(t)
	a := w.assignmentID("a")
	if g := w.markerGo("a", a, "claim", "label"); g["action"] != "add" {
		t.Fatal(g)
	}
	w.markerSettle("a", a, "claim", "label", map[string]any{"outcome": "done"})
	w.release("a")
	if g := w.markerGo("c", a, "close", "label"); g["action"] != "remove" {
		t.Fatal("close did not remove the label:", g)
	}
	w.assign("b", watchIssue)
	b := w.assignmentID("b")
	if g := w.markerGo("c", b, "claim", "label"); g["action"] != "wait" {
		t.Fatal("claim add not fenced by the in-flight removal:", g)
	}
	w.markerSettle("c", a, "close", "label", map[string]any{"outcome": "done"})
	w.markersDue()
	if g := w.markerGo("c", b, "claim", "label"); g["action"] != "add" {
		t.Fatal("claim add not sent after the removal settled:", g)
	}
}

// M5: the label is removed only when the last confirmed assignment closes.
func TestOverlappingAssignmentKeepsTheLabel(t *testing.T) {
	w := newWatchHarness(t)
	a := w.assignmentID("a")
	w.markerGo("a", a, "claim", "label")
	w.markerSettle("a", a, "claim", "label", map[string]any{"outcome": "done"})
	b := w.propose("b", map[string]any{"issue_uuid": watchIssue, "issue_identifier": "FIX-1"})["assignment"].(map[string]any)
	w.call("assignment_confirm", "c", map[string]any{"target_session": "b", "revision": b["revision"], "allow_overlap": true})
	w.release("a")
	if g := w.markerGo("c", a, "close", "label"); g["action"] != "none" || g["state"] != "skipped" {
		t.Fatal("label removed while another assignment holds the issue:", g)
	}
}

func TestCloseNoteSaysWhyInFixedWords(t *testing.T) {
	for reason, words := range map[string]string{"self": "done", "coordinator": "released by the coordinator", "replaced": "moved to other work", "access_revoked": "access ended", "retired": "session ended"} {
		body := markerBody(map[string]any{"assignment_id": "asg_1", "phase": "close", "session_name": "worker", "reason": reason})
		if !strings.Contains(body, "("+words+")") {
			t.Fatal(reason, body)
		}
	}
	long := strings.Repeat("n", 40)
	if body := markerBody(map[string]any{"assignment_id": "asg_1", "phase": "claim", "session_name": long}); strings.Contains(body, long) || !strings.Contains(body, strings.Repeat("n", 31)+"…") {
		t.Fatal("a long session name reached Linear uncut:", body)
	}
}

func TestMarkerSettleBacksOffAndParksOutOfBinding(t *testing.T) {
	w := newWatchHarness(t)
	id := w.assignmentID("a")
	for i := 1; i <= markerMaxOutside; i++ {
		w.markersDue()
		if g := w.markerGo("a", id, "claim", "label"); g["action"] != "add" {
			t.Fatal(i, g)
		}
		out := w.markerSettle("a", id, "claim", "label", map[string]any{"outcome": "out_of_binding"})
		if i < markerMaxOutside && (out["state"] != "pending" || out["next_attempt_at"].(string) <= layout(time.Now())) {
			t.Fatal("no backoff:", out)
		}
		if i == markerMaxOutside && out["state"] != "failed_out_of_binding" {
			t.Fatal("not parked:", out)
		}
	}
	// The backoff is the row's, but the comment part keeps its own state.
	w.markersDue()
	if g := w.markerGo("a", id, "claim", "comment"); g["action"] != "post" {
		t.Fatal("the comment part shares the label's state:", g)
	}
}

// The watcher gets due markers under its lease, charged before any read.
func TestMarkerDueIsFencedAndCharged(t *testing.T) {
	w := newWatchHarness(t)
	if _, e := w.s.Execute(context.Background(), w.request("marker_due", "c", map[string]any{"lease_token": strings.Repeat("0", 32), "generation": w.gen})); !codeIs(e, "conflict", "lease was lost") {
		t.Fatal("marker_due without the lease:", e)
	}
	due := w.call("marker_due", "c", w.fenced(map[string]any{})).(map[string]any)
	markers := due["markers"].([]any)
	if len(markers) != 1 || markers[0].(map[string]any)["assignment_id"] != w.assignmentID("a") {
		t.Fatal(due)
	}
	var used int
	if e := w.s.db.QueryRow(`SELECT budget_used FROM watch_lease`).Scan(&used); e != nil || used != markerDueCalls {
		t.Fatal("markers not charged:", used, e)
	}
	// A fenced go works for the watcher too.
	g := w.call("marker_go", "c", w.fenced(map[string]any{"assignment_id": w.assignmentID("a"), "phase": "claim", "part": "label"})).(map[string]any)
	if g["action"] != "add" {
		t.Fatal(g)
	}
}

func TestWatchStatusReportsWatcherAndMarkerDrift(t *testing.T) {
	w := newWatchHarness(t)
	status := func() map[string]any {
		return w.call("comment_watch_status", "", map[string]any{}).(map[string]any)
	}
	s := status()
	if s["watched_issues"] != 1 || s["watcher_live"] != true || s["markers_pending"] != 0 {
		t.Fatal(s)
	}
	id := w.assignmentID("a")
	w.markerGo("a", id, "claim", "label")
	w.markerSettle("a", id, "claim", "label", map[string]any{"outcome": "done"})
	w.release("a")
	old := layout(time.Now().Add(-time.Hour))
	if _, e := w.s.db.Exec(`UPDATE assignment_markers SET created_at=?`, old); e != nil {
		t.Fatal(e)
	}
	if _, e := w.s.db.Exec(`UPDATE watch_lease SET expires_at=?`, old); e != nil {
		t.Fatal(e)
	}
	s = status()
	if s["close_label_drift"] != 1 || s["markers_pending"] != 2 || s["watcher_live"] != false || s["markers_oldest_pending"] != old {
		t.Fatal("drift not reported:", s)
	}
	// The server's watcher: no ticks, a good tick under an hour ago, and
	// one only older (its lease has lapsed between ticks either way).
	if s["server_watch_ticks"] != false || s["server_watch_live"] != false || s["server_watch_last_ok"] != nil {
		t.Fatal("server watcher without ticks:", s)
	}
	recent := layout(time.Now().Add(-40 * time.Minute))
	if _, e := w.s.db.Exec(`INSERT INTO watch_ticks(project_id,last_tick_at,last_ok_at) SELECT id,?,? FROM projects`, recent, recent); e != nil {
		t.Fatal(e)
	}
	if s = status(); s["server_watch_ticks"] != true || s["server_watch_live"] != true || s["server_watch_last_ok"] != recent {
		t.Fatal("a recent good tick:", s)
	}
	stale := layout(time.Now().Add(-61 * time.Minute))
	if _, e := w.s.db.Exec(`UPDATE watch_ticks SET last_tick_at=?,last_ok_at=?`, layout(time.Now()), stale); e != nil {
		t.Fatal(e)
	}
	if s = status(); s["server_watch_live"] != false || s["server_watch_last_ok"] != stale {
		t.Fatal("ticks failing for an hour:", s)
	}
	// Another confirmed assignment on the issue keeps the label rightly.
	w.assign("b", watchIssue)
	if s = status(); s["close_label_drift"] != 0 {
		t.Fatal("an issue another assignment holds counted as drift:", s)
	}
}

// PR3-H1: a label write whose outcome is unknown may have landed, so the
// close still removes the label once the attempt is stale.
func TestUncertainLabelAddStillGetsRemovedAtClose(t *testing.T) {
	w := newWatchHarness(t)
	id := w.assignmentID("a")
	w.markerGo("a", id, "claim", "label")
	if out := w.markerSettle("a", id, "claim", "label", map[string]any{"outcome": "uncertain"}); out["state"] != "in_flight" {
		t.Fatal("an uncertain add lost its may-have-landed state:", out)
	}
	w.release("a")
	w.markersDue()
	if g := w.markerGo("c", id, "close", "label"); g["action"] != "wait" {
		t.Fatal("close went ahead while the uncertain add is young:", g)
	}
	w.age()
	if g := w.markerGo("c", id, "close", "label"); g["action"] != "remove" {
		t.Fatal("an uncertain add was never removed:", g)
	}
}

// PR3-H1: an uncertain removal keeps fencing a later claim's add.
func TestUncertainRemovalKeepsFencingTheNextClaim(t *testing.T) {
	w := newWatchHarness(t)
	a := w.assignmentID("a")
	w.markerGo("a", a, "claim", "label")
	w.markerSettle("a", a, "claim", "label", map[string]any{"outcome": "done"})
	w.release("a")
	w.markerGo("c", a, "close", "label")
	w.markerSettle("c", a, "close", "label", map[string]any{"outcome": "uncertain"})
	w.assign("b", watchIssue)
	b := w.assignmentID("b")
	if g := w.markerGo("c", b, "claim", "label"); g["action"] != "wait" {
		t.Fatal("claim add not fenced by an uncertain removal:", g)
	}
	w.age()
	if g := w.markerGo("c", b, "claim", "label"); g["action"] != "add" {
		t.Fatal("claim add stuck after the removal went stale:", g)
	}
}

// PR3-M1: a receipt settles only the attempt this session was handed, and
// a found comment must sit in the attempt's window.
func TestMarkerCommentSettleNeedsItsOwnAttemptOrTheWindow(t *testing.T) {
	w := newWatchHarness(t)
	id := w.assignmentID("a")
	w.markerGo("a", id, "claim", "comment")
	if e := w.errorOf("marker_settle", "c", map[string]any{"assignment_id": id, "phase": "claim", "part": "comment", "outcome": "done", "comment_id": humanOne}); !strings.Contains(e, "handed to another session") {
		t.Fatal("another session settled the attempt with an arbitrary comment:", e)
	}
	if e := w.errorOf("marker_settle", "c", map[string]any{"assignment_id": id, "phase": "claim", "part": "comment", "outcome": "absent"}); !strings.Contains(e, "absent applies to a checked") {
		t.Fatal("a young attempt was taken over:", e)
	}
	w.age()
	g := w.markerGo("c", id, "claim", "comment")
	if g["action"] != "check" || g["check_from"] == nil {
		t.Fatal("an attempt that may have landed was not checked first:", g)
	}
	var reserved string
	if e := w.s.db.QueryRow(`SELECT reserved_at FROM linear_comment_ledger WHERE marker_key<>''`).Scan(&reserved); e != nil {
		t.Fatal(e)
	}
	outside := at(reserved).Add(20 * time.Minute).Format(time.RFC3339Nano)
	if e := w.errorOf("marker_settle", "c", map[string]any{"assignment_id": id, "phase": "claim", "part": "comment", "outcome": "done", "via": "body_match", "comment_id": humanOne, "created_at": outside}); !strings.Contains(e, "outside the marker attempt's window") {
		t.Fatal("a comment outside the window settled the marker:", e)
	}
	// Nothing found: the checker gets a fresh attempt of its own.
	post := w.markerSettle("c", id, "claim", "comment", map[string]any{"outcome": "absent"})
	if post["action"] != "post" || post["body"] == nil {
		t.Fatal(post)
	}
	w.markerSettle("c", id, "claim", "comment", map[string]any{"outcome": "done", "comment_id": markerComment})
	if c, _ := w.markerState(id, "claim"); c != "done" {
		t.Fatal(c)
	}
	// Inside the window, a found comment settles it.
	w2 := newWatchHarness(t)
	id2 := w2.assignmentID("a")
	w2.markerGo("a", id2, "claim", "comment")
	w2.age()
	w2.markerGo("c", id2, "claim", "comment")
	if e := w2.s.db.QueryRow(`SELECT reserved_at FROM linear_comment_ledger WHERE marker_key<>''`).Scan(&reserved); e != nil {
		t.Fatal(e)
	}
	inside := at(reserved).Add(time.Minute).Format(time.RFC3339Nano)
	if out := w2.markerSettle("c", id2, "claim", "comment", map[string]any{"outcome": "done", "via": "body_match", "comment_id": markerComment, "created_at": inside}); out["state"] != "done" {
		t.Fatal(out)
	}
}

func (w *watchHarness) markerStatus(session string, a map[string]any) []any {
	w.t.Helper()
	return w.call("marker_status", session, a).(map[string]any)["markers"].([]any)
}

// marker_status reads an issue's or an assignment's marker rows, newest
// first, from a session or the person's own terminal, and takes exactly
// one of the two.
func TestMarkerStatusReadsByIssueOrAssignment(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.skipMarker("b", "from_state")
	w.call("assignment_release", "b", map[string]any{"revision": w.live("b")["revision"], "outcome": "done"})
	for _, session := range []string{"a", ""} {
		for _, a := range []map[string]any{{"issue_uuid": strings.ToUpper(sfIssueNext)}, {"assignment_id": id}} {
			rows := w.markerStatus(session, a)
			if len(rows) != 2 {
				t.Fatal(session, a, rows)
			}
			close, claim := rows[0].(map[string]any), rows[1].(map[string]any)
			if close["phase"] != "close" || close["state_state"] != "pending" || close["summary"] != "comment waiting_on_claim, label pending, state pending" {
				t.Fatal("close", close)
			}
			if claim["phase"] != "claim" || claim["assignment_id"] != id || claim["issue_uuid"] != sfIssueNext || claim["issue_identifier"] != "FIX-1" ||
				claim["comment_state"] != "pending" || claim["label_state"] != "pending" || claim["state_state"] != "skipped" || claim["state_skip_reason"] != "from_state" ||
				claim["state_target"] != "" || claim["state_landed"] != "" || claim["created_at"] == "" || claim["updated_at"] == "" ||
				claim["summary"] != "not moved to In Progress: someone else had moved the issue, so it was left as it is; set it yourself if right" {
				t.Fatal("claim", claim)
			}
		}
	}
	// By the identifier the assignments recorded, with no Linear read: every
	// assignment that recorded it, here the harness's first too.
	for _, session := range []string{"a", ""} {
		rows := w.markerStatus(session, map[string]any{"issue_identifier": "FIX-1"})
		issues := map[string]int{}
		for _, r := range rows {
			row := r.(map[string]any)
			if row["issue_identifier"] != "FIX-1" {
				t.Fatal(row)
			}
			issues[row["issue_uuid"].(string)]++
		}
		if len(rows) != 3 || issues[sfIssueNext] != 2 || rows[1].(map[string]any)["state_skip_reason"] != "from_state" {
			t.Fatal(session, rows)
		}
	}
	if rows := w.markerStatus("a", map[string]any{"issue_identifier": "FIX-404"}); len(rows) != 0 {
		t.Fatal(rows)
	}
	if rows := w.markerStatus("a", map[string]any{"assignment_id": "asg_none"}); len(rows) != 0 {
		t.Fatal(rows)
	}
	for name, a := range map[string]map[string]any{
		"both":    {"issue_uuid": sfIssueNext, "assignment_id": id},
		"neither": {},
		"bad":     {"issue_uuid": "FIX-1"},
		"bad key": {"issue_identifier": "FIX 1"},
		"key+id":  {"issue_identifier": "FIX-1", "issue_uuid": sfIssueNext},
		"key+asg": {"issue_identifier": "FIX-1", "assignment_id": id},
		"extra":   {"issue_uuid": sfIssueNext, "phase": "claim"},
	} {
		if _, err := w.s.Execute(context.Background(), w.request("marker_status", "a", a)); err == nil {
			t.Fatal(name, "was accepted")
		}
	}
	if _, err := w.s.Execute(context.Background(), w.request("marker_status", "", map[string]any{"issue_uuid": sfIssueNext, "instance": "a-instance"})); err == nil || !strings.Contains(err.Error(), "instance requires session") {
		t.Fatal(err)
	}
}

// The identifier matches whatever case either side uses.
func TestMarkerStatusMatchesTheIdentifierInAnyCase(t *testing.T) {
	w := newWatchHarness(t)
	w.assign("b", sfIssueNext)
	if rows := w.markerStatus("", map[string]any{"issue_identifier": "fix-1"}); len(rows) == 0 || rows[0].(map[string]any)["issue_identifier"] != "FIX-1" {
		t.Fatal(rows)
	}
	w.release("b")
	w.propose("b", map[string]any{"issue_uuid": sfIssueOther, "issue_identifier": "fix-2"})
	w.call("assignment_confirm", "c", map[string]any{"target_session": "b", "revision": w.live("b")["revision"]})
	rows := w.markerStatus("", map[string]any{"issue_identifier": "FIX-2"})
	if len(rows) != 1 || rows[0].(map[string]any)["issue_uuid"] != sfIssueOther || rows[0].(map[string]any)["issue_identifier"] != "fix-2" {
		t.Fatal(rows)
	}
}

// At most markerStatusRows rows, the newest first.
func TestMarkerStatusKeepsTheNewestRows(t *testing.T) {
	w := newWatchHarness(t)
	for i := 0; i < markerStatusRows/2+1; i++ {
		w.release("a")
		w.assign("a", watchIssue)
	}
	rows := w.markerStatus("", map[string]any{"issue_uuid": watchIssue})
	if len(rows) != markerStatusRows {
		t.Fatal(len(rows))
	}
	newest := rows[0].(map[string]any)
	if newest["assignment_id"] != w.assignmentID("a") || newest["phase"] != "claim" {
		t.Fatal(newest)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].(map[string]any)["updated_at"].(string) < rows[i].(map[string]any)["updated_at"].(string) {
			t.Fatal("not newest first at", i)
		}
	}
}

// Doctor and the overview count the week's state skips a notice tells of,
// and doctor names the newest; configured skips and older ones don't count.
func TestMarkerStateSkipCounts(t *testing.T) {
	w := newWatchHarness(t)
	w.link(0, []string{cpOne})
	w.setStatusPolicy(0, sfPolicy(true))
	count := func() (int, any, any) {
		t.Helper()
		st := w.call("comment_watch_status", "", map[string]any{}).(map[string]any)
		return st["markers_state_skipped"].(int), st["markers_state_skipped_newest"], w.overview("")["linear"].(map[string]any)["markers_state_skipped"]
	}
	if n, newest, overview := count(); n != 0 || newest != nil || overview != 0 {
		t.Fatal(n, newest, overview)
	}
	w.assign("b", sfIssueNext)
	w.skipMarker("b", "team_off")
	if n, _, _ := count(); n != 0 {
		t.Fatal("a configured skip counted:", n)
	}
	w.release("b")
	w.assign("b", sfIssueNext)
	w.skipMarker("b", "from_state")
	n, newest, overview := count()
	if n != 1 || overview != 1 {
		t.Fatal(n, overview)
	}
	if got := newest.(map[string]any); got["issue_uuid"] != sfIssueNext || got["reason"] != "from_state" {
		t.Fatal(got)
	}
	if _, err := w.s.db.Exec(`UPDATE assignment_markers SET updated_at=? WHERE state_skip_reason='from_state'`, layout(time.Now().Add(-8*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if n, newest, overview := count(); n != 0 || newest != nil || overview != 0 {
		t.Fatal("an old skip counted:", n, newest, overview)
	}
}
