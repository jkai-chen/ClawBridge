package watcher

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearprovider"
)

// logCapture collects the watcher's log lines.
func logCapture(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	prev := logf
	logf = func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, a...))
	}
	t.Cleanup(func() { logf = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func intakeLines(lines []string, part string) []string {
	out := []string{}
	for _, l := range lines {
		if strings.HasPrefix(l, "watcher: intake:") && strings.Contains(l, part) {
			out = append(out, l)
		}
	}
	return out
}

func rowFor(e *env, issue string) map[string]any {
	for _, r := range e.queue() {
		if r["issue_uuid"] == issue {
			return r
		}
	}
	return nil
}

// s1Env starts S1 with a trusted person, issueA labelled, and the first
// (cursor-starting) poll done.
func s1Env(t *testing.T) *env {
	t.Helper()
	e := setup(t)
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	e.f.labelled = []string{issueA}
	e.tick()
	return e
}

// pastWindow makes the watcher's clock half an hour on, so every issue the
// fake serves (updated just now) is older than intakeLag: no candidate waits.
func pastWindow(e *env) {
	e.w.Now = func() time.Time { return time.Now().Add(30 * time.Minute) }
}

// nextPoll is the S1 source's next poll, relative to the real clock.
func nextPoll(e *env) time.Duration {
	var next string
	e.queryRow(`SELECT next_poll_at FROM watch_cursors WHERE source='intake-s1'`, &next)
	at, err := time.Parse(time.RFC3339Nano, next)
	if err != nil {
		e.t.Fatal(err)
	}
	return time.Until(at)
}

func added() map[string]any { return map[string]any{"addedLabelIds": []string{labelNeeds}} }

// The S1 query asks for no delegate field, the S2 query does: one signal's
// fields can never break the other's.
func TestHistoryQueriesAreSplit(t *testing.T) {
	if strings.Contains(historyQueryS1, "Delegate") || !strings.Contains(historyQueryS1, "addedLabelIds") {
		t.Fatal(historyQueryS1)
	}
	if !strings.Contains(historyQueryS2, "toDelegate") || strings.Contains(historyQueryS2, "addedLabelIds") {
		t.Fatal(historyQueryS2)
	}
}

// A normal label add by a trusted person is queued trusted, as before.
func TestIntakeS1TrustedAddIsUnchanged(t *testing.T) {
	lines := logCapture(t)
	e := s1Env(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.history[issueA] = []map[string]any{entry(now, trustedUser, added())}
	e.dueNow()
	e.tick()
	if r := rowFor(e, issueA); r == nil || r["trigger_cap"] != "auto" {
		t.Fatal(e.queue())
	}
	if got := intakeLines(lines(), "untrusted"); len(got) != 0 {
		t.Fatal(got)
	}
}

// A coalesced record without the label add leaves S1 with no transition:
// skipped and logged with its reason, and no line carries issue text.
func TestIntakeS1TransitionNotFoundIsSkippedAndLogged(t *testing.T) {
	lines := logCapture(t)
	e := s1Env(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.history[issueA] = []map[string]any{entry(now, trustedUser, map[string]any{"addedLabelIds": []string{}})}
	pastWindow(e)
	e.dueNow()
	tick := e.tick()
	if len(e.queue()) != 0 || tick.Intake.Skipped["transition_not_found"] != 1 {
		t.Fatalf("%+v %v", tick.Intake, e.queue())
	}
	want := "watcher: intake: project " + e.pid + " S1 FIX-" + issueA[:1] + " skipped: transition_not_found"
	if got := intakeLines(lines(), "skipped: transition_not_found"); len(got) != 1 || got[0] != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	for _, l := range lines() {
		if strings.Contains(l, "title "+issueA[:1]) {
			t.Fatal("the title escaped:", l)
		}
	}
}

// A history read Linear refuses as invalid names its field in the log, the
// issue is queued untrusted once, and a second pass neither duplicates nor
// reopens it.
func TestIntakeS1ValidationErrorQueuesUntrustedOnce(t *testing.T) {
	lines := logCapture(t)
	e := s1Env(t)
	pastWindow(e)
	e.f.historyErr = &linearapp.Error{Code: "query_invalid", Field: "addedLabelIds"}
	e.dueNow()
	tick := e.tick()
	r := rowFor(e, issueA)
	if r == nil || r["trigger_cap"] != "notify" || len(tick.Intake.Queued) != 1 {
		t.Fatalf("%v %+v", e.queue(), tick.Intake)
	}
	want := "watcher: intake: project " + e.pid + " S1 FIX-" + issueA[:1] + " queued untrusted: actor_unreadable (graphql_validation:addedLabelIds)"
	if got := intakeLines(lines(), "queued untrusted"); len(got) != 1 || got[0] != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if n := len(e.inbox("coord", "intake")); n != 1 {
		t.Fatal("coordinator notices:", n)
	}
	// Dismissed, then seen again: not reopened.
	e.call("intake_dismiss", "coord", map[string]any{"issue_uuid": issueA})
	e.dueNow()
	tick = e.tick()
	if r = rowFor(e, issueA); len(e.queue()) != 1 || r["state"] != "dismissed" || len(tick.Intake.Queued) != 0 || tick.Intake.Skipped["actor_unreadable"] != 1 {
		t.Fatalf("%v %+v", e.queue(), tick.Intake)
	}
	if n := len(e.inbox("coord", "intake")); n != 1 {
		t.Fatal("coordinator notices:", n)
	}
	sum := intakeLines(lines(), " skips: actor_unreadable=1")
	if len(sum) != 1 || sum[0] != "watcher: intake: project "+e.pid+" skips: actor_unreadable=1" {
		t.Fatal(sum)
	}
}

// The skip line of a row the queue already has is logged once an hour.
func TestIntakeSkipLogIsRateLimited(t *testing.T) {
	lines := logCapture(t)
	e := s1Env(t)
	clock := time.Now().Add(30 * time.Minute)
	e.w.Now = func() time.Time { return clock }
	e.f.historyErr = &linearapp.Error{Code: "unavailable", Status: 502}
	e.dueNow()
	e.tick() // queues untrusted
	e.call("intake_dismiss", "coord", map[string]any{"issue_uuid": issueA})
	count := func() int { return len(intakeLines(lines(), "skipped: actor_unreadable (http_502)")) }
	for i, step := range []struct {
		advance time.Duration
		total   int
	}{{0, 1}, {10 * time.Minute, 1}, {40 * time.Minute, 1}, {20 * time.Minute, 2}} {
		clock = clock.Add(step.advance)
		e.dueNow()
		e.tick()
		if count() != step.total {
			t.Fatal(i, "lines:", count(), lines())
		}
	}
}

// S2: a delegation by a trusted person is trusted; with no delegation
// entry, or an unreadable history, the issue is queued untrusted once.
func TestIntakeS2Fallbacks(t *testing.T) {
	lines := logCapture(t)
	e := setup(t)
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	e.tick()
	pastWindow(e)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.extra = map[string]linearprovider.IssueV2{issueC: {Issue: linearprovider.Issue{ID: issueC, Identifier: "FIX-7", Title: "title 7", TeamID: team}, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: now}}
	e.f.delegated = []string{issueA, issueB}
	e.f.history[issueA] = []map[string]any{entry(now, trustedUser, map[string]any{"toDelegate": map[string]any{"id": appUser}, "fromDelegate": nil})}
	e.f.history[issueB] = []map[string]any{entry(now, trustedUser, map[string]any{"toDelegate": nil})}
	e.dueNow()
	e.tick()
	if r := rowFor(e, issueA); r == nil || r["trigger_cap"] != "auto" {
		t.Fatal(e.queue())
	}
	if r := rowFor(e, issueB); r == nil || r["trigger_cap"] != "notify" {
		t.Fatal(e.queue())
	}
	if got := intakeLines(lines(), "S2 FIX-"+issueB[:1]+" queued untrusted: transition_not_found"); len(got) != 1 {
		t.Fatal(lines())
	}
	// A read error, only on S2's query: untrusted once, and a repeat pass
	// neither duplicates nor reopens.
	e.f.historyErr, e.f.historyOnly = &linearapp.Error{Code: "query_invalid", Field: "toDelegate"}, historyQueryS2
	e.f.delegated = []string{issueC}
	e.dueNow()
	e.tick()
	e.dueNow()
	e.tick()
	if r := rowFor(e, issueC); r == nil || r["trigger_cap"] != "notify" || len(e.queue()) != 3 {
		t.Fatal(e.queue())
	}
	if got := intakeLines(lines(), "S2 FIX-"+issueC[:1]+" queued untrusted: actor_unreadable (graphql_validation:toDelegate)"); len(got) != 1 {
		t.Fatal(lines())
	}
}

// S1 keeps working while S2's query is broken: the queries are split.
func TestIntakeS1SurvivesABrokenS2Query(t *testing.T) {
	e := s1Env(t)
	e.f.historyErr, e.f.historyOnly = &linearapp.Error{Code: "query_invalid", Field: "toDelegate"}, historyQueryS2
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.history[issueA] = []map[string]any{entry(now, trustedUser, added())}
	e.dueNow()
	e.tick()
	if r := rowFor(e, issueA); r == nil || r["trigger_cap"] != "auto" {
		t.Fatal(e.queue())
	}
}

// Linear's history can trail a change: an S1 candidate with an empty
// history is pending, the source is read again in two minutes, and the
// trusted entry that appears by then queues it trusted.
func TestIntakeS1PendingThenTrusted(t *testing.T) {
	lines := logCapture(t)
	e := s1Env(t)
	clock := time.Now()
	e.w.Now = func() time.Time { return clock }
	e.f.history[issueA] = nil
	e.dueNow()
	tick := e.tick()
	if len(e.queue()) != 0 || tick.Intake.Skipped["pending_transition_not_found"] != 1 {
		t.Fatalf("%+v %v", tick.Intake, e.queue())
	}
	want := "watcher: intake: project " + e.pid + " S1 FIX-" + issueA[:1] + " pending: transition_not_found (retry)"
	if got := intakeLines(lines(), "pending:"); len(got) != 1 || got[0] != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if d := nextPoll(e); d > 2*time.Minute+5*time.Second || d < time.Minute {
		t.Fatal("the next poll is not two minutes away:", d)
	}
	// Two minutes on, the history has caught up.
	clock = clock.Add(2 * time.Minute)
	e.f.history[issueA] = []map[string]any{entry(time.Now().UTC().Format(time.RFC3339Nano), trustedUser, added())}
	e.dueNow()
	e.tick()
	if r := rowFor(e, issueA); r == nil || r["trigger_cap"] != "auto" {
		t.Fatal(e.queue())
	}
	if d := nextPoll(e); d < 4*time.Minute {
		t.Fatal("no pending left, yet the next poll is soon:", d)
	}
}

// With the history empty past the window, the last pass gives up: S1's
// missing transition is skipped and logged, and the cadence is normal.
func TestIntakeS1PendingGivesUpAfterTheWindow(t *testing.T) {
	lines := logCapture(t)
	e := s1Env(t)
	clock := time.Now()
	e.w.Now = func() time.Time { return clock }
	e.f.history[issueA] = nil
	for _, step := range []time.Duration{0, 4 * time.Minute, 4 * time.Minute, 4 * time.Minute} {
		clock = clock.Add(step)
		e.dueNow()
		e.tick()
	}
	if len(e.queue()) != 0 {
		t.Fatal(e.queue())
	}
	if got := intakeLines(lines(), "skipped: transition_not_found"); len(got) != 1 {
		t.Fatal(lines())
	}
	if d := nextPoll(e); d < 4*time.Minute {
		t.Fatal("pending after the window:", d)
	}
}

// A persisting read error is pending, then untrusted once after the window.
func TestIntakeS2ReadErrorPendingThenUntrusted(t *testing.T) {
	lines := logCapture(t)
	e := setup(t)
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	e.tick()
	clock := time.Now()
	e.w.Now = func() time.Time { return clock }
	e.f.delegated = []string{issueA}
	e.f.historyErr = &linearapp.Error{Code: "unavailable", Status: 503}
	e.dueNow()
	e.tick()
	if len(e.queue()) != 0 {
		t.Fatal(e.queue())
	}
	if got := intakeLines(lines(), "S2 FIX-"+issueA[:1]+" pending: actor_unreadable (http_503, retry)"); len(got) != 1 {
		t.Fatal(lines())
	}
	clock = clock.Add(11 * time.Minute)
	e.dueNow()
	e.tick()
	e.dueNow()
	e.tick()
	if r := rowFor(e, issueA); r == nil || r["trigger_cap"] != "notify" || len(e.queue()) != 1 {
		t.Fatal(e.queue())
	}
	if got := intakeLines(lines(), "queued untrusted: actor_unreadable (http_503)"); len(got) != 1 {
		t.Fatal(lines())
	}
}

// creatorEnv is S1 with an empty history for issueA, the label on it, and
// a creator who made the issue 30 seconds before its last update. clock is
// the watcher's time.
func creatorEnv(t *testing.T, creator string) (*env, *time.Time) {
	t.Helper()
	e := s1Env(t)
	clock := time.Now()
	e.w.Now = func() time.Time { return clock }
	e.f.issueLabels = map[string][]string{issueA: {labelNeeds}}
	e.f.creator, e.f.createdAt = creator, time.Now().Add(-30*time.Second).UTC().Format(time.RFC3339Nano)
	e.f.history[issueA] = nil
	return e, &clock
}

// passes runs polls spaced by step, the cursor moving as the soon re-polls
// move it, and returns the queue rows' caps by issue after each.
func passes(e *env, clock *time.Time, n int, step time.Duration) {
	for i := 0; i < n; i++ {
		*clock = clock.Add(step)
		e.dueNow()
		e.tick()
	}
}

// A label its creator set at creation leaves no history. While the window
// is open the candidate waits (another actor's entry may lag); at its end a
// trusted creator is credited.
func TestIntakeS1CreatorTrustedAtWindowEnd(t *testing.T) {
	lines := logCapture(t)
	e, clock := creatorEnv(t, trustedUser)
	passes(e, clock, 1, 0)
	if len(e.queue()) != 0 {
		t.Fatal("credited on the first pass:", e.queue())
	}
	passes(e, clock, 3, 4*time.Minute)
	r := rowFor(e, issueA)
	if r == nil || r["trigger_cap"] != "auto" || len(e.queue()) != 1 {
		t.Fatal(e.queue(), lines())
	}
	want := "watcher: intake: project " + e.pid + " S1 FIX-" + issueA[:1] + " queued: creator (trusted)"
	if got := intakeLines(lines(), "queued: creator"); len(got) != 1 || got[0] != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	// Dismissed, then read again: it stays closed.
	e.call("intake_dismiss", "coord", map[string]any{"issue_uuid": issueA})
	passes(e, clock, 2, 4*time.Minute)
	if r = rowFor(e, issueA); r["state"] != "dismissed" || len(e.queue()) != 1 {
		t.Fatal("a dismissed item was reopened:", e.queue())
	}
}

// An untrusted creator is queued untrusted, once, after the window.
func TestIntakeS1CreatorUntrustedAtWindowEnd(t *testing.T) {
	lines := logCapture(t)
	e, clock := creatorEnv(t, strangerID)
	passes(e, clock, 4, 4*time.Minute)
	if r := rowFor(e, issueA); r == nil || r["trigger_cap"] != "notify" || len(e.queue()) != 1 {
		t.Fatal(e.queue(), lines())
	}
	if got := intakeLines(lines(), "queued: creator (untrusted)"); len(got) != 1 {
		t.Fatal(lines())
	}
}

// Another actor's entry that lags into the window is credited to that
// actor, not to the creator.
func TestIntakeS1LaggingOtherActorBeatsTheCreator(t *testing.T) {
	e, clock := creatorEnv(t, strangerID)
	passes(e, clock, 1, 0)
	e.f.history[issueA] = []map[string]any{entry(time.Now().UTC().Format(time.RFC3339Nano), trustedUser, added())}
	passes(e, clock, 1, 2*time.Minute)
	if r := rowFor(e, issueA); r == nil || r["trigger_cap"] != "auto" {
		t.Fatal(e.queue())
	}
}

// No creator proof: an old issue, a cut-off history, a creator that is the
// app or an integration, a bot's entry, or another actor's entry without the
// label. Nothing is queued, trusted or not.
func TestIntakeS1NoCreatorProof(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	other := []map[string]any{entry(time.Now().UTC().Format(time.RFC3339Nano), strangerID, map[string]any{"addedLabelIds": []string{}})}
	bot := []map[string]any{entry(time.Now().UTC().Format(time.RFC3339Nano), trustedUser, map[string]any{"botActor": map[string]any{"id": "bot"}})}
	for name, set := range map[string]func(f *fakeLinear){
		"old issue":    func(f *fakeLinear) { f.createdAt = old },
		"cut off":      func(f *fakeLinear) { f.historyMore = true },
		"the app":      func(f *fakeLinear) { f.creator = appUser },
		"integration":  func(f *fakeLinear) { f.creatorApp = true },
		"bot entry":    func(f *fakeLinear) { f.history[issueA] = bot },
		"other actor":  func(f *fakeLinear) { f.history[issueA] = other },
		"no createdAt": func(f *fakeLinear) { f.createdAt = "" },
	} {
		e, clock := creatorEnv(t, trustedUser)
		set(e.f)
		passes(e, clock, 4, 4*time.Minute)
		if len(e.queue()) != 0 {
			t.Fatal(name, "was queued:", e.queue())
		}
	}
}

// The S2 query asks for no field the live schema isn't known to have.
func TestHistoryQueryS2AsksOnlyForToDelegate(t *testing.T) {
	if strings.Contains(historyQueryS2, "fromDelegate") || !strings.Contains(historyQueryS2, "toDelegate") {
		t.Fatal(historyQueryS2)
	}
}

// The pass summary is logged for a skip that is not a spill, or when the
// teams without a label change; spill counts alone are quiet.
func TestLogSkipsQuietForSpillOnly(t *testing.T) {
	lines := logCapture(t)
	w := &Watcher{}
	spill := func(teams ...string) *IntakePoll {
		return &IntakePoll{Skipped: map[string]int{"other_team": 2, "no_label": 1}, NoLabel: teams}
	}
	w.logSkips("p", spill())
	w.logSkips("p", spill("t1"))
	w.logSkips("p", spill("t1"))
	w.logSkips("p", spill("t1", "t2"))
	w.logSkips("p", &IntakePoll{Skipped: map[string]int{"actor_unreadable": 1, "other_team": 1}})
	got := lines()
	want := []string{
		"watcher: intake: project p skips: no_label=1 other_team=2 no_label_teams: t1",
		"watcher: intake: project p skips: no_label=1 other_team=2 no_label_teams: t1,t2",
		"watcher: intake: project p skips: actor_unreadable=1 other_team=1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", got)
	}
}
