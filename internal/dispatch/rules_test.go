package dispatch

import (
	"testing"
	"time"
)

// Each row of the decision table, through the guardrails.
func TestRulesTable(t *testing.T) {
	for name, c := range map[string]struct {
		edit       func(*Case)
		readFailed bool
		waits      int
		age        time.Duration
		want       Action
		reason     string
		domain     string
	}{
		"trusted, one project domain": {func(c *Case) {}, false, 0, 0, Confirm, "trusted", "payments"},
		"retired session":             {func(c *Case) { c.Session.Status = "retired" }, false, 0, 0, Decline, "session_gone", ""},
		"stale, recent":               {func(c *Case) { c.Session.Presence = "stale" }, false, 0, time.Hour, Wait, "session_stale", "payments"},
		"stale, long":                 {func(c *Case) { c.Session.Presence = "stale" }, false, 0, 7 * time.Hour, Escalate, "session_stale", "payments"},
		"read failed":                 {func(c *Case) { c.Issue.Authoritative = false }, true, 0, 0, Wait, "facts_unavailable", "payments"},
		"read failed thrice":          {func(c *Case) { c.Issue.Authoritative = false }, true, 3, 0, Escalate, "facts_unavailable", "payments"},
		"closed issue":                {func(c *Case) { c.Issue.StateType = "completed" }, false, 0, 0, Escalate, "issue_closed", "payments"},
		"duplicate issue":             {func(c *Case) { c.Issue.StateType = "duplicate" }, false, 0, 0, Escalate, "issue_closed", "payments"},
		"held":                        {func(c *Case) { c.Holders = 1 }, false, 0, 0, Escalate, "overlap", "payments"},
		"untrusted":                   {func(c *Case) { c.Session.ID = "ses_other" }, false, 0, 0, Escalate, "untrusted", "payments"},
		"label only":                  {func(c *Case) { c.Issue.ProjectID = ""; c.Issue.Labels = []string{"Platform"} }, false, 0, 0, Escalate, "label_only", "platform"},
		"no domain":                   {func(c *Case) { c.Issue.ProjectID = "" }, false, 0, 0, Escalate, "no_domain", ""},
		"two label domains":           {func(c *Case) { c.Issue.ProjectID = ""; c.Issue.Labels = []string{"payments", "platform"} }, false, 0, 0, Escalate, "ambiguous", ""},
		// The client said one project, but the authoritative read was not
		// made: the guardrail refuses the grant.
		"client facts only": {func(c *Case) { c.Issue.Authoritative = false }, false, 0, 0, Escalate, "guardrail:facts_not_authoritative", "payments"},
	} {
		cs := confirmable()
		c.edit(&cs)
		p, action, why := Run(cs, c.readFailed, c.waits, c.age)
		if action != c.want || why != c.reason || p.Domain != c.domain {
			t.Fatalf("%s: %s %s %s, want %s %s %s", name, action, why, p.Domain, c.want, c.reason, c.domain)
		}
	}
}

// Silence never grants: however long a claim the dispatcher may not
// confirm stays pending, and however many times it waited, it is never
// confirmed.
func TestSilenceNeverGrants(t *testing.T) {
	for name, edit := range map[string]func(*Case){
		"untrusted":  func(c *Case) { c.Session.ID = "ses_other" },
		"label only": func(c *Case) { c.Issue.ProjectID = ""; c.Issue.Labels = []string{"payments"} },
		"held":       func(c *Case) { c.Holders = 1 },
		"stale":      func(c *Case) { c.Session.Presence = "stale" },
	} {
		for _, age := range []time.Duration{0, 24 * time.Hour, 30 * 24 * time.Hour} {
			for _, waits := range []int{0, 3, 100} {
				c := confirmable()
				edit(&c)
				// Neither the engine nor what the guardrails let through
				// may be a confirm.
				if p, action, _ := Run(c, false, waits, age); action == Confirm || p.Action == Confirm {
					t.Fatalf("%s confirmed after %s and %d waits", name, age, waits)
				}
			}
		}
	}
}

// The read is worth its call only for a claim that could be confirmed.
func TestNeedsRead(t *testing.T) {
	c := confirmable()
	c.Issue.Authoritative = false
	if !NeedsRead(c) {
		t.Fatal("a confirmable claim does not need the read")
	}
	for name, edit := range map[string]func(*Case){
		"untrusted": func(c *Case) { c.Session.ID = "ses_other" },
		"no domain": func(c *Case) { c.Issue.ProjectID = "" },
		"stale":     func(c *Case) { c.Session.Presence = "stale" },
	} {
		cs := c
		edit(&cs)
		if NeedsRead(cs) {
			t.Fatal(name, "spends a read")
		}
	}
}
