package linearboard

import (
	"context"
	"net/http"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// readCounts is what one board command costs on the fake: Linear reads by
// kind, and capability computations (each re-derives every tool schema).
type readCounts struct{ labelPages, statuses, teams, projects, issues, capabilities int }

type countingV2 struct {
	*providerV2
	n *readCounts
}

func (p countingV2) Labels(ctx context.Context, team string, o linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	p.n.labelPages++
	return p.providerV2.Labels(ctx, team, o)
}
func (p countingV2) Statuses(ctx context.Context, team string) ([]linearprovider.Status, error) {
	p.n.statuses++
	return p.providerV2.Statuses(ctx, team)
}
func (p countingV2) GetTeam(ctx context.Context, id string) (linearprovider.Team, error) {
	p.n.teams++
	return p.providerV2.GetTeam(ctx, id)
}
func (p countingV2) GetProject(ctx context.Context, id string) (linearprovider.Project, error) {
	p.n.projects++
	return p.providerV2.GetProject(ctx, id)
}
func (p countingV2) IssueV2(ctx context.Context, id string) (linearprovider.IssueV2, error) {
	p.n.issues++
	return p.providerV2.IssueV2(ctx, id)
}
func (p countingV2) Capabilities(ctx context.Context) (linearprovider.Capabilities, error) {
	p.n.capabilities++
	return p.providerV2.Capabilities(ctx)
}

// countStateID is a workflow state of the default team on the fake.
const countStateID = "99999999-9999-4999-8999-999999999999"

// The reads each write command costs on the fake (2 teams, 2 projects in
// the binding). The numbers are the evidence for the efficiency work: one
// label walk per phase for any number of labels, one binding verification per
// command, and one capability computation per relation command. A create
// reads its team's labels and states once, before the ledger write; an
// update and a relation add still read their issues again inside it.
func TestLinearV2WriteCommandReadCounts(t *testing.T) {
	for name, c := range map[string]struct {
		run  func(*Service, *providerV2, bus.Request) error
		want readCounts
	}{
		"add two labels": {func(s *Service, p *providerV2, scope bus.Request) error {
			p.caps.LabelDeltas = true
			p.labels = []linearprovider.Label{{ID: labelA, TeamID: teamID, EligibilityKnown: true}, {ID: memberA, TeamID: teamID, EligibilityKnown: true}}
			labelledIssueV2(p)
			_, e := s.WriteV2(context.Background(), scope, deltaV2("count-labels", []string{labelA, memberA}, nil))
			return e
		}, readCounts{labelPages: 2, teams: 2, projects: 2, issues: 2, capabilities: 1}},
		"create with a state and labels": {func(s *Service, p *providerV2, scope bus.Request) error {
			p.labels = []linearprovider.Label{{ID: labelA, TeamID: teamID, EligibilityKnown: true}}
			p.statuses = []linearprovider.Status{{ID: countStateID, TeamID: teamID, Name: "Todo"}}
			title := "count create"
			in := WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: "count-create", Title: &title, StateID: ptrV2(countStateID)}, Project: linearprovider.Field[*string]{Present: true, Value: ptrV2(projectA)}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{labelA}}}
			_, e := s.WriteV2(context.Background(), scope, in)
			return e
		}, readCounts{labelPages: 1, statuses: 1, teams: 2, projects: 2, capabilities: 1}},
		"update": {func(s *Service, p *providerV2, scope bus.Request) error {
			_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "count-update", IssueID: issueID, Title: ptrV2("count update")}})
			return e
		}, readCounts{teams: 2, projects: 2, issues: 2, capabilities: 1}},
		"update with a state": {func(s *Service, p *providerV2, scope bus.Request) error {
			p.statuses = []linearprovider.Status{{ID: countStateID, TeamID: teamID, Name: "Todo"}}
			_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "count-update-state", IssueID: issueID, StateID: ptrV2(countStateID)}})
			return e
		}, readCounts{statuses: 2, teams: 2, projects: 2, issues: 2, capabilities: 1}},
		"relation add read back": {func(s *Service, p *providerV2, scope bus.Request) error {
			p.unconfirmedEdges = true
			p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
			// The response does not name the edge; the read-back finds it.
			p.after = func() {
				i := p.issues[issueID]
				i.Relations = []linearprovider.Relation{{ID: labelA, Type: "blocks", FromID: issueID, ToID: parentA}}
				p.issues[issueID] = i
			}
			_, e := s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "count-relation", Action: "add", Type: "blocks", FromID: issueID, ToID: parentA})
			return e
		}, readCounts{teams: 2, projects: 2, issues: 6, capabilities: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, p, scope := testV2(t)
			var n readCounts
			s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return countingV2{p, &n}, nil }
			if e := c.run(s, p, scope); e != nil || p.writes != 1 {
				t.Fatal(e, p.writes)
			}
			t.Logf("%s: %+v", name, n)
			if n != c.want {
				t.Fatalf("reads %+v, want %+v", n, c.want)
			}
		})
	}
}
