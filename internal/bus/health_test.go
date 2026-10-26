package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

type healthHarness struct {
	*harness
	pid, token string
	gen        int
}

// newHealthHarness binds the project to two Linear projects, makes c the
// coordinator, and takes the watch lease as the system session.
func newHealthHarness(t *testing.T) *healthHarness {
	t.Helper()
	h := &healthHarness{harness: newHarness(t)}
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0, projectOne, projectTwo))
	if e := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&h.pid); e != nil {
		t.Fatal(e)
	}
	l := h.sys("comment_watch_lease", map[string]any{"action": "acquire"})
	h.token, h.gen = l["lease_token"].(string), l["generation"].(int)
	return h
}

func (h *healthHarness) sys(op string, a map[string]any) map[string]any {
	h.t.Helper()
	r, e := h.sysErr(op, a)
	if e != nil {
		h.t.Fatal(op, e)
	}
	return r
}

func (h *healthHarness) sysErr(op string, a map[string]any) (map[string]any, error) {
	if op != "comment_watch_lease" {
		a["lease_token"], a["generation"] = h.token, h.gen
	}
	r, e := h.s.ExecuteSystem(context.Background(), h.pid, Request{Operation: op, Args: a})
	m, _ := r.Data.(map[string]any)
	return m, e
}

func (h *healthHarness) enable(project string, rev int, policy map[string]any) map[string]any {
	h.t.Helper()
	if policy == nil {
		policy = map[string]any{"enabled": true}
	}
	return h.call("health_set", "a", map[string]any{"linear_project": project, "expected_revision": rev, "health_policy": policy}).(map[string]any)
}

func (h *healthHarness) due() {
	h.t.Helper()
	if _, e := h.s.db.Exec(`UPDATE project_health SET next_health_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		h.t.Fatal(e)
	}
}

func (h *healthHarness) notices() int {
	h.t.Helper()
	var n int
	if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE kind='project-health'`).Scan(&n); e != nil {
		h.t.Fatal(e)
	}
	return n
}

// run makes one run of project due and reports facts for it.
func (h *healthHarness) run(project string, facts map[string]any) map[string]any {
	h.t.Helper()
	h.due()
	runs := h.sys("health_due", map[string]any{"limit": 2})["runs"].([]any)
	found := false
	for _, r := range runs {
		found = found || r.(map[string]any)["linear_project"] == project
	}
	if !found {
		h.t.Fatal("not due:", runs)
	}
	return h.sys("health_report", map[string]any{"linear_project": project, "calls": 3, "facts": facts})
}

func blockedFacts(n int) map[string]any {
	ids := []any{}
	for i := 0; i < n && i < 10; i++ {
		ids = append(ids, "FJA-"+string(rune('1'+i)))
	}
	return map[string]any{"total": 10, "completed": 2, "canceled": 0, "started": 3, "stalled": []any{}, "stalled_count": 0, "unverified": 0, "blocked": ids, "blocked_count": n, "target_date": "", "start_date": "2026-01-01", "start_from": "startDate"}
}

func TestHealthPolicyRules(t *testing.T) {
	p, err := ParseHealthPolicy(map[string]any{"enabled": true})
	if err != nil || p != func() HealthPolicy { d := defaultHealthPolicy(); d.Enabled = true; return d }() {
		t.Fatal("defaults", p, err)
	}
	for name, v := range map[string]map[string]any{
		"no enabled":       {"weekly": true},
		"post":             {"enabled": true, "post": true},
		"unknown":          {"enabled": true, "claims": 2},
		"risk not below":   {"enabled": true, "blocked_risk": 5, "blocked_off": 5},
		"pct over 100":     {"enabled": true, "stalled_off_pct": 101},
		"stalled days":     {"enabled": true, "stalled_days": 0},
		"enabled a string": {"enabled": "yes"},
	} {
		if _, err := ParseHealthPolicy(v); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	if _, err := ParseHealthPolicy(map[string]any{"enabled": true, "post": true}); err == nil || !strings.Contains(err.Error(), "not available yet") {
		t.Fatal(err)
	}
}

// The rules (design §2), each threshold, from typed facts.
func TestComputeHealthRules(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	p := defaultHealthPolicy()
	base := func(edit func(f *HealthFacts)) HealthFacts {
		f := HealthFacts{Total: 10, StartDate: "2026-01-01", StartFrom: "startDate"}
		edit(f2(&f))
		return f
	}
	for _, c := range []struct {
		name, want, signal string
		facts              HealthFacts
	}{
		{"no issues", "", "", base(func(f *HealthFacts) { f.Total = 0 })},
		{"all canceled", "", "", base(func(f *HealthFacts) { f.Canceled = 10 })},
		{"quiet", "onTrack", "", base(func(f *HealthFacts) {})},
		{"one blocked never flips", "onTrack", "blocked", base(func(f *HealthFacts) { f.BlockedCount = 1 })},
		{"two blocked", "atRisk", "blocked", base(func(f *HealthFacts) { f.BlockedCount = 2 })},
		{"five blocked", "offTrack", "blocked", base(func(f *HealthFacts) { f.BlockedCount = 5 })},
		{"stalled 1 of 10", "onTrack", "stalled", base(func(f *HealthFacts) { f.Started, f.StalledCount = 10, 1 })},
		{"stalled 2", "atRisk", "stalled", base(func(f *HealthFacts) { f.Started, f.StalledCount = 10, 2 })},
		{"stalled 20 pct", "atRisk", "stalled", base(func(f *HealthFacts) { f.Total, f.Started, f.StalledCount = 10, 5, 1 })},
		{"stalled 40 pct", "offTrack", "stalled", base(func(f *HealthFacts) { f.Total, f.Started, f.StalledCount = 10, 5, 2 })},
		{"stalled 5", "offTrack", "stalled", base(func(f *HealthFacts) { f.Total, f.Started, f.StalledCount = 100, 50, 5 })},
		// Start 1 Jan, target 30 Dec: about 42% of the time used by 1 Jun.
		{"schedule on", "onTrack", "schedule", base(func(f *HealthFacts) { f.TargetDate, f.Completed = "2026-12-30", 3 })},
		{"schedule gap over 25", "atRisk", "schedule", base(func(f *HealthFacts) { f.TargetDate, f.Completed = "2026-12-30", 1 })},
		{"schedule gap over 50", "offTrack", "schedule", base(func(f *HealthFacts) { f.TargetDate, f.Completed = "2026-07-01", 2 })},
		{"target passed", "offTrack", "schedule", base(func(f *HealthFacts) { f.TargetDate, f.Completed = "2026-05-01", 9 })},
		{"target passed done", "onTrack", "schedule", base(func(f *HealthFacts) { f.TargetDate, f.Completed = "2026-05-01", 10 })},
		{"target is today", "atRisk", "schedule", base(func(f *HealthFacts) { f.TargetDate, f.Completed = "2026-06-01", 7 })},
		{"no target skips schedule", "onTrack", "", base(func(f *HealthFacts) { f.Completed = 0 })},
	} {
		got, signals := ComputeHealth(c.facts, p, now)
		if got != c.want {
			t.Fatal(c.name, got, signals)
		}
		if c.signal != "" {
			found := false
			for _, s := range signals {
				found = found || s.Signal == c.signal && s.Health == c.want
			}
			if !found {
				t.Fatal(c.name, "no deciding signal", signals)
			}
		}
		if c.want != "" && c.facts.TargetDate == "" {
			for _, s := range signals {
				if s.Signal == "schedule" {
					t.Fatal(c.name, "a schedule signal without a target date")
				}
			}
		}
	}
	// The body says which start was used.
	_, signals := ComputeHealth(base(func(f *HealthFacts) { f.TargetDate, f.StartFrom = "2026-12-30", "createdAt" }), p, now)
	if !strings.Contains(signals[0].Detail, "creation date") || !strings.Contains(signals[0].Detail, "not estimates") {
		t.Fatal(signals[0].Detail)
	}
}

func f2(f *HealthFacts) *HealthFacts { return f }

func TestHealthSetIsPerBoundProject(t *testing.T) {
	h := newHealthHarness(t)
	got := h.call("health_get", "b", map[string]any{}).(map[string]any)
	if len(got["projects"].([]any)) != 0 || !strings.Contains(got["posting"].(string), "nothing is written to Linear") {
		t.Fatal(got)
	}
	unbound := "33333333-3333-4333-8333-333333333333"
	h.fail("health_set", "a", map[string]any{"linear_project": unbound, "expected_revision": 0, "health_policy": map[string]any{"enabled": true}})
	h.fail("health_set", "a", map[string]any{"linear_project": projectOne, "expected_revision": 0, "health_policy": map[string]any{"enabled": true, "post": true}})
	set := h.enable(projectOne, 0, nil)["projects"].([]any)[0].(map[string]any)
	if set["revision"] != 1 || set["enabled"] != true {
		t.Fatal(set)
	}
	// The slot is the project's own minute of the day, in the future.
	next := at(set["next_health_at"].(string))
	if !next.After(time.Now().Add(-time.Minute)) || next.Sub(next.Truncate(24*time.Hour)) != healthSlot(projectOne) {
		t.Fatal("slot", next, healthSlot(projectOne))
	}
	h.fail("health_set", "a", map[string]any{"linear_project": projectOne, "expected_revision": 0, "health_policy": map[string]any{"enabled": false}})
	h.enable(projectOne, 1, map[string]any{"enabled": false})
	// A team board has no Linear projects.
	h.call("taskboard_set_v2", "a", taskboardArgsV2(1))
	h.fail("health_set", "a", map[string]any{"linear_project": projectOne, "expected_revision": 2, "health_policy": map[string]any{"enabled": true}})
}

// HP-M3: slots are spread by hash and stable.
func TestHealthSlotsAreSpreadAndStable(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		id := strings.Replace("00000000-0000-4000-8000-0000000000XX", "XX", string(rune('a'+i))+"0", 1)
		s := healthSlot(id)
		if s != healthSlot(id) || s < 0 || s >= 24*time.Hour {
			t.Fatal(id, s)
		}
		seen[s] = true
	}
	if len(seen) < 18 {
		t.Fatal("slots bunch together:", len(seen))
	}
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		next := nextHealthAt(projectOne, now)
		if !next.After(now) || next.Sub(now) > 24*time.Hour {
			t.Fatal(now, next)
		}
		now = next
	}
}

// Hysteresis: a changed health is acted on only on its second run in a
// row; the coordinator is told, and nothing else.
func TestHealthHysteresisNotifiesTheCoordinator(t *testing.T) {
	h := newHealthHarness(t)
	h.enable(projectOne, 0, nil)
	if r := h.run(projectOne, blockedFacts(2)); r["health"] != "atRisk" || r["acted"] != "" || r["pending_health"] != "atRisk" {
		t.Fatal("first run acted:", r)
	}
	if h.notices() != 0 {
		t.Fatal("a single run notified")
	}
	if r := h.run(projectOne, blockedFacts(0)); r["acted"] != "" || r["pending_health"] != "onTrack" {
		t.Fatal("a flip-flop acted:", r)
	}
	if r := h.run(projectOne, blockedFacts(0)); r["acted"] != "changed" {
		t.Fatal("two runs did not act:", r)
	}
	if r := h.run(projectOne, blockedFacts(0)); r["acted"] != "" {
		t.Fatal("an unchanged health acted again (weekly is off):", r)
	}
	h.run(projectOne, blockedFacts(5))
	h.run(projectOne, blockedFacts(5))
	var body, address string
	if e := h.s.db.QueryRow(`SELECT body,address FROM messages WHERE kind='project-health' ORDER BY sequence DESC LIMIT 1`).Scan(&body, &address); e != nil {
		t.Fatal(e)
	}
	if h.notices() != 2 || address != "c" || !strings.Contains(body, "now Off track, was On track") || !strings.Contains(body, "FJA-1") || !strings.Contains(body, "Nothing was posted to Linear") || strings.Contains(body, "@") {
		t.Fatal(h.notices(), address, body)
	}
	row := h.call("health_get", "b", map[string]any{}).(map[string]any)["projects"].([]any)[0].(map[string]any)
	if row["last_health"] != "offTrack" || row["pending_health"] != "" || row["last_report"] == nil {
		t.Fatal(row)
	}
}

func TestHealthWeeklySummary(t *testing.T) {
	h := newHealthHarness(t)
	h.enable(projectOne, 0, map[string]any{"enabled": true, "weekly": true})
	h.run(projectOne, blockedFacts(0))
	h.run(projectOne, blockedFacts(0))
	if r := h.run(projectOne, blockedFacts(0)); r["acted"] != "" {
		t.Fatal("weekly before a week:", r)
	}
	if _, e := h.s.db.Exec(`UPDATE project_health SET last_acted_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		t.Fatal(e)
	}
	if r := h.run(projectOne, blockedFacts(0)); r["acted"] != "weekly" || h.notices() != 2 {
		t.Fatal(r, h.notices())
	}
}

// HP-M3: at most two runs a call, only bound and enabled projects, and
// none unless the hour's budget pays for them in full after the comment
// watch's reserve; an unused charge is refunded.
func TestHealthDueCapsAndBudget(t *testing.T) {
	h := newHealthHarness(t)
	h.enable(projectOne, 0, nil)
	h.enable(projectTwo, 0, nil)
	h.due()
	if _, e := h.sysErr("health_due", map[string]any{"limit": 3}); e == nil {
		t.Fatal("a limit over the cap was accepted")
	}
	if runs := h.sys("health_due", map[string]any{"limit": 1})["runs"].([]any); len(runs) != 1 {
		t.Fatal(runs)
	}
	before := h.sys("watch_charge", map[string]any{"calls": 0})["budget_used"].(int)
	if runs := h.sys("health_due", map[string]any{"limit": 2})["runs"].([]any); len(runs) != 2 {
		t.Fatal(runs)
	}
	after := h.sys("watch_charge", map[string]any{"calls": 0})["budget_used"].(int)
	if after-before != 2*HealthMaxCalls {
		t.Fatal("charge", before, after)
	}
	h.sys("health_report", map[string]any{"linear_project": projectOne, "calls": 4, "facts": blockedFacts(0)})
	if refunded := h.sys("watch_charge", map[string]any{"calls": 0})["budget_used"].(int); refunded != after-(HealthMaxCalls-4) {
		t.Fatal("refund", after, refunded)
	}
	// Budget short: the run waits, whole.
	h.due()
	if _, e := h.s.db.Exec(`UPDATE watch_lease SET budget_used=budget_limit-?`, HealthMaxCalls+watchConnectCalls); e != nil {
		t.Fatal(e)
	}
	if r := h.sys("health_due", map[string]any{"limit": 2}); len(r["runs"].([]any)) != 0 || r["deferred"] != true {
		t.Fatal("ran on a short budget:", r)
	}
	if _, e := h.s.db.Exec(`UPDATE watch_lease SET budget_used=0`); e != nil {
		t.Fatal(e)
	}
	// A project dropped from the binding is never run.
	h.call("taskboard_set_v2", "a", taskboardArgsV2(1, projectTwo))
	for _, r := range h.sys("health_due", map[string]any{"limit": 2})["runs"].([]any) {
		if r.(map[string]any)["linear_project"] == projectOne {
			t.Fatal("an unbound project ran")
		}
	}
}

func TestHealthReportRefusesUntypedFactsAndRetriesFailures(t *testing.T) {
	h := newHealthHarness(t)
	h.enable(projectOne, 0, nil)
	h.due()
	h.sys("health_due", map[string]any{"limit": 2})
	for name, f := range map[string]map[string]any{
		"title text":   func() map[string]any { f := blockedFacts(1); f["blocked"] = []any{"Fix the login page"}; return f }(),
		"extra field":  func() map[string]any { f := blockedFacts(0); f["title"] = "x"; return f }(),
		"inconsistent": func() map[string]any { f := blockedFacts(0); f["stalled_count"] = 9; return f }(),
		"no start":     func() map[string]any { f := blockedFacts(0); f["start_date"] = ""; return f }(),
	} {
		if _, e := h.sysErr("health_report", map[string]any{"linear_project": projectOne, "calls": 2, "facts": f}); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	r := h.sys("health_report", map[string]any{"linear_project": projectOne, "calls": 2, "error": "issues_unreadable"})
	row := h.call("health_get", "b", map[string]any{}).(map[string]any)["projects"].([]any)[0].(map[string]any)
	if r["error"] != "issues_unreadable" || row["last_error"] != "issues_unreadable" || at(row["next_health_at"].(string)).Sub(at(row["last_run_at"].(string))) != healthRetry {
		t.Fatal(r, row)
	}
}

// HP-L2: a client can never take a health: operation ID.
func TestHealthOperationIDsAreReserved(t *testing.T) {
	w := newWatchHarness(t)
	if _, err := w.reserve("a", "health:2026-10-26", map[string]any{}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatal(err)
	}
}
