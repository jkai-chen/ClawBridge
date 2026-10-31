package dispatch

import (
	"strings"
	"time"

	"clawbridge/internal/bus"
)

// Proposal is an engine's proposal for a case: an action, a reason code and
// the domain it concerns ("" for the fallback).
type Proposal struct {
	Action Action
	Reason string
	Domain string
}

// maxFactWaits is how many ticks a claim waits for an authoritative read
// before it is escalated as facts_unavailable.
const maxFactWaits = 3

// Trusted reports whether the session's ID, with its recorded principal, is
// on the domain's trust list.
func Trusted(s Session, d bus.DispatcherDomain) bool { return trusted(s, d) }

// LabelDomains are the domains one of the issue's labels matches: they route
// a notice or an escalation, never grant.
func LabelDomains(issue Issue, domains []bus.DispatcherDomain) []bus.DispatcherDomain {
	var out []bus.DispatcherDomain
	for _, d := range domains {
		hit := false
		for _, want := range d.Match.Labels {
			for _, l := range issue.Labels {
				if strings.EqualFold(want, l) {
					hit = true
				}
			}
		}
		if hit {
			out = append(out, d)
		}
	}
	return out
}

// NeedsRead reports whether the authoritative read is worth its Linear call.
// It is when the claim could be confirmed: the client's facts point at
// exactly one project domain that trusts the session, which is active. It is
// also when the client reported no project at all (no client does today) and
// some domain matches by project, since the app's read is the authority and
// may find one; whether the session is trusted is then decided on its facts.
// Label domains never grant, so with none matching by project a read buys
// nothing. A claim with no issue has nothing to read either.
func NeedsRead(c Case) bool {
	if c.Session.Status != sessionActive || c.Session.Presence == presenceStale {
		return false
	}
	if c.Issue.ProjectID == "" {
		// A paths-only claim names no issue to read.
		if c.Issue.ID == "" {
			return false
		}
		for _, d := range c.Domains {
			if len(d.Match.LinearProjects) > 0 {
				return true
			}
		}
		return false
	}
	ds := ProjectDomains(c.Issue, c.Domains)
	return len(ds) == 1 && trusted(c.Session, ds[0])
}

// Rules is the rules engine (live/DISPATCHER-DESIGN.md r4 §5): one proposal
// per pending claim, from the facts. readFailed says the authoritative read
// was needed and could not be made; waits counts this claim's earlier waits;
// age is how long it has been pending. Its proposal still goes through the
// guardrails (Decide): this table never grants on its own.
func Rules(c Case, readFailed bool, waits int, age time.Duration) Proposal {
	switch c.Session.Status {
	case sessionStatusRetire, sessionStatusMoved:
		return Proposal{Decline, "session_gone", ""}
	}
	domainOf := func() string {
		if ds := ProjectDomains(c.Issue, c.Domains); len(ds) == 1 {
			return ds[0].ID
		}
		if ls := LabelDomains(c.Issue, c.Domains); len(ls) == 1 {
			return ls[0].ID
		}
		return ""
	}
	if readFailed {
		if waits >= maxFactWaits {
			return Proposal{Escalate, "facts_unavailable", domainOf()}
		}
		return Proposal{Wait, "facts_unavailable", domainOf()}
	}
	if c.Session.Presence == presenceStale {
		// A sleeping laptop: wait a while, then a person decides (DP-L1).
		if age < staleAfter(c)/4 {
			return Proposal{Wait, "session_stale", domainOf()}
		}
		return Proposal{Escalate, "session_stale", domainOf()}
	}
	if c.Issue.Authoritative && !openStates[c.Issue.StateType] {
		return Proposal{Escalate, "issue_closed", domainOf()}
	}
	if c.Holders > 0 || c.NeedsOverlap {
		return Proposal{Escalate, "overlap", domainOf()}
	}
	projects := ProjectDomains(c.Issue, c.Domains)
	switch len(projects) {
	case 0:
		labels := LabelDomains(c.Issue, c.Domains)
		switch len(labels) {
		case 0:
			return Proposal{Escalate, "no_domain", ""}
		case 1:
			return Proposal{Escalate, "label_only", labels[0].ID}
		}
		return Proposal{Escalate, "ambiguous", ""}
	case 1:
	default:
		return Proposal{Escalate, "ambiguous", ""}
	}
	if !trusted(c.Session, projects[0]) {
		return Proposal{Escalate, "untrusted", projects[0].ID}
	}
	return Proposal{Confirm, "trusted", projects[0].ID}
}

// Run is one case through the rules engine and the guardrails: the action
// to take and its reason, with the engine's own proposal kept for the
// record.
func Run(c Case, readFailed bool, waits int, age time.Duration) (Proposal, Action, string) {
	p := Rules(c, readFailed, waits, age)
	action, why := Decide(c, p.Action)
	if why == "" {
		why = p.Reason
	}
	return p, action, why
}
