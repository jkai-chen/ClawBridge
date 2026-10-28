package linearboard

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// In these tests otherTeamID is a sub-team of teamID. The parent's labels
// are teamID's; a sub-team made from its parent's settings holds copies.
const (
	parentNeeds = "a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1"
	copyNeeds   = "b2b2b2b2-b2b2-4b2b-8b2b-b2b2b2b2b2b2"
	parentBug   = "c3c3c3c3-c3c3-4c3c-8c3c-c3c3c3c3c3c3"
	copyBug     = "d4d4d4d4-d4d4-4d4d-8d4d-d4d4d4d4d4d4"
	subIssue    = "e5e5e5e5-e5e5-4e5e-8e5e-e5e5e5e5e5e5"
)

func teamLabelRow(id, name, team string) linearprovider.Label {
	return linearprovider.Label{ID: id, Name: name, TeamID: team, EligibilityKnown: true}
}

func subTeamV2(t *testing.T) (*Service, *authorityV2, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	p.parents = map[string]string{otherTeamID: teamID}
	p.labels = []linearprovider.Label{teamLabelRow(parentNeeds, "needs-agent", teamID), teamLabelRow(parentBug, "bug", teamID)}
	p.issues[subIssue] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: subIssue, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, LabelsKnown: true, LabelIDs: []string{}}
	return s, a, p, scope
}

// A sub-team's set is its own label of the name, then its bound parent's;
// the team's own is the one applied.
func TestLabelSetSpansTheBoundParent(t *testing.T) {
	ctx := context.Background()
	s, a, p, scope := subTeamV2(t)
	set, e := s.LabelSetV2(ctx, scope, otherTeamID, "needs-agent")
	if e != nil || !reflect.DeepEqual(set.IDs, []string{parentNeeds}) || set.Apply != parentNeeds || set.Calls != 2 {
		t.Fatal("the parent's label alone:", set, e)
	}
	p.labels = append(p.labels, teamLabelRow(copyNeeds, "needs-agent", otherTeamID))
	if set, e = s.LabelSetV2(ctx, scope, otherTeamID, "needs-agent"); e != nil || !reflect.DeepEqual(set.IDs, []string{copyNeeds, parentNeeds}) || set.Apply != copyNeeds || !reflect.DeepEqual(set.Scopes, []string{"team", "parent"}) {
		t.Fatal("a copy and the parent's:", set, e)
	}
	// The parent's own set never reaches into its sub-team.
	if set, e = s.LabelSetV2(ctx, scope, teamID, "needs-agent"); e != nil || !reflect.DeepEqual(set.IDs, []string{parentNeeds}) {
		t.Fatal("the parent's set:", set, e)
	}
	// An ineligible copy is in the set but never applied.
	p.labels[len(p.labels)-1].EligibilityKnown = false
	if set, e = s.LabelSetV2(ctx, scope, otherTeamID, "needs-agent"); e != nil || set.Apply != parentNeeds || len(set.IDs) != 2 {
		t.Fatal("an ineligible copy applied:", set, e)
	}
	// Two labels of the name in one team: nothing is guessed.
	p.labels = append(p.labels, teamLabelRow(copyBug, "needs-agent", otherTeamID))
	if _, e = s.LabelSetV2(ctx, scope, otherTeamID, "needs-agent"); !errors.Is(e, ErrLabelAmbiguous) {
		t.Fatal("ambiguous set accepted:", e)
	}
	// Without a parent in Linear, or with the parent unbound, the set is
	// the team's own.
	p.labels = []linearprovider.Label{teamLabelRow(parentNeeds, "needs-agent", teamID), teamLabelRow(copyNeeds, "needs-agent", otherTeamID)}
	p.parents = nil
	if set, e = s.LabelSetV2(ctx, scope, otherTeamID, "needs-agent"); e != nil || !reflect.DeepEqual(set.IDs, []string{copyNeeds}) {
		t.Fatal("no parent:", set, e)
	}
	p.parents = map[string]string{otherTeamID: teamID}
	a.b.LinearTeamIDs, a.b.LinearDefaultTeamID, a.b.LinearProjectIDs = []string{otherTeamID}, otherTeamID, []string{projectB}
	if set, e = s.LabelSetV2(ctx, scope, otherTeamID, "needs-agent"); e != nil || !reflect.DeepEqual(set.IDs, []string{copyNeeds}) {
		t.Fatal("unbound parent:", set, e)
	}
}

// Linear lets a sub-team's issue carry its parent's label, so a write may
// apply it when the parent is bound; a third team's label never.
func TestLabelWriteAcceptsTheBoundParentsLabel(t *testing.T) {
	ctx := context.Background()
	s, a, p, scope := subTeamV2(t)
	write := func(op string, labels ...string) error {
		_, e := s.WriteV2(ctx, scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: op, IssueID: subIssue}, Labels: linearprovider.Field[[]string]{Present: true, Value: labels}, ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{}}})
		return e
	}
	if e := write("parent-label", parentBug); e != nil || p.writes != 1 {
		t.Fatal("the bound parent's label refused:", e, p.writes)
	}
	const elsewhere = "f6f6f6f6-f6f6-4f6f-8f6f-f6f6f6f6f6f6"
	p.labels = append(p.labels, teamLabelRow(elsewhere, "bug", "abababab-abab-4bab-8bab-abababababab"))
	if e := write("third-team", elsewhere); e == nil || p.writes != 1 {
		t.Fatal("a third team's label applied:", e)
	}
	p.parents = nil
	if e := write("no-parent", parentBug); e == nil || p.writes != 1 {
		t.Fatal("the parent's label applied with no parent:", e)
	}
	_ = a
}

// Intake's read of a sub-team accepts its bound parent's label; a label
// neither lists is ErrLabelNotVerified.
func TestTeamLabelledReadsTheParentsLabel(t *testing.T) {
	ctx := context.Background()
	s, _, p, scope := subTeamV2(t)
	in := TeamLabelledInputV2{TeamID: otherTeamID, LabelID: parentNeeds, UpdatedAfter: "2026-10-22T09:00:00Z", Limit: 50}
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{{Issue: linearprovider.Issue{ID: subIssue, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, LabelIDs: []string{parentNeeds}, LabelsKnown: true, UpdatedAt: "2026-10-22T10:00:00Z"}}, Complete: true}}
	page, e := s.TeamLabelledV2(ctx, scope, in)
	if e != nil || len(page.Items) != 1 || page.Items[0].ID != subIssue {
		t.Fatal(page, e)
	}
	if q := p.queries[len(p.queries)-1]; q.TeamID != otherTeamID || q.LabelID != parentNeeds {
		t.Fatal(q)
	}
	p.parents = nil
	if _, e = s.TeamLabelledV2(ctx, scope, in); !errors.Is(e, ErrLabelNotVerified) {
		t.Fatal("a label the team can't carry accepted:", e)
	}
}

// issues --label with the parent's label also reads a bound sub-team, but
// only while the sub-team holds no copy of the name.
func TestIssueLabelFilterReachesSubTeamsWithoutACopy(t *testing.T) {
	ctx := context.Background()
	s, _, p, scope := subTeamV2(t)
	teams := func() []string {
		var out []string
		cursor := ""
		for {
			p.queries = nil
			page, e := s.IssuesV2(ctx, scope, IssuesInputV2{Limit: 10, LabelID: parentBug, Cursor: cursor})
			if e != nil {
				t.Fatal(e)
			}
			for _, q := range p.queries {
				out = append(out, q.TeamID)
			}
			if page.Complete {
				return out
			}
			cursor = page.NextCursor
		}
	}
	if got := teams(); !reflect.DeepEqual(got, []string{teamID, otherTeamID}) {
		t.Fatal("no copy:", got)
	}
	p.labels = append(p.labels, teamLabelRow(copyBug, "bug", otherTeamID))
	if got := teams(); !reflect.DeepEqual(got, []string{teamID}) {
		t.Fatal("with a copy:", got)
	}
}

// A marker on a sub-team issue adds the team's own agent-working, else its
// bound parent's, and a close removes whichever of the set it carries.
func TestMarkerUsesTheSubTeamsLabelSet(t *testing.T) {
	ctx := context.Background()
	const parentMarker = "a7a7a7a7-a7a7-4a7a-8a7a-a7a7a7a7a7a7"
	const copyMarker = "b8b8b8b8-b8b8-4b8b-8b8b-b8b8b8b8b8b8"
	s, a, p, scope := markerTest(t)
	p.parents = map[string]string{otherTeamID: teamID}
	p.labels = []linearprovider.Label{teamLabelRow(parentMarker, bus.MarkerLabel, teamID)}
	p.issues[subIssue] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: subIssue, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, LabelsKnown: true, LabelIDs: []string{}}
	a.comments.gos = map[string]map[string]any{"comment": {"action": "none"}, "label": {"action": "add", "issue_uuid": subIssue, "label": bus.MarkerLabel}}
	if out := s.ApplyMarker(ctx, scope, "asg_1", "claim", nil); out.Label != "done" || len(p.updated) != 1 || !reflect.DeepEqual(p.updated[0].AddLabels, []string{parentMarker}) {
		t.Fatal("the parent's label not added:", out, p.updated)
	}
	p.labels = append(p.labels, teamLabelRow(copyMarker, bus.MarkerLabel, otherTeamID))
	p.updated = nil
	if out := s.ApplyMarker(ctx, scope, "asg_1", "claim", nil); out.Label != "done" || len(p.updated) != 1 || !reflect.DeepEqual(p.updated[0].AddLabels, []string{copyMarker}) {
		t.Fatal("the team's own copy not preferred:", out, p.updated)
	}
	issue := p.issues[subIssue]
	issue.LabelIDs = []string{parentMarker}
	p.issues[subIssue] = issue
	p.updated = nil
	a.comments.gos["label"] = map[string]any{"action": "remove", "issue_uuid": subIssue}
	if out := s.ApplyMarker(ctx, scope, "asg_1", "close", nil); out.Label != "done" || len(p.updated) != 1 || !reflect.DeepEqual(p.updated[0].RemoveLabels, []string{parentMarker}) {
		t.Fatal("the parent's label not removed from the sub-team issue:", out, p.updated)
	}
}

// A workspace label is listed for the team and its parent alike, under one
// ID: it is in the set once. Archived labels and groups are never in it.
func TestLabelSetCountsAWorkspaceLabelOnce(t *testing.T) {
	const workspace = "c9c9c9c9-c9c9-4c9c-8c9c-c9c9c9c9c9c9"
	s, _, p, scope := subTeamV2(t)
	shared := linearprovider.Label{ID: workspace, Name: "bug", Global: true, EligibilityKnown: true}
	archived := teamLabelRow(copyBug, "bug", otherTeamID)
	archived.Archived = true
	group := teamLabelRow(parentBug, "bug", teamID)
	group.Group = true
	catalog := teamFilteredCatalogV2{providerV2: p, byTeam: map[string][]linearprovider.Label{
		teamID:      {shared, group},
		otherTeamID: {shared, archived},
	}}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return catalog, nil }
	set, e := s.LabelSetV2(context.Background(), scope, otherTeamID, "bug")
	if e != nil || !reflect.DeepEqual(set.IDs, []string{workspace}) || set.Apply != workspace || !reflect.DeepEqual(set.Scopes, []string{"workspace"}) {
		t.Fatal(set, e)
	}
}

// teamCatalogV2 is the native label-ID proof with a catalog per team.
type teamCatalogV2 struct {
	*providerV2
	byTeam map[string][]linearprovider.LabelName
}

func (p teamCatalogV2) LabelCatalog(_ context.Context, team string, _ linearprovider.PageOptions) (linearprovider.Page[linearprovider.LabelName], error) {
	return linearprovider.Page[linearprovider.LabelName]{Items: p.byTeam[team], Complete: true}, nil
}
func (p teamCatalogV2) IssueHasLabel(context.Context, string, string, string, string) (bool, bool, error) {
	return false, false, nil
}

// A sub-team row that names its labels only is proven against the catalog
// of the team holding the label: the parent's.
func TestNamesOnlyRowsAreProvenAgainstTheParentsCatalog(t *testing.T) {
	ctx := context.Background()
	s, _, p, scope := subTeamV2(t)
	p.caps.LabelMembershipProof = true
	catalog := teamCatalogV2{providerV2: p, byTeam: map[string][]linearprovider.LabelName{
		teamID: {{ID: parentNeeds, Name: "needs-agent"}, {ID: parentBug, Name: "bug"}},
	}}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return catalog, nil }
	row := func(name string) linearprovider.IssueV2 {
		return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: subIssue, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, LabelNames: []string{name}, UpdatedAt: "2026-10-22T10:00:00Z"}
	}
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{row("needs-agent")}, Complete: true}}
	page, e := s.TeamLabelledV2(ctx, scope, TeamLabelledInputV2{TeamID: otherTeamID, LabelID: parentNeeds, UpdatedAfter: "2026-10-22T09:00:00Z", Limit: 50})
	if e != nil || len(page.Items) != 1 {
		t.Fatal("intake's read:", page, e)
	}
	// issues --label: the parent's pair first (empty), then the sub-team's.
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Complete: true}, {Items: []linearprovider.IssueV2{row("bug")}, Complete: true}}
	first, e := s.IssuesV2(ctx, scope, IssuesInputV2{Limit: 10, LabelID: parentBug})
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.IssuesV2(ctx, scope, IssuesInputV2{Limit: 10, LabelID: parentBug, Cursor: first.NextCursor})
	if e != nil || len(got.Items) != 1 {
		t.Fatal("issues --label on the sub-team:", got, e)
	}
}
