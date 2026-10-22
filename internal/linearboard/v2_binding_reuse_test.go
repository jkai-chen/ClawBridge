package linearboard

import (
	"context"
	"net/http"
	"testing"

	"clawbridge/internal/linearprovider"
)

// countingBindingV2 counts the Linear reads of binding verification.
type countingBindingV2 struct {
	*providerV2
	teams, projects *int
}

func (p countingBindingV2) GetTeam(ctx context.Context, id string) (linearprovider.Team, error) {
	*p.teams++
	return p.providerV2.GetTeam(ctx, id)
}
func (p countingBindingV2) GetProject(ctx context.Context, id string) (linearprovider.Project, error) {
	*p.projects++
	return p.providerV2.GetProject(ctx, id)
}

func TestLinearV2WritesVerifyTheBindingOncePerCommand(t *testing.T) {
	for _, op := range []string{"assignee", "relation"} {
		t.Run(op, func(t *testing.T) {
			s, _, p, scope := testV2(t)
			teams, projects := 0, 0
			s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
				return countingBindingV2{providerV2: p, teams: &teams, projects: &projects}, nil
			}
			p.members = []linearprovider.Member{{ID: memberA, TeamID: teamID, Active: true, EligibilityKnown: true}}
			p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
			reads := p.reads
			var e error
			if op == "assignee" {
				_, e = s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "reuse-" + op, IssueID: issueID}, Assignee: linearprovider.Field[*string]{Present: true, Value: ptrV2(memberA)}})
			} else {
				_, e = s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "reuse-" + op, Action: "add", Type: "blocks", FromID: issueID, ToID: parentA})
			}
			// One verification of the 2-team, 2-project binding, not two.
			if e != nil || p.writes != 1 || teams != 2 || projects != 2 {
				t.Fatalf("binding read teams=%d projects=%d: %v", teams, projects, e)
			}
			// The issues themselves are still re-read inside the lock.
			want := 2
			if op == "relation" {
				want = 4
			}
			if p.reads-reads != want {
				t.Fatalf("issue reads = %d, want %d (preflight and in-lock)", p.reads-reads, want)
			}
		})
	}
}

func TestLinearV2BindingChangeIsStillRefusedInsideTheLock(t *testing.T) {
	s, a, p, scope := testV2(t)
	p.members = []linearprovider.Member{{ID: memberA, TeamID: teamID, Active: true, EligibilityKnown: true}}
	// The ClawBridge binding revision moves after preflight: the reused
	// verification must not let the write through.
	a.afterAccess = func(n int) {
		if n == 2 {
			a.b.Revision++
		}
	}
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "revision-moved", IssueID: issueID}, Assignee: linearprovider.Field[*string]{Present: true, Value: ptrV2(memberA)}}
	if _, e := s.WriteV2(context.Background(), scope, in); e == nil || p.writes != 0 {
		t.Fatal("binding change inside the lock dispatched", e)
	}
}
