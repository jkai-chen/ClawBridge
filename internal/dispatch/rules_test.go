package dispatch

import (
	"testing"
	"time"

	"clawbridge/internal/bus"
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
		"stale, untrusted":            {func(c *Case) { c.Session.Presence = "stale"; c.Session.ID = "ses_other" }, false, 0, time.Hour, Escalate, "untrusted", "payments"},
		"stale, closed":               {func(c *Case) { c.Session.Presence = "stale"; c.Issue.StateType = "completed" }, false, 0, time.Hour, Wait, "session_stale", "payments"},
		"stale, untrusted, no domain": {func(c *Case) {
			c.Session.Presence = "stale"
			c.Session.ID = "ses_other"
			c.Issue.ProjectID = ""
		}, false, 0, time.Hour, Escalate, "no_domain", ""},
		"stale, untrusted, client": {func(c *Case) { c.Session.Presence = "stale"; c.Session.ID = "ses_other"; c.Issue.Authoritative = false }, false, 0, time.Hour, Wait, "session_stale", "payments"},
		"stale, untrusted, closed": {func(c *Case) {
			c.Session.Presence = "stale"
			c.Session.ID = "ses_other"
			c.Issue.StateType = "completed"
		}, false, 0, time.Hour, Wait, "session_stale", "payments"},
		"read failed":        {func(c *Case) { c.Issue.Authoritative = false }, true, 0, 0, Wait, "facts_unavailable", "payments"},
		"read failed thrice": {func(c *Case) { c.Issue.Authoritative = false }, true, 3, 0, Escalate, "facts_unavailable", "payments"},
		"closed issue":       {func(c *Case) { c.Issue.StateType = "completed" }, false, 0, 0, Escalate, "issue_closed", "payments"},
		"duplicate issue":    {func(c *Case) { c.Issue.StateType = "duplicate" }, false, 0, 0, Escalate, "issue_closed", "payments"},
		"held":               {func(c *Case) { c.Holders = 1 }, false, 0, 0, Escalate, "overlap", "payments"},
		"untrusted":          {func(c *Case) { c.Session.ID = "ses_other" }, false, 0, 0, Escalate, "untrusted", "payments"},
		"label only":         {func(c *Case) { c.Issue.ProjectID = ""; c.Issue.Labels = []string{"Platform"} }, false, 0, 0, Escalate, "label_only", "platform"},
		"no domain":          {func(c *Case) { c.Issue.ProjectID = "" }, false, 0, 0, Escalate, "no_domain", ""},
		"two label domains":  {func(c *Case) { c.Issue.ProjectID = ""; c.Issue.Labels = []string{"payments", "platform"} }, false, 0, 0, Escalate, "ambiguous", ""},
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
	// A stale session that no project domain trusts can never be confirmed;
	// the read only routes its escalation.
	stale := c
	stale.Session.Presence = "stale"
	stale.Session.ID = "ses_other"
	stale.Issue.ProjectID = ""
	if !NeedsRead(stale) {
		t.Fatal("a stale untrusted claim does not read")
	}
	stale.Session.Status = "retired"
	if NeedsRead(stale) {
		t.Fatal("a retired session spends a read")
	}
	for name, edit := range map[string]func(*Case){
		"untrusted": func(c *Case) { c.Session.ID = "ses_other" },
		"stale":     func(c *Case) { c.Session.Presence = "stale" },
		"retired":   func(c *Case) { c.Session.Status = "retired" },
		"elsewhere": func(c *Case) { c.Issue.ProjectID = projPlat },
	} {
		cs := c
		edit(&cs)
		if NeedsRead(cs) {
			t.Fatal(name, "spends a read")
		}
	}
}

// A claim whose client reported no project is read when some domain matches
// by project: the app's read is the authority, and it may find one.
func TestNeedsReadWithoutAClientProject(t *testing.T) {
	c := confirmable()
	c.Issue.ProjectID, c.Issue.Authoritative = "", false
	if !NeedsRead(c) {
		t.Fatal("an empty project with a project domain does not read")
	}
	// Trust is judged on the facts the read returns, not before it.
	untrusted := c
	untrusted.Session.ID = "ses_other"
	if !NeedsRead(untrusted) {
		t.Fatal("an untrusted session's empty project does not read")
	}
	labelsOnly := c
	labelsOnly.Domains = []bus.DispatcherDomain{{ID: "labels", Match: bus.DispatcherMatch{Labels: []string{"payments"}}}}
	if NeedsRead(labelsOnly) {
		t.Fatal("label domains cannot grant, so they spend no read")
	}
	for name, edit := range map[string]func(*Case){
		"stale":      func(c *Case) { c.Session.Presence = "stale" },
		"retired":    func(c *Case) { c.Session.Status = "retired" },
		"moved":      func(c *Case) { c.Session.Status = "moved" },
		"no issue":   func(c *Case) { c.Issue.ID = "" },
		"no domains": func(c *Case) { c.Domains = nil },
	} {
		cs := c
		edit(&cs)
		if NeedsRead(cs) {
			t.Fatal(name, "spends a read")
		}
	}
}
