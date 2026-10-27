package bus

import (
	"strings"
	"testing"
)

// closeOutcome is the close row's outcome and state part, "" for none.
func (w *watchHarness) closeOutcome(id string) (outcome, state string) {
	w.t.Helper()
	var o any
	if e := w.s.db.QueryRow(`SELECT outcome,state_state FROM assignment_markers WHERE assignment_id=? AND phase='close'`, id).Scan(&o, &state); e != nil {
		w.t.Fatal(e)
	}
	if o != nil {
		outcome = str(map[string]any{"o": o}, "o")
	}
	return
}

// Retiring a session that holds a confirmed assignment, with neither
// outcome nor confirm, is refused, naming the issue; nothing changes.
func TestRetireAsksWhatBecomesOfAConfirmedAssignment(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	e := w.errorOf("retire_session", "b", map[string]any{})
	if !strings.Contains(e, "session b holds the confirmed assignment for FIX-1") || !strings.Contains(e, "outcome done") || !strings.Contains(e, "confirm") {
		t.Fatal(e)
	}
	if live := w.live("b"); live == nil || live["state"] != "confirmed" {
		t.Fatal("the refusal released the assignment:", live)
	}
	if e := w.errorOf("retire_session", "b", map[string]any{"outcome": "finished"}); !strings.Contains(e, "outcome must be done") {
		t.Fatal(e)
	}
}

// outcome done releases with done, as assignment_release does: the close's
// state part is due, under the project's close rule.
func TestRetireWithOutcomeDoneFinishesTheAssignment(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	got := w.call("retire_session", "b", map[string]any{"outcome": "done"}).(map[string]any)
	if got["status"] != "retired" || got["assignment"].(map[string]any)["outcome"] != "done" {
		t.Fatal(got)
	}
	if outcome, state := w.closeOutcome(id); outcome != "done" || state != "pending" {
		t.Fatal("the close is not done:", outcome, state)
	}
}

// confirm releases it as it is: no outcome, and the issue stays put.
func TestRetireWithConfirmReleasesAsItIs(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	got := w.call("retire_session", "b", map[string]any{"confirm": true}).(map[string]any)
	if got["status"] != "retired" || got["assignment"].(map[string]any)["outcome"] != nil {
		t.Fatal(got)
	}
	if outcome, state := w.closeOutcome(id); outcome != "" || state == "pending" {
		t.Fatal("a release as it is moved the issue:", outcome, state)
	}
}

// No question without a confirmed assignment, nor for a move to a
// successor, who takes the work over; done with a successor hands nothing
// on.
func TestRetireAsksOnlyWhenWorkWouldBeLeft(t *testing.T) {
	w := newWatchHarness(t)
	w.propose("b", map[string]any{"issue_uuid": issueOne, "issue_identifier": "FIX-2"})
	if got := w.call("retire_session", "b", map[string]any{}).(map[string]any); got["status"] != "retired" {
		t.Fatal("a proposal alone asked:", got)
	}
	got := w.call("retire_session", "a", map[string]any{"status": "moved", "successor": "c"}).(map[string]any)
	if got["status"] != "moved" || got["assignment"].(map[string]any)["transferred_to"] == nil {
		t.Fatal("a move to a successor asked, or handed nothing on:", got)
	}
	v := newWatchHarness(t)
	v.setStatusPolicy(0, sfPolicy(true))
	v.assign("b", sfIssueNext)
	got = v.call("retire_session", "b", map[string]any{"status": "moved", "successor": "c", "outcome": "done"}).(map[string]any)
	ended := got["assignment"].(map[string]any)
	if ended["outcome"] != "done" || ended["transferred_to"] != nil || ended["transfer_skipped"] != "the work is done" {
		t.Fatal(ended)
	}
}
