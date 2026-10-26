// Package dispatch holds the dispatcher's guardrails (live/DISPATCHER-
// DESIGN.md r4 §3.3): fixed predicates over a case's facts, and the only
// place a grant is allowed. Whatever engine proposes an action, rules or a
// model, the executor carries it out only if Check permits it; anything
// else becomes escalate(guardrail). No engine can widen them.
package dispatch

import (
	"time"

	"clawbridge/internal/bus"
)

// Action is what the dispatcher may do with a case.
type Action string

const (
	Confirm        Action = "confirm"
	Decline        Action = "decline"
	Wait           Action = "wait"
	Escalate       Action = "escalate"
	Route          Action = "route"
	ReleasePending Action = "release_pending"
	Release        Action = "release"
)

// Release limits (§7).
const (
	MaxReleasesPerDay   = 5
	minReleaseNotice    = 2 * time.Hour
	defaultStaleAfter   = 24 * time.Hour
	sessionActive       = "active"
	presenceStale       = "stale"
	sessionStatusMoved  = "moved"
	sessionStatusRetire = "retired"
)

// openStates are the only Linear state types a claim may be confirmed in:
// anything else (completed, canceled, duplicate, or a type Linear adds
// later) is closed.
var openStates = map[string]bool{"triage": true, "backlog": true, "unstarted": true, "started": true}

// Session is the proposing or holding session, from the bus.
type Session struct {
	ID, Principal string
	// Status is active, retired or moved; Presence is active or stale.
	Status, Presence string
	// AbsentFor is how long the session has been stale, retired or moved
	// (zero while it is active and fresh).
	AbsentFor time.Duration
}

// Issue is the issue's facts. Authoritative is set only when they come from
// the server's own read as the Linear app at decision time (DP-H1); facts a
// client reported may route, never grant.
type Issue struct {
	ID, ProjectID, StateType string
	Labels                   []string
	Authoritative            bool
}

// Case is everything a decision needs.
type Case struct {
	Session Session
	Issue   Issue
	Domains []bus.DispatcherDomain
	// Holders counts the issue's confirmed holders other than Session.
	Holders int
	// NeedsOverlap is set when the proposal asked for allow_overlap.
	NeedsOverlap bool
	// PolicyRevision is the domains policy's current revision; FactsRevision
	// the one the facts were gathered under.
	PolicyRevision, FactsRevision int64
	// Release: how long the holder has shown no activity on the issue, when
	// the release notice went out (zero if none), and today's releases.
	InactiveFor   time.Duration
	NoticeAt      time.Time
	ReleasesToday int
	Now           time.Time
}

// ProjectDomains are the domains whose Linear projects include the issue's
// project: only these can grant.
func ProjectDomains(issue Issue, domains []bus.DispatcherDomain) []bus.DispatcherDomain {
	var out []bus.DispatcherDomain
	if issue.ProjectID == "" {
		return nil
	}
	for _, d := range domains {
		for _, p := range d.Match.LinearProjects {
			if p == issue.ProjectID {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// trusted reports whether the session's ID is on the domain's trust list,
// with the principal recorded when it was trusted (DP-M1).
func trusted(s Session, d bus.DispatcherDomain) bool {
	for _, t := range d.TrustedSessions {
		if t.ID != "" && t.ID == s.ID && t.Principal == s.Principal {
			return true
		}
	}
	return false
}

// staleAfter is the domain's release window, or the default.
func staleAfter(c Case) time.Duration {
	if ds := ProjectDomains(c.Issue, c.Domains); len(ds) == 1 && ds[0].StaleAfterHours > 0 {
		return time.Duration(ds[0].StaleAfterHours) * time.Hour
	}
	return defaultStaleAfter
}

// abandoned is the release condition: the holder absent and inactive, both,
// for the whole window.
func abandoned(c Case) bool {
	gone := c.Session.Presence == presenceStale || c.Session.Status == sessionStatusRetire || c.Session.Status == sessionStatusMoved
	window := staleAfter(c)
	return gone && c.Session.AbsentFor >= window && c.InactiveFor >= window
}

// releaseDue is when a release may follow its notice.
func releaseDue(c Case) time.Time {
	wait := staleAfter(c) / 4
	if wait < minReleaseNotice {
		wait = minReleaseNotice
	}
	return c.NoticeAt.Add(wait)
}

// Check reports whether the guardrails permit action for the case, and if
// not, a reason code.
func Check(c Case, action Action) (bool, string) {
	switch action {
	case Escalate, Wait, Route:
		return true, ""
	case Confirm:
		switch {
		case !c.Issue.Authoritative:
			return false, "facts_not_authoritative"
		case c.PolicyRevision != c.FactsRevision:
			return false, "policy_changed"
		case c.Session.Status != sessionActive || c.Session.Presence == presenceStale:
			return false, "session_not_active"
		case !openStates[c.Issue.StateType]:
			return false, "issue_closed"
		case c.Holders > 0 || c.NeedsOverlap:
			return false, "overlap"
		}
		ds := ProjectDomains(c.Issue, c.Domains)
		if len(ds) != 1 {
			return false, "not_one_project_domain"
		}
		if !trusted(c.Session, ds[0]) {
			return false, "untrusted"
		}
		return true, ""
	case Decline:
		if c.Session.Status == sessionStatusRetire || c.Session.Status == sessionStatusMoved {
			return true, ""
		}
		return false, "session_not_gone"
	case ReleasePending:
		if abandoned(c) {
			return true, ""
		}
		return false, "not_abandoned"
	case Release:
		switch {
		case !abandoned(c):
			return false, "not_abandoned"
		case c.NoticeAt.IsZero() || c.Now.Before(releaseDue(c)):
			return false, "notice_period"
		case c.ReleasesToday >= MaxReleasesPerDay:
			return false, "release_cap"
		}
		return true, ""
	}
	return false, "unknown_action"
}

// Permitted is the set of actions the guardrails allow for the case: what
// a model engine may choose from.
func Permitted(c Case) []Action {
	var out []Action
	for _, a := range []Action{Confirm, Decline, Wait, Escalate, Route, ReleasePending, Release} {
		if ok, _ := Check(c, a); ok {
			out = append(out, a)
		}
	}
	return out
}

// Decide applies the guardrails to an engine's proposal: a permitted action
// stands; anything else becomes escalate, with the guardrail's reason.
func Decide(c Case, proposed Action) (Action, string) {
	if ok, reason := Check(c, proposed); !ok {
		return Escalate, "guardrail:" + reason
	}
	return proposed, ""
}
