package linearboard

import (
	"context"
	"net/http"
	"testing"

	"clawbridge/internal/linearprovider"
)

type unfilteredCatalogV2 struct{ *providerV2 }

func (p unfilteredCatalogV2) Labels(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	return linearprovider.Page[linearprovider.Label]{Items: p.labels, Complete: true}, nil
}
func (p unfilteredCatalogV2) Members(context.Context, string, linearprovider.PageOptions) (linearprovider.Page[linearprovider.Member], error) {
	return linearprovider.Page[linearprovider.Member]{Items: p.members, Complete: true}, nil
}

func TestLinearV2MutationValidatesWholeCatalogPageBeforeSelectingProof(t *testing.T) {
	for _, kind := range []string{"label archived", "label unknown", "label foreign", "member inactive", "member unknown", "member foreign", "identical label", "identical member"} {
		t.Run(kind, func(t *testing.T) {
			s, _, p, scope := testV2(t)
			s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return unfilteredCatalogV2{p}, nil }
			in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "catalog-proof", IssueID: issueID}}
			label := linearprovider.Label{ID: labelA, TeamID: teamID, EligibilityKnown: true}
			member := linearprovider.Member{ID: memberA, TeamID: teamID, EligibilityKnown: true, Active: true}
			p.labels = []linearprovider.Label{label, label}
			p.members = []linearprovider.Member{member, member}
			switch kind {
			case "label archived":
				p.labels[1].Archived = true
			case "label unknown":
				p.labels[1].EligibilityKnown = false
			case "label foreign":
				p.labels[1].TeamID = otherTeamID
			case "member inactive":
				p.members[1].Active = false
			case "member unknown":
				p.members[1].EligibilityKnown = false
			case "member foreign":
				p.members[1].TeamID = otherTeamID
			}
			if kind == "identical member" || kind[:6] == "member" {
				in.Assignee = linearprovider.Field[*string]{Present: true, Value: ptrV2(memberA)}
			} else {
				in.Labels = linearprovider.Field[[]string]{Present: true, Value: []string{labelA}}
			}
			_, err := s.WriteV2(context.Background(), scope, in)
			positive := kind == "identical label" || kind == "identical member"
			if positive {
				if err != nil || p.writes != 1 {
					t.Fatalf("identical proof rejected: %v writes=%d", err, p.writes)
				}
			} else if err == nil || p.writes != 0 {
				t.Fatalf("contradictory proof authorized mutation: %v writes=%d", err, p.writes)
			}
		})
	}
}

func TestLinearV2UnknownProjectCannotExposeProjectSetIssue(t *testing.T) {
	s, _, p, scope := testV2(t)
	issue := p.issues[issueID]
	issue.ProjectKnown = false
	issue.Description = "synthetic private body"
	p.issues[issueID] = issue
	got, err := s.IssueV2(context.Background(), scope, issueID)
	if err == nil || got.ID != "" || got.Description != "" {
		t.Fatalf("unknown project exposed: %+v %v", got, err)
	}
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{issue}, Complete: true}}
	page, err := s.IssuesV2(context.Background(), scope, IssuesInputV2{Limit: 10})
	if err == nil || len(page.Items) != 0 {
		t.Fatalf("unknown page project exposed: %+v %v", page, err)
	}
}

func TestLinearV2WorkspaceLabelRowsDoNotAbortTeamLookup(t *testing.T) {
	const globalLabel = "88888888-8888-4888-8888-888888888888"
	const unscopedLabel = "99999999-9999-4999-8999-999999999999"
	s, _, p, scope := testV2(t)
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return unfilteredCatalogV2{p}, nil }
	p.labels = []linearprovider.Label{
		{ID: globalLabel, Global: true, EligibilityKnown: true},
		{ID: unscopedLabel},
		{ID: labelA, TeamID: teamID, EligibilityKnown: true},
	}
	page, err := s.LabelsV2(context.Background(), scope, teamID, 50, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != labelA {
		t.Fatalf("listing must return only verified team rows: %+v %v", page, err)
	}
	for n, id := range []string{globalLabel, unscopedLabel} {
		in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "refused-" + id, IssueID: issueID}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{id}}}
		if _, err = s.WriteV2(context.Background(), scope, in); err == nil || p.writes != 0 {
			t.Fatalf("requested out-of-team label %d applied: %v", n, err)
		}
	}
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "team-label", IssueID: issueID}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{labelA}}}
	if _, err = s.WriteV2(context.Background(), scope, in); err != nil || p.writes != 1 {
		t.Fatalf("workspace row aborted team label lookup: %v writes=%d", err, p.writes)
	}
	p.labels = append(p.labels, linearprovider.Label{ID: globalLabel, Global: true})
	if _, err = s.LabelsV2(context.Background(), scope, teamID, 50, ""); err == nil {
		t.Fatal("contradictory skipped row was not detected")
	}
}
