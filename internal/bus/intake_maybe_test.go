package bus

import (
	"context"
	"testing"
)

// signalClean is signalBy for an event the intake pass proved, from Linear's
// history, to carry no title or description change since the transition
// (FR58).
func (h *textHarness) signalClean(signal, at, updated, title, description string) string {
	h.t.Helper()
	ev := IntakeEvent{Signal: signal, Actor: trustedUser, Trusted: true, At: at, TextClean: true, Issue: IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7", TitleSHA256: title, DescriptionSHA256: description, UpdatedAt: updated}}
	got, err := h.s.IntakeEnqueueSystem(context.Background(), h.pid, ev)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

// A trusted signal for an issue updated beyond the skew, with the history
// showing only other fields changed, is queued clean: no flag, no change
// notice.
func TestIntakeQueueingBeyondTheSkewIsCleanWhenHistoryProvedIt(t *testing.T) {
	h := newTextHarness(t)
	if got := h.signalClean("S1", "2026-10-23T12:00:00Z", "2026-10-23T12:05:00Z", "t1", "d1"); got != "queued" {
		t.Fatal(got)
	}
	if h.reason() != "" || h.status()["changed"] != false || len(h.maybeNotices()) != 0 || h.followUps() != 0 || h.notices() != 1 {
		t.Fatal(h.reason(), h.maybeNotices(), h.notices())
	}
	// The same event without the proof is the FR57 maybe flag.
	h2 := newTextHarness(t)
	h2.signal("2026-10-23T12:00:00Z", "2026-10-23T12:05:00Z", "t1", "d1")
	if h2.reason() != "text_maybe" || len(h2.maybeNotices()) != 1 {
		t.Fatal(h2.reason())
	}
	// An untrusted event is never vouched by a proof.
	h3 := newTextHarness(t)
	ev := IntakeEvent{Signal: "S1", At: "2026-10-23T12:00:00Z", TextClean: true, Issue: IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7", TitleSHA256: "t1", DescriptionSHA256: "d1", UpdatedAt: "2026-10-23T12:30:00Z"}}
	if _, err := h3.s.IntakeEnqueueSystem(context.Background(), h3.pid, ev); err != nil || h3.reason() != "" {
		t.Fatal(err, h3.reason())
	}
	if eventVouchesText(ev) {
		t.Fatal("an untrusted event vouched")
	}
}

// The re-queue of a closed row, a re-vouch and a cap raise take the proof
// like the skew.
func TestIntakeReVouchBeyondTheSkewNeedsTheProof(t *testing.T) {
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	h.check("t1", "d2")
	if h.reason() != "text_changed" {
		t.Fatal(h.reason())
	}
	// Beyond the skew, no proof: not re-vouched.
	if got := h.signal("2026-10-23T12:00:00Z", "2026-10-23T12:00:30Z", "t1", "d3"); got != "already_queued" {
		t.Fatal(got)
	}
	if h.reason() != "text_changed" || h.stored() != "t1|d1" {
		t.Fatal("vouched without proof:", h.reason(), h.stored())
	}
	// Beyond the skew, proved: re-vouched, flag cleared.
	if got := h.signalClean("S1", "2026-10-23T13:00:00Z", "2026-10-23T13:00:30Z", "t1", "d3"); got != "already_queued" {
		t.Fatal(got)
	}
	if h.reason() != "" || h.stored() != "t1|d3" || h.status()["changed"] != false {
		t.Fatal("not re-vouched:", h.reason(), h.stored())
	}
	// A mention is not a vouch even when proved.
	h.check("t1", "d4")
	h.signalClean("S3", "2026-10-23T14:00:00Z", "2026-10-23T14:00:30Z", "t1", "d4")
	if h.reason() != "text_changed" {
		t.Fatal("a mention vouched:", h.reason())
	}
}

// The re-queue of a closed row beyond the skew is queued clean with the
// proof.
func TestIntakeRequeueBeyondTheSkewIsCleanWhenHistoryProvedIt(t *testing.T) {
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	h.close()
	if got := h.signalClean("S1", "2026-10-23T12:00:00Z", "2026-10-23T12:05:00Z", "t1", "d5"); got != "queued" {
		t.Fatal(got)
	}
	if h.reason() != "" || len(h.maybeNotices()) != 0 || h.stored() != "t1|d5" {
		t.Fatal(h.reason(), h.stored())
	}
}

// The status tells a maybe flag from a definite one; both are changed.
func TestIntakeTextStatusTellsMaybeFromDefinite(t *testing.T) {
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	if st := h.status(); st["changed"] != false || st["maybe"] != false {
		t.Fatal(st)
	}
	if out := h.check("t1", "d2"); out["changed"] != true || out["maybe"] != false {
		t.Fatal(out)
	}
	if st := h.status(); st["changed"] != true || st["maybe"] != false {
		t.Fatal("definite:", st)
	}
	m := newTextHarness(t)
	m.signal("2026-10-23T12:00:00Z", "2026-10-23T12:05:00Z", "t1", "d1")
	if st := m.status(); st["changed"] != true || st["maybe"] != true || m.reason() != "text_maybe" {
		t.Fatal("maybe:", st, m.reason())
	}
	// The intake pass's own check of a maybe-flagged row reports it too.
	if out := m.check("t1", "d1"); out["changed"] != true || out["maybe"] != true {
		t.Fatal(out)
	}
	if st := (newTextHarness(t)).status(); st["tracked"] != false || st["maybe"] != false {
		t.Fatal(st)
	}
}

// A delivered maybe flag keeps its hedge in the queue (text_maybe), is a
// text mark, and a re-vouch clears it.
func TestIntakeDeliveredMaybeKeepsItsHedgeUntilReVouched(t *testing.T) {
	h := newTextHarness(t)
	h.signal("2026-10-23T12:00:00Z", "2026-10-23T12:05:00Z", "t1", "d1")
	var reason string
	if err := h.s.db.QueryRow(`SELECT reason FROM intake_queue`).Scan(&reason); err != nil || reason != "text_maybe" || !isTextMark(reason) || !isMaybeMark(reason) {
		t.Fatal(reason, err)
	}
	var n int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM intake_queue WHERE ` + sqlTextMark).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if rows, _ := h.call("intake_list", "a", map[string]any{}).([]map[string]any); len(rows) != 1 || rows[0]["reason"] != "text_maybe" {
		t.Fatal("intake list:", h.call("intake_list", "a", map[string]any{}))
	}
	h.signal("2026-10-23T13:00:00Z", "2026-10-23T13:00:01Z", "t1", "d2")
	if h.reason() != "" || h.status()["changed"] != false || h.status()["maybe"] != false {
		t.Fatal("not cleared:", h.reason())
	}
}
