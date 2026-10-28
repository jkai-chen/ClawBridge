package linearboard

import (
	"context"
	"net/http"
	"testing"
	"time"

	"clawbridge/internal/linearprovider"
)

// The re-checks don't wait in tests.
func init() { deltaRecheckDelays = []time.Duration{0, 0} }

// lagging answers a label check with the old membership for its first
// stale checks, as Linear's list does right after a write.
type lagging struct {
	membershipV2
	stale *int
	old   map[string]bool
}

func (l lagging) IssueHasLabel(ctx context.Context, issue, team, project, label string) (bool, bool, error) {
	if _, lags := l.old[label]; lags && *l.stale > 0 {
		*l.stale--
		*l.membershipV2.checks++
		return l.old[label], true, nil
	}
	return l.membershipV2.IssueHasLabel(ctx, issue, team, project, label)
}

// Seen live: a label added seconds after the issue's create reads as absent
// from Linear's list for a moment. A re-check that finds it confirms the
// add; a contradiction that holds through every re-check is still a
// receipt_mismatch.
func TestLinearV2DeltaReadBackWaitsOutTheListsLag(t *testing.T) {
	ctx := context.Background()
	catalog := []linearprovider.LabelName{{ID: labelA, Name: "needs-agent"}}
	for name, c := range map[string]struct {
		stale  int
		checks int
		ok     bool
	}{
		"in step":          {0, 2, true},
		"one stale":        {1, 3, true},
		"two stale":        {2, 4, true},
		"stale throughout": {3, 3, false},
	} {
		t.Run(name, func(t *testing.T) {
			s, p, m, _ := resolvingV2(t, []string{"needs-agent"}, catalog, map[string]bool{labelA: true})
			_, a, _, scope := testV2(t)
			p.caps.LabelDeltas = true
			p.labels = []linearprovider.Label{{ID: labelA, TeamID: teamID, EligibilityKnown: true}, {ID: memberA, TeamID: teamID, EligibilityKnown: true}}
			stale := 0
			l := lagging{membershipV2: *m, stale: &stale, old: map[string]bool{memberA: false}}
			s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return l, nil }
			// The add of memberA lags; the removal of labelA reads in step.
			p.after = func() { m.member[labelA], m.member[memberA] = false, true; stale = c.stale; *m.checks = 0 }
			in := deltaV2("lag", []string{memberA}, []string{labelA})
			r, e := s.WriteV2(ctx, scope, in)
			// A lasting contradiction stops the checks: the removal may go
			// unchecked, so it counts the add's three at least.
			if *m.checks != c.checks && (c.ok || *m.checks < c.checks) {
				t.Fatal("checks", *m.checks, "want", c.checks)
			}
			if c.ok && (e != nil || r.(ReceiptV2).Unverified != nil) {
				t.Fatal("a lagging list was not waited out:", r, e)
			}
			if !c.ok && (e == nil || ledgerStateV2(t, s, a, in.OperationID) != "pending") {
				t.Fatal("a lasting contradiction was accepted:", r, e)
			}
		})
	}
}
