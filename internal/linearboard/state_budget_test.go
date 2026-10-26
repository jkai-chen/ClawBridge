package linearboard

import (
	"context"
	"testing"

	"clawbridge/internal/bus"
)

// measureRow applies one claim marker row with the given parts as a server
// watcher does (the read binding proof cached, an authoritative state read
// when hosted) and counts what it spent: every call that would be a Linear
// tool call, plus each authoritative state read.
func measureRow(t *testing.T, parts []string, hosted bool) (spent, reserved int) {
	t.Helper()
	s, a, p, scope := stateTest(t, "claim", sfBacklog, sfPolicy(), false)
	gos := map[string]map[string]any{
		"comment": {"action": "post", "issue_uuid": issueID, "body": claimBody},
		"label":   {"action": "add", "issue_uuid": issueID, "label": bus.MarkerLabel},
		"state":   a.comments.gos["state"],
	}
	a.comments.gos = map[string]map[string]any{}
	for _, k := range parts {
		a.comments.gos[k] = gos[k]
	}
	s.CacheReadBindingProof(BindingProofTTL)
	p.stateByName = true
	reads := 0
	if hosted {
		s.cfg.IssueState = func(_ context.Context, issue string) (string, string, error) {
			reads++
			state := p.issues[issue].StateID
			if n := len(p.updated); n > 0 && p.updated[n-1].StateID != nil {
				state = *p.updated[n-1].StateID
			}
			return state, teamID, nil
		}
	}
	// The cycle's read proof is already cached when its rows apply.
	if _, e := s.BindingV2(context.Background(), scope); e != nil {
		t.Fatal(e)
	}
	before := p.tools
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	reserved = bus.MarkerRowCalls(len(a.b.LinearTeamIDs), len(a.b.LinearProjectIDs))
	for _, k := range parts {
		if got := settled(a, k); got["outcome"] != "done" {
			t.Fatal(parts, k, "did not apply:", got)
		}
		if k == "state" {
			reserved += bus.MarkerStateCalls
		}
	}
	return p.tools - before + reads, reserved
}

// The bus charges each marker row MarkerRowCalls up front (the row's
// binding proof included), and MarkerStateCalls more for a live state
// part; markers are never charged again after. What a row spends must fit.
func TestMarkerRowCostFitsItsReservation(t *testing.T) {
	for _, c := range []struct {
		name   string
		parts  []string
		hosted bool
	}{
		{"note and label", []string{"comment", "label"}, true},
		{"state only, hosted", []string{"state"}, true},
		{"state only, client", []string{"state"}, false},
		{"all three, hosted", []string{"comment", "label", "state"}, true},
		{"all three, client", []string{"comment", "label", "state"}, false},
	} {
		spent, reserved := measureRow(t, c.parts, c.hosted)
		if spent > reserved {
			t.Errorf("%s spends %d Linear calls; the bus reserves %d", c.name, spent, reserved)
		}
	}
}
