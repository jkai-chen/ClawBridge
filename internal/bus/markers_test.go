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
	if g["action"] != "post" || !strings.HasPrefix(body, "🤖 Claimed by ClawBridge agent session a · ref cb-") || !strings.HasSuffix(body, "\n\n— 🤖 ClawBridge agent · session a") || strings.Contains(body, id) {
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
	w.markerSettle("c", id, "claim", "comment", map[string]any{"outcome": "done", "comment_id": markerComment})
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
