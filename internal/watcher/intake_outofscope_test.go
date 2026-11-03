package watcher

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// FR69: a queued issue outside the binding's verified scope is counted as
// out_of_scope, never logged by ID, and read again only after a growing wait.

// queueOut queues issueOut, which the fake serves from another team, so
// IssueV2 refuses it as outside the binding.
func queueOut(e *env) {
	e.sql(fmt.Sprintf(`INSERT INTO intake_queue(project_id,issue_uuid,signals,trigger_cap,cap_set_by,queued_title_sha256,state,created_at,updated_at) VALUES((SELECT id FROM projects WHERE slug='p'),'%s','S1','auto','x','t','queued','2026-10-23T10:00:00Z','2026-10-23T10:00:00Z')`, issueOut))
}

func captureLog(t *testing.T) *[]string {
	t.Helper()
	var logged []string
	old := logf
	logf = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	t.Cleanup(func() { logf = old })
	return &logged
}

func TestIntakeTextCountsAnOutOfScopeRowWithoutNamingIt(t *testing.T) {
	logged := captureLog(t)
	e := queuedA(t)
	queueOut(e)
	later(e, 30)
	skipped := e.tick().Intake.Skipped
	if skipped["out_of_scope"] != 1 || skipped["text_unreadable"] != 0 {
		t.Fatal(skipped)
	}
	for _, l := range *logged {
		if strings.Contains(l, issueOut) || strings.Contains(l, "out_of_scope") || strings.Contains(l, "text_unreadable") {
			t.Fatal("logged:", l)
		}
	}
}

func TestIntakeTextBacksOffAnOutOfScopeRow(t *testing.T) {
	e := queuedA(t)
	queueOut(e)
	base := time.Now()
	at := func(d time.Duration) int {
		e.w.Now = func() time.Time { return base.Add(d) }
		return e.tick().Intake.Skipped["out_of_scope"]
	}
	// Read, then not again for an hour, then at an hour, then two more hours.
	steps := []struct {
		at   time.Duration
		want int
	}{
		{30 * time.Minute, 1},
		{89 * time.Minute, 0},
		{90 * time.Minute, 1},
		{3*time.Hour + 29*time.Minute, 0},
		{3*time.Hour + 30*time.Minute, 1},
		// 4 h wait, then 8, 16, then the cap.
		{7*time.Hour + 29*time.Minute, 0},
		{7*time.Hour + 30*time.Minute, 1},
		{15*time.Hour + 29*time.Minute, 0},
		{15*time.Hour + 30*time.Minute, 1},
		{31*time.Hour + 29*time.Minute, 0},
		{31*time.Hour + 30*time.Minute, 1},
		{55*time.Hour + 29*time.Minute, 0},
		{55*time.Hour + 30*time.Minute, 1},
		{79*time.Hour + 29*time.Minute, 0},
		{79*time.Hour + 30*time.Minute, 1},
	}
	for _, s := range steps {
		if got := at(s.at); got != s.want {
			t.Fatalf("at %s: out_of_scope %d, want %d", s.at, got, s.want)
		}
	}
}

func TestIntakeTextBackoffEndsWhenTheRowLeavesTheQueue(t *testing.T) {
	e := queuedA(t)
	queueOut(e)
	base := time.Now()
	e.w.Now = func() time.Time { return base.Add(30 * time.Minute) }
	if e.tick().Intake.Skipped["out_of_scope"] != 1 {
		t.Fatal("not read")
	}
	if len(e.w.textBackoff) != 1 {
		t.Fatal(e.w.textBackoff)
	}
	e.sql(`UPDATE intake_queue SET state='dismissed' WHERE issue_uuid=?`, issueOut)
	e.w.Now = func() time.Time { return base.Add(40 * time.Minute) }
	e.tick()
	if len(e.w.textBackoff) != 0 {
		t.Fatal("backoff kept:", e.w.textBackoff)
	}
}

func TestIntakeTextOtherErrorsStayUnreadableAndRetryAtTheNormalWait(t *testing.T) {
	logged := captureLog(t)
	e := queuedA(t)
	queueOut(e)
	e.f.issueErr = map[string]error{issueOut: context.DeadlineExceeded}
	base := time.Now()
	at := func(d time.Duration) map[string]int {
		e.w.Now = func() time.Time { return base.Add(d) }
		return e.tick().Intake.Skipped
	}
	if s := at(30 * time.Minute); s["text_unreadable"] != 1 || s["out_of_scope"] != 0 {
		t.Fatal(s)
	}
	if s := at(39 * time.Minute); s["text_unreadable"] != 0 {
		t.Fatal("read before the normal wait:", s)
	}
	if s := at(41 * time.Minute); s["text_unreadable"] != 1 {
		t.Fatal("not retried at the normal wait:", s)
	}
	found := false
	for _, l := range *logged {
		found = found || strings.Contains(l, "text_unreadable (timeout)")
	}
	if !found {
		t.Fatal("no text_unreadable (timeout) line:", *logged)
	}
}
