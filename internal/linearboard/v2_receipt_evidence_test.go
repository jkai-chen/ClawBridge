package linearboard

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

func edgeFixtureV2(t *testing.T, visible bool) (*Service, *authorityV2, *providerV2, func(bool)) {
	t.Helper()
	s, a, p, _ := testV2(t)
	p.unconfirmedEdges = true
	p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true}
	show := func(on bool) {
		i := p.issues[issueID]
		i.Relations = nil
		if on {
			i.Relations = []linearprovider.Relation{{ID: labelA, Type: "blocks", FromID: issueID, ToID: parentA}}
		}
		p.issues[issueID] = i
	}
	show(visible)
	return s, a, p, show
}

// An edge write whose response does not name the edge is accepted only when
// a read shows it took effect; otherwise it is uncertain, never accepted.
func TestLinearV2UnnamedEdgeWritesAreConfirmedByReading(t *testing.T) {
	ctx := context.Background()
	_, _, _, scope := testV2(t)
	add := RelationInputV2{OperationID: "add", Action: "add", Type: "blocks", FromID: issueID, ToID: parentA}
	remove := RelationInputV2{OperationID: "remove", Action: "remove", Type: "blocks", FromID: issueID, ToID: parentA}

	s, _, p, show := edgeFixtureV2(t, false)
	p.after = func() { show(true) }
	if r, e := s.RelationV2(ctx, scope, add); e != nil || r.(ReceiptV2).Outcome != "accepted" || r.(ReceiptV2).ID != labelA {
		t.Fatal("visible added edge not confirmed", r, e)
	}
	s, a, _, _ := edgeFixtureV2(t, false)
	if _, e := s.RelationV2(ctx, scope, add); e == nil || !strings.Contains(e.Error(), "receipt_unverified") || ledgerStateV2(t, s, a, add.OperationID) != "pending" {
		t.Fatal("unconfirmed added edge was not left uncertain", e)
	}
	s, _, p, show = edgeFixtureV2(t, true)
	p.after = func() { show(false) }
	if r, e := s.RelationV2(ctx, scope, remove); e != nil || r.(ReceiptV2).Outcome != "accepted" {
		t.Fatal("removal of a visible edge not confirmed", r, e)
	}
	// Native relation lists are visible, not complete: an edge never seen
	// cannot be proven removed by its absence.
	s, a, p, _ = edgeFixtureV2(t, false)
	if _, e := s.RelationV2(ctx, scope, remove); e == nil || p.writes != 1 || ledgerStateV2(t, s, a, remove.OperationID) != "pending" {
		t.Fatal("removal of an unseen edge was accepted", e)
	}
	s, a, p, _ = edgeFixtureV2(t, true)
	if _, e := s.RelationV2(ctx, scope, remove); e == nil || ledgerStateV2(t, s, a, remove.OperationID) != "pending" {
		t.Fatal("an edge still visible after removal was accepted", e)
	}
}

func TestLinearV2IssueReceiptsCheckKnownFieldsAndNameUnknownOnes(t *testing.T) {
	ctx := context.Background()
	s, a, p, scope := testV2(t)
	p.members = []linearprovider.Member{{ID: memberA, TeamID: teamID, Active: true, EligibilityKnown: true}}
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "assign", IssueID: issueID}, Assignee: linearprovider.Field[*string]{Present: true, Value: ptrV2(memberA)}}
	r, e := s.WriteV2(ctx, scope, in)
	if e != nil || r.(ReceiptV2).Unverified != nil {
		t.Fatal("reported assignee not verified", r, e)
	}
	// Linear reports a different assignee: the write is uncertain.
	in.OperationID = "assign-other"
	p.after = func() {}
	wrong := &assigneeOverride{providerV2: p, assignee: parentA}
	s.cfg.Open = openV2(wrong)
	if _, e = s.WriteV2(ctx, scope, in); e == nil || !strings.Contains(e.Error(), "receipt_mismatch") || ledgerStateV2(t, s, a, in.OperationID) != "pending" {
		t.Fatal("contradicting receipt accepted", e)
	}
	// Linear does not report the assignee or labels: accepted, named unverified.
	silent := &assigneeOverride{providerV2: p, silent: true}
	s.cfg.Open = openV2(silent)
	in.OperationID = "assign-silent"
	labelledIssueV2(p)
	p.labels = []linearprovider.Label{{ID: labelA, TeamID: teamID, EligibilityKnown: true}}
	in.Labels = linearprovider.Field[[]string]{Present: true, Value: []string{labelA}}
	in.ExpectedLabels = linearprovider.Field[[]string]{Present: true, Value: []string{}}
	r, e = s.WriteV2(ctx, scope, in)
	if e != nil || !reflect.DeepEqual(r.(ReceiptV2).Unverified, []string{"labels", "assignee"}) {
		t.Fatal("unreported fields not named unverified", r, e)
	}
	raw, _ := json.Marshal(r)
	if !validReceiptV2(raw) {
		t.Fatal("an accepted receipt with unverified fields does not validate", string(raw))
	}
}

func TestLinearV2RecordedUnverifiedFieldsAreValidated(t *testing.T) {
	issue := func(outcome string, fields []string) []byte {
		raw, _ := json.Marshal(ReceiptV2{Kind: "issue", Outcome: outcome, ID: issueID, TeamID: teamID, Unverified: fields})
		return raw
	}
	comment, _ := json.Marshal(ReceiptV2{Kind: "comment", Outcome: "accepted", ID: issueID, IssueID: issueID, Unverified: []string{"labels"}})
	for name, c := range map[string]struct {
		raw []byte
		ok  bool
	}{
		"named fields":     {issue("accepted", []string{"labels", "state"}), true},
		"unknown field":    {issue("accepted", []string{"title"}), false},
		"duplicate field":  {issue("accepted", []string{"labels", "labels"}), false},
		"on unchanged":     {issue("observed_unchanged", []string{"labels"}), false},
		"on a comment":     {comment, false},
		"empty field list": {[]byte(`{"kind":"issue","outcome":"accepted","id":"` + issueID + `","team_id":"` + teamID + `","unverified":[]}`), false},
	} {
		if validReceiptV2(c.raw) != c.ok {
			t.Error(name, string(c.raw))
		}
	}
}

// assigneeOverride answers updates as Linear might: with another assignee,
// or without reporting labels and assignee at all.
type assigneeOverride struct {
	*providerV2
	assignee string
	silent   bool
}

func (p *assigneeOverride) UpdateV2(ctx context.Context, in linearprovider.UpdateInputV2) (linearprovider.IssueV2, error) {
	i, e := p.providerV2.UpdateV2(ctx, in)
	switch {
	case p.silent:
		i.LabelIDs, i.LabelsKnown, i.AssigneeID, i.AssigneeKnown = nil, false, "", false
	default:
		i.AssigneeID, i.AssigneeKnown = p.assignee, true
	}
	return i, e
}

func openV2(p Provider) func(context.Context, *http.Client, bool) (Provider, error) {
	return func(context.Context, *http.Client, bool) (Provider, error) { return p, nil }
}
