package dispatch

import (
	"testing"
	"time"

	"clawbridge/internal/bus"
)

const (
	projPay  = "11111111-1111-4111-8111-111111111111"
	projPlat = "22222222-2222-4222-8222-222222222222"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func domains() []bus.DispatcherDomain {
	return []bus.DispatcherDomain{
		{ID: "payments", Match: bus.DispatcherMatch{LinearProjects: []string{projPay}, Labels: []string{"payments"}}, StaleAfterHours: 24,
			TrustedSessions: []bus.DispatcherSessionRef{{ID: "ses_eng", Name: "eng", Principal: "person@example.com"}}},
		{ID: "platform", Match: bus.DispatcherMatch{LinearProjects: []string{projPlat}, Labels: []string{"platform"}}, StaleAfterHours: 8},
	}
}

// confirmable is a case every confirm guardrail passes.
func confirmable() Case {
	return Case{
		Session:        Session{ID: "ses_eng", Principal: "person@example.com", Status: "active", Presence: "active"},
		Issue:          Issue{ID: "i", ProjectID: projPay, StateType: "unstarted", Authoritative: true},
		Domains:        domains(),
		PolicyRevision: 3, FactsRevision: 3, Now: now,
	}
}

// Confirm needs every condition; breaking any one refuses it with its own
// reason.
func TestConfirmGuardrail(t *testing.T) {
	if ok, why := Check(confirmable(), Confirm); !ok {
		t.Fatal("the confirmable case was refused:", why)
	}
	for name, c := range map[string]struct {
		edit func(*Case)
		why  string
	}{
		"client facts":        {func(c *Case) { c.Issue.Authoritative = false }, "facts_not_authoritative"},
		"policy changed":      {func(c *Case) { c.PolicyRevision = 4 }, "policy_changed"},
		"stale session":       {func(c *Case) { c.Session.Presence = "stale" }, "session_not_active"},
		"retired session":     {func(c *Case) { c.Session.Status = "retired" }, "session_not_active"},
		"completed issue":     {func(c *Case) { c.Issue.StateType = "completed" }, "issue_closed"},
		"canceled issue":      {func(c *Case) { c.Issue.StateType = "canceled" }, "issue_closed"},
		"held":                {func(c *Case) { c.Holders = 1 }, "overlap"},
		"overlap asked":       {func(c *Case) { c.NeedsOverlap = true }, "overlap"},
		"label-only match":    {func(c *Case) { c.Issue.ProjectID = ""; c.Issue.Labels = []string{"payments"} }, "not_one_project_domain"},
		"unmatched project":   {func(c *Case) { c.Issue.ProjectID = "33333333-3333-4333-8333-333333333333" }, "not_one_project_domain"},
		"untrusted session":   {func(c *Case) { c.Session.ID = "ses_other" }, "untrusted"},
		"name reused":         {func(c *Case) { c.Session.Principal = "someone@example.com" }, "untrusted"},
		"trusted elsewhere":   {func(c *Case) { c.Issue.ProjectID = projPlat }, "untrusted"},
		"two project domains": {func(c *Case) { c.Domains[1].Match.LinearProjects = append(c.Domains[1].Match.LinearProjects, projPay) }, "not_one_project_domain"},
	} {
		cs := confirmable()
		c.edit(&cs)
		if ok, why := Check(cs, Confirm); ok || why != c.why {
			t.Fatalf("%s: ok=%v why=%s, want %s", name, ok, why, c.why)
		}
	}
}

// Decline only a session that is gone; a stale (sleeping) one is never
// declined (DP-L1).
func TestDeclineGuardrail(t *testing.T) {
	for status, presence := range map[string]string{"retired": "active", "moved": "stale"} {
		c := confirmable()
		c.Session.Status, c.Session.Presence = status, presence
		if ok, _ := Check(c, Decline); !ok {
			t.Fatal(status, "not declinable")
		}
	}
	c := confirmable()
	c.Session.Presence = "stale"
	if ok, _ := Check(c, Decline); ok {
		t.Fatal("a stale session was declined")
	}
}

// Release needs both signals for the domain's window, a notice at least
// max(2 h, window/4) old, and today's releases under the cap.
func TestReleaseGuardrails(t *testing.T) {
	abandonedCase := func() Case {
		c := confirmable()
		c.Session.Presence = "stale"
		c.Session.AbsentFor, c.InactiveFor = 25*time.Hour, 25*time.Hour
		return c
	}
	if ok, _ := Check(abandonedCase(), ReleasePending); !ok {
		t.Fatal("not release_pending")
	}
	for name, edit := range map[string]func(*Case){
		"present":         func(c *Case) { c.Session.Presence = "active"; c.Session.Status = "active" },
		"recently gone":   func(c *Case) { c.Session.AbsentFor = 23 * time.Hour },
		"recent activity": func(c *Case) { c.InactiveFor = 23 * time.Hour },
	} {
		c := abandonedCase()
		edit(&c)
		if ok, _ := Check(c, ReleasePending); ok {
			t.Fatal(name, "was release_pending")
		}
		if ok, _ := Check(c, Release); ok {
			t.Fatal(name, "was released")
		}
	}
	c := abandonedCase()
	if ok, why := Check(c, Release); ok || why != "notice_period" {
		t.Fatal("released with no notice", why)
	}
	// 24 h window: the notice period is max(2 h, 6 h) = 6 h.
	c.NoticeAt = now.Add(-5 * time.Hour)
	if ok, why := Check(c, Release); ok || why != "notice_period" {
		t.Fatal("released inside the notice period", why)
	}
	c.NoticeAt = now.Add(-6 * time.Hour)
	if ok, why := Check(c, Release); !ok {
		t.Fatal("not released after the notice period", why)
	}
	c.ReleasesToday = MaxReleasesPerDay
	if ok, why := Check(c, Release); ok || why != "release_cap" {
		t.Fatal("past the daily cap", why)
	}
	// An 8 h domain: the window is 8 h and the notice period the 2 h floor.
	c = abandonedCase()
	c.Issue.ProjectID = projPlat
	c.Session.AbsentFor, c.InactiveFor = 9*time.Hour, 9*time.Hour
	c.NoticeAt = now.Add(-2 * time.Hour)
	if ok, why := Check(c, Release); !ok {
		t.Fatal("the domain's window was not used", why)
	}
	// A 4 h domain: window/4 is 1 h, but the notice period never drops under
	// 2 h.
	c.Domains[1].StaleAfterHours = 4
	c.Session.AbsentFor, c.InactiveFor = 5*time.Hour, 5*time.Hour
	c.NoticeAt = now.Add(-90 * time.Minute)
	if ok, why := Check(c, Release); ok || why != "notice_period" {
		t.Fatal("released under the 2 h notice floor", why)
	}
}

// An engine's proposal outside the permitted set becomes escalate, with the
// guardrail's reason; the always-permitted actions stand.
func TestDecideAndPermitted(t *testing.T) {
	c := confirmable()
	c.Session.ID = "ses_other"
	if a, why := Decide(c, Confirm); a != Escalate || why != "guardrail:untrusted" {
		t.Fatal(a, why)
	}
	for _, a := range []Action{Wait, Escalate, Route} {
		if got, _ := Decide(c, a); got != a {
			t.Fatal(a, got)
		}
	}
	if got, _ := Decide(c, "approve_everything"); got != Escalate {
		t.Fatal("an unknown action stood:", got)
	}
	got := map[Action]bool{}
	for _, a := range Permitted(confirmable()) {
		got[a] = true
	}
	if !got[Confirm] || got[Decline] || got[Release] || !got[Escalate] {
		t.Fatal(got)
	}
}
