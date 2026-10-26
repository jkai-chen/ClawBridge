package linearboard

import (
	"context"
	"testing"

	"clawbridge/internal/bus"
)

// A state move as a hosted server applies it, measured: every Linear tool
// call and every authoritative state read, on a board whose binding proof
// is cached as the watcher's is.
func measureStateMove(t *testing.T, kind, current, phase string, hosted bool) int {
	t.Helper()
	s, a, p, scope := stateTest(t, kind, current, sfPolicy(), false)
	a.comments.gos = map[string]map[string]any{"state": a.comments.gos["state"]}
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
	// Warm the binding proof, as the row's other parts already did.
	if _, e := s.BindingV2(context.Background(), scope); e != nil {
		t.Fatal(e)
	}
	before := p.tools
	s.ApplyMarker(context.Background(), scope, "asg_1", phase, nil)
	if got := settled(a, "state"); got["outcome"] != "done" || len(stateMoves(p)) != 1 {
		t.Fatal("the move did not run:", got)
	}
	return p.tools - before + reads
}

// The bus charges a state part MarkerStateCalls up front, and markers are
// never charged again after: what a move spends, hosted or not, must fit.
func TestStateMoveCostFitsItsReservation(t *testing.T) {
	_, a, _, _ := stateTest(t, "claim", sfBacklog, sfPolicy(), false)
	reserved := bus.MarkerStateCalls(len(a.b.LinearTeamIDs), len(a.b.LinearProjectIDs))
	for name, spent := range map[string]int{
		"hosted claim":   measureStateMove(t, "claim", sfBacklog, "claim", true),
		"hosted release": measureStateMove(t, "release_done", sfProgress, "close", true),
		"client claim":   measureStateMove(t, "claim", sfBacklog, "claim", false),
	} {
		if spent > reserved {
			t.Errorf("%s spends %d Linear calls; the bus reserves %d", name, spent, reserved)
		}
	}
}
