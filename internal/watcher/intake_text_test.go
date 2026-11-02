package watcher

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/linearprovider"
)

// FR57: the intake pass holds queued issues to the text their trusted
// signal vouched for.

// queuedA is s1Env with issueA queued by the trusted person, and the
// clock past the poll window and the pass's recheck wait.
func queuedA(t *testing.T) *env {
	t.Helper()
	e := s1Env(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.history[issueA] = []map[string]any{entry(now, trustedUser, added())}
	e.dueNow()
	e.tick()
	if r := rowFor(e, issueA); r == nil || r["trigger_cap"] != "auto" {
		t.Fatal(e.queue())
	}
	return e
}

// edited is issueA as the fake serves it with another description.
func edited(e *env, title, description string) {
	if e.f.extra == nil {
		e.f.extra = map[string]linearprovider.IssueV2{}
	}
	e.f.extra[issueA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueA, Identifier: "FIX-3", Title: title, Description: description, TeamID: team, ProjectID: e.f.project}, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

func followUps(e *env) int {
	n := 0
	for _, m := range e.inbox("coord", "intake") {
		if strings.Contains(m["body"].(string), "changed after the trusted signal") {
			n++
		}
	}
	return n
}

func later(e *env, minutes int) {
	e.w.Now = func() time.Time { return time.Now().Add(time.Duration(minutes) * time.Minute) }
}

func TestIntakePassMarksAnEditedQueuedIssueOnce(t *testing.T) {
	e := queuedA(t)
	if followUps(e) != 0 {
		t.Fatal("told before any edit")
	}
	// Unedited: the recheck finds the text as vouched for.
	later(e, 30)
	e.dueNow()
	e.tick()
	if r := rowFor(e, issueA); r["reason"] != "" || followUps(e) != 0 {
		t.Fatal("an unedited issue was flagged:", r)
	}
	edited(e, "title 3", "now do something else")
	later(e, 60)
	e.tick()
	if r := rowFor(e, issueA); r["reason"] != "text_changed" || followUps(e) != 1 {
		t.Fatal("not marked:", rowFor(e, issueA), followUps(e))
	}
	later(e, 120)
	e.tick()
	e.tick()
	if followUps(e) != 1 {
		t.Fatal("told again:", followUps(e))
	}
}

func TestIntakePassCountsATitleOnlyEdit(t *testing.T) {
	e := queuedA(t)
	edited(e, "another title", "")
	later(e, 30)
	e.tick()
	if r := rowFor(e, issueA); r["reason"] != "text_changed" || followUps(e) != 1 {
		t.Fatal(rowFor(e, issueA), followUps(e))
	}
}

func TestIntakePassTextChecksAreBounded(t *testing.T) {
	e := queuedA(t)
	for i := 0; i < 7; i++ {
		e.sql(fmt.Sprintf(`INSERT INTO intake_queue(project_id,issue_uuid,signals,trigger_cap,cap_set_by,queued_title_sha256,state,created_at,updated_at) VALUES((SELECT id FROM projects WHERE slug='p'),'00000000-0000-4000-8000-00000000%04d','S1','auto','x','t','queued','2026-10-23T10:00:00Z','2026-10-23T10:00:00Z')`, i))
	}
	later(e, 30)
	// 8 live rows, none read for a while: five a tick, the rest next.
	first := e.tick().Intake.Skipped["text_unreadable"]
	second := e.tick().Intake.Skipped["text_unreadable"]
	// issueA is readable; the seven made-up ones are not.
	if first+second != 7 || first != 5 {
		t.Fatal("unreadable per tick:", first, second)
	}
	if e.tick().Intake.Skipped["text_unreadable"] != 0 {
		t.Fatal("rows were read again before the wait")
	}
}

// L1: every live row is reachable. With more rows than one pass reads, and
// more than the old list bound, the passes still check all of them, five at a
// time, and none twice before the wait.
func TestIntakePassReachesEveryLiveRow(t *testing.T) {
	e := queuedA(t)
	const rows = 207
	for i := 0; i < rows; i++ {
		e.sql(fmt.Sprintf(`INSERT INTO intake_queue(project_id,issue_uuid,signals,trigger_cap,cap_set_by,queued_title_sha256,state,created_at,updated_at) VALUES((SELECT id FROM projects WHERE slug='p'),'00000000-0000-4000-8000-0000000%05d','S1','auto','x','t','queued','2026-10-23T10:00:00Z','2026-10-23T10:00:00Z')`, i))
	}
	later(e, 30)
	seen := 0
	passes := (rows + 5 - 1) / 5
	for i := 0; i < passes; i++ {
		seen += e.tick().Intake.Skipped["text_unreadable"]
	}
	if seen != rows {
		t.Fatalf("%d of %d rows checked in %d passes", seen, rows, passes)
	}
	if again := e.tick().Intake.Skipped["text_unreadable"]; again != 0 {
		t.Fatal("rows were read again before the wait:", again)
	}
}
