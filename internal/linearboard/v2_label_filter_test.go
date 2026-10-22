package linearboard

import (
	"context"
	"net/http"
	"testing"

	"clawbridge/internal/linearprovider"
)

// teamFilteredCatalogV2 returns each team's provider-filtered label list, as
// the native list_issue_labels does: rows carry no team of their own.
type teamFilteredCatalogV2 struct {
	*providerV2
	byTeam map[string][]linearprovider.Label
}

func (p teamFilteredCatalogV2) Labels(_ context.Context, team string, _ linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	return linearprovider.Page[linearprovider.Label]{Items: p.byTeam[team], Complete: true}, nil
}

func TestLinearV2FilterScopedLabelsApplyOnlyFromTheIssueTeamList(t *testing.T) {
	const teamOnly = "88888888-8888-4888-8888-888888888888"
	const otherOnly = "99999999-9999-4999-8999-999999999999"
	const workspace = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const retired = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	filtered := func(id string) linearprovider.Label {
		return linearprovider.Label{ID: id, FilterScoped: true, EligibilityKnown: true}
	}
	gone := filtered(retired)
	gone.Archived = true
	s, _, p, scope := testV2(t)
	catalog := teamFilteredCatalogV2{providerV2: p, byTeam: map[string][]linearprovider.Label{
		teamID:      {filtered(teamOnly), filtered(workspace), gone},
		otherTeamID: {filtered(otherOnly), filtered(workspace)},
	}}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return catalog, nil }
	write := func(op string, labels ...string) error {
		_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: op, IssueID: issueID}, Labels: linearprovider.Field[[]string]{Present: true, Value: labels}, ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{}}})
		return e
	}
	for op, label := range map[string]string{"other-team": otherOnly, "retired": retired} {
		if e := write(op, label); e == nil || p.writes != 0 {
			t.Fatalf("%s label applied to an issue of a team whose list lacks it: %v", op, e)
		}
	}
	if e := write("applies", teamOnly, workspace); e != nil || p.writes != 1 {
		t.Fatalf("filter-proven labels refused: %v writes=%d", e, p.writes)
	}
	page, e := s.LabelsV2(context.Background(), scope, teamID, 50, "")
	if e != nil || len(page.Items) != 3 {
		t.Fatal("filter-scoped rows not listed", page, e)
	}
	// Without per-row scope, a label listed for several allowed teams spans
	// them; one listed for a single team selects only that team.
	for _, id := range []string{workspace, teamOnly, otherOnly} {
		if _, e = s.IssuesV2(context.Background(), scope, IssuesInputV2{Limit: 10, LabelID: id}); e != nil {
			t.Fatalf("label filter %s refused: %v", id, e)
		}
	}
	catalog.byTeam[teamID] = append(catalog.byTeam[teamID], linearprovider.Label{ID: workspace, TeamID: teamID, EligibilityKnown: true})
	if _, e = s.IssuesV2(context.Background(), scope, IssuesInputV2{Limit: 10, LabelID: workspace}); e == nil {
		t.Fatal("contradictory filter-scoped and team-scoped rows accepted")
	}
	contradictory := filtered(teamOnly)
	contradictory.Global = true
	if checkLabelV2(contradictory, teamID) == nil {
		t.Fatal("filter-scoped row with its own scope accepted")
	}
}

func TestLinearV2ContextReportsCapabilityState(t *testing.T) {
	s, _, p, scope := testV2(t)
	p.caps.LabelEligibilityByFilter = false
	p.caps.Reasons = map[string]string{"label_eligibility_by_filter": "list_issue_labels is not advertised"}
	got, e := s.ContextV2(context.Background(), scope)
	if e != nil || got.Capabilities == nil || got.Capabilities.LabelEligibilityByFilter || got.Capabilities.Reasons["label_eligibility_by_filter"] == "" || !got.Capabilities.ParentSameDestination || p.writes != 0 {
		t.Fatal("context omitted capability state", got.Capabilities, e)
	}
}
