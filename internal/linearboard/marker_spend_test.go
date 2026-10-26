package linearboard

import (
	"context"
	"net/http"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// toolCounting reports the fake's tool calls as Sent, as the native
// provider does.
type toolCounting struct{ commentingV2 }

func (c toolCounting) Sent() uint64 { return uint64(c.providerV2.tools) }

// markerCycle runs one watch cycle that applies one due claim row, charged
// 99, over a provider that counts (or not), and returns what was reported.
func markerCycle(t *testing.T, counting, state bool) (*authorityV2, *providerV2) {
	t.Helper()
	var s *Service
	var a *authorityV2
	var p *providerV2
	var scope bus.Request
	if state {
		s, a, p, scope = stateTest(t, "claim", sfBacklog, sfPolicy(), false)
		p.stateByName = true
		s.cfg.IssueState = func(_ context.Context, issue string) (string, string, error) {
			state := p.issues[issue].StateID
			if n := len(p.updated); n > 0 && p.updated[n-1].StateID != nil {
				state = *p.updated[n-1].StateID
			}
			return state, teamID, nil
		}
	} else {
		s, a, p, scope = markerTest(t)
	}
	read, since, deadlines := []string{}, []string{}, []time.Time{}
	result := linearprovider.Comments{Complete: true}
	c := commentingV2{providerV2: p, read: &read, since: &since, deadlines: &deadlines, result: &result}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
		if counting {
			return toolCounting{c}, nil
		}
		return c, nil
	}
	a.comments.due = []any{}
	a.comments.markersDue = []any{map[string]any{"assignment_id": "asg_1", "phase": "claim", "issue_uuid": issueID, "charged": 99}}
	if err := s.Watch(context.Background(), scope, WatchOptions{Budget: 50, Once: true}); err != nil {
		t.Fatal(err)
	}
	return a, p
}

// An applied row reports what it spent, so the bus gives back the unused
// part of its charge: every tool call (its first connection paid by the
// cycle's up-front charge), plus the hosted app's state reads.
func TestMarkerRowReportsWhatItSpent(t *testing.T) {
	a, p := markerCycle(t, true, false)
	if len(a.comments.markerSpents) != 1 {
		t.Fatal("no report", a.comments.markerSpents)
	}
	r := a.comments.markerSpents[0]
	if r["assignment_id"] != "asg_1" || r["phase"] != "claim" || r["charged"] != 99 || r["calls"] != p.tools || p.tools == 0 {
		t.Fatal(r, p.tools)
	}
	// A hosted state move adds its two GraphQL state reads.
	a, p = markerCycle(t, true, true)
	if r = a.comments.markerSpents[0]; r["calls"] != p.tools+2 {
		t.Fatal("state reads not counted:", r["calls"], p.tools)
	}
	// A provider that cannot count reports nothing: the charge stands.
	if a, _ = markerCycle(t, false, false); len(a.comments.markerSpents) != 0 {
		t.Fatal("an uncounted row was reported", a.comments.markerSpents)
	}
}
