package watcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// FR58: before the skew rule flags a trusted signal's issue as a maybe, the
// intake pass reads Linear's history and holds the text as vouched when it
// shows only other fields changed.

// histNode is one history entry of the app's read: createdAt and the fields
// that make it a known kind.
func histNode(at time.Time, fields map[string]any) map[string]any {
	n := map[string]any{"createdAt": at.UTC().Format(time.RFC3339Nano), "actorId": trustedUser}
	for k, v := range fields {
		n[k] = v
	}
	return n
}

var (
	priorityChange = map[string]any{"fromPriority": 2.0, "toPriority": 1.0}
	labelChange    = map[string]any{"addedLabelIds": []string{"cccccccc-cccc-4ccc-8ccc-000000000009"}}
	titleChange    = map[string]any{"fromTitle": "old", "toTitle": "new"}
	descChange     = map[string]any{"updatedDescription": true}
)

// historyText counts the app's history reads of the text check.
func historyReads(e *env) int {
	n := 0
	for _, q := range e.f.queries {
		if strings.Contains(q, "query IssueHistory(") {
			n++
		}
	}
	return n
}

// skewedA is s1Env where the trusted label add was ten minutes ago and the
// issue was last updated just now (beyond the skew). hist is the app's
// history read, built from the transition time and the update time.
func skewedA(t *testing.T, hist func(at, updated time.Time) []map[string]any) (*env, time.Time, time.Time) {
	t.Helper()
	e := s1Env(t)
	at := time.Now().UTC().Add(-10 * time.Minute)
	updated := time.Now().UTC()
	e.f.history[issueA] = []map[string]any{entry(at.Format(time.RFC3339Nano), trustedUser, added())}
	e.f.extra = map[string]linearprovider.IssueV2{issueA: {Issue: linearprovider.Issue{ID: issueA, Identifier: "FIX-3", Title: "title 3", TeamID: team, ProjectID: e.f.project}, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: updated.Format(time.RFC3339Nano)}}
	if hist != nil {
		e.f.textHist = map[string][]map[string]any{issueA: hist(at, updated)}
	}
	e.dueNow()
	return e, at, updated
}

func otherChanges(at, updated time.Time) []map[string]any {
	return []map[string]any{
		histNode(updated.Add(-time.Second), priorityChange),
		histNode(updated.Add(-time.Minute), labelChange),
		histNode(at, labelChange),
	}
}

func maybeNotices(e *env) int {
	n := 0
	for _, m := range e.inbox("coord", "intake") {
		if strings.Contains(m["body"].(string), "may have changed") {
			n++
		}
	}
	return n
}

func TestIntakeHistoryOnlyOtherChangesIsStampedClean(t *testing.T) {
	e, _, _ := skewedA(t, otherChanges)
	tick := e.tick()
	r := rowFor(e, issueA)
	if r == nil || r["trigger_cap"] != "auto" || r["reason"] != "" || maybeNotices(e) != 0 || followUps(e) != 0 {
		t.Fatal("flagged on an unrelated change:", r, maybeNotices(e))
	}
	if historyReads(e) != 1 {
		t.Fatal("history reads:", historyReads(e))
	}
	// The read is in the pass's charge: the same pass within the skew spends one call less.
	e2, at, _ := skewedA(t, nil)
	e2.f.extra[issueA] = linearprovider.IssueV2{Issue: e2.f.extra[issueA].Issue, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: at.Add(time.Second).Format(time.RFC3339Nano)}
	tick2 := e2.tick()
	if historyReads(e2) != 0 || tick.Intake.Calls != tick2.Intake.Calls+1 {
		t.Fatal("charge:", tick.Intake.Calls, tick2.Intake.Calls, historyReads(e2))
	}
	if r := rowFor(e2, issueA); r == nil || r["reason"] != "" {
		t.Fatal(r)
	}
}

func TestIntakeHistoryTextChangeKeepsTheMaybeFlag(t *testing.T) {
	for name, change := range map[string]map[string]any{"description": descChange, "title": titleChange} {
		t.Run(name, func(t *testing.T) {
			// Linear groups changes made together into one entry, so the
			// text edit may come with a known kind (a priority change) and
			// is told by its own fields, not by being an unknown kind.
			for shape, mixed := range map[string]map[string]any{"alone": change, "with a priority change": {"fromPriority": 2.0, "toPriority": 1.0, "fromTitle": change["fromTitle"], "toTitle": change["toTitle"], "updatedDescription": change["updatedDescription"]}} {
				e, _, _ := skewedA(t, func(at, updated time.Time) []map[string]any {
					return []map[string]any{histNode(updated.Add(-time.Second), mixed), histNode(updated.Add(-time.Minute), priorityChange), histNode(at, labelChange)}
				})
				e.tick()
				if r := rowFor(e, issueA); r == nil || r["reason"] != "text_maybe" || maybeNotices(e) != 1 {
					t.Fatal(shape, "not flagged:", rowFor(e, issueA), maybeNotices(e))
				}
			}
		})
	}
}

// Whatever the history cannot prove leaves the skew rule's flag.
func TestIntakeHistoryThatProvesNothingKeepsTheMaybeFlag(t *testing.T) {
	cases := map[string]func(e *env){
		"read error": func(e *env) { e.f.textHistErr = errors.New("history unreadable") },
		"incomplete walk": func(e *env) {
			// Never reaches the transition within the pages it may read.
			e.f.textHist[issueA], e.f.textHistMore = e.f.textHist[issueA][:2], true
		},
		"unknown kind": func(e *env) {
			e.f.textHist[issueA][1] = histNode(time.Now().UTC().Add(-time.Minute), nil)
		},
		"not caught up": func(e *env) {
			// Only older entries: the update itself has no entry yet.
			e.f.textHist[issueA] = e.f.textHist[issueA][1:]
		},
		"empty history": func(e *env) { e.f.textHist[issueA] = []map[string]any{} },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			e, _, _ := skewedA(t, otherChanges)
			mod(e)
			e.tick()
			if r := rowFor(e, issueA); r == nil || r["reason"] != "text_maybe" || maybeNotices(e) != 1 {
				t.Fatal("not flagged:", rowFor(e, issueA), maybeNotices(e))
			}
		})
	}
}

// A budget that has run out reads no history and proves nothing.
func TestIntakeHistoryNeedsBudget(t *testing.T) {
	e, at, updated := skewedA(t, otherChanges)
	r := &intakeRun{w: e.w, poll: &IntakePoll{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ev := bus.IntakeEvent{Signal: "S1", Actor: trustedUser, Trusted: true, At: at.Format(time.RFC3339Nano), Issue: bus.IntakeIssue{UUID: issueA, TitleSHA256: "t", UpdatedAt: updated.Format(time.RFC3339Nano)}}
	if !r.textUnchanged(context.Background(), ev) {
		t.Fatal("the fixture's history should prove the text with room left")
	}
	r.poll.Calls, e.f.queries = 0, nil
	if r.textUnchanged(ctx, ev) || historyReads(e) != 0 || r.poll.Calls != 0 {
		t.Fatal("read with no room left")
	}
}

// Within the skew no history is read.
func TestIntakeWithinTheSkewReadsNoHistory(t *testing.T) {
	e := queuedA(t)
	if historyReads(e) != 0 || rowFor(e, issueA)["reason"] != "" {
		t.Fatal(historyReads(e), rowFor(e, issueA))
	}
}

// A history that shows a lagging update (the newest entry is older than the
// update minus the skew) with only other fields still leaves the flag; the
// same history with the update explained does not.
func TestIntakeHistoryMustExplainTheLastUpdate(t *testing.T) {
	e, _, _ := skewedA(t, func(at, updated time.Time) []map[string]any {
		return []map[string]any{histNode(updated.Add(-time.Minute), priorityChange), histNode(at, labelChange)}
	})
	e.tick()
	if r := rowFor(e, issueA); r == nil || r["reason"] != "text_maybe" || maybeNotices(e) != 1 {
		t.Fatal("a lagging history proved the text:", rowFor(e, issueA))
	}
}

// A re-vouch (a new label add) beyond the skew clears a flag when the
// history shows only other fields changed since it, and not when it shows
// a text change.
func TestIntakeReVouchBeyondTheSkewNeedsTheHistoryProof(t *testing.T) {
	for name, change := range map[string]map[string]any{"other fields only": priorityChange, "description updated": descChange} {
		t.Run(name, func(t *testing.T) {
			// First signal: its issue changed in the text, so it is flagged.
			e, _, _ := skewedA(t, func(at, updated time.Time) []map[string]any {
				return []map[string]any{histNode(updated.Add(-time.Second), descChange), histNode(at, labelChange)}
			})
			e.tick()
			if r := rowFor(e, issueA); r == nil || r["reason"] != "text_maybe" {
				t.Fatal(rowFor(e, issueA))
			}
			e.sql(`UPDATE intake_queue SET queued_updated_at='2000-01-01T00:00:00Z'`)
			// The person adds the label again two minutes ago; the issue was
			// updated after that.
			again := time.Now().UTC().Add(-2 * time.Minute)
			updated := time.Now().UTC()
			e.f.history[issueA] = []map[string]any{entry(again.Format(time.RFC3339Nano), trustedUser, added())}
			e.f.extra[issueA] = linearprovider.IssueV2{Issue: e.f.extra[issueA].Issue, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: updated.Format(time.RFC3339Nano)}
			e.f.textHist = map[string][]map[string]any{issueA: {histNode(updated.Add(-time.Second), change), histNode(again, labelChange)}}
			e.dueNow()
			e.tick()
			r := rowFor(e, issueA)
			cleared := name == "other fields only"
			if r == nil || (r["reason"] == "") != cleared {
				t.Fatal("re-vouch:", r)
			}
		})
	}
}
