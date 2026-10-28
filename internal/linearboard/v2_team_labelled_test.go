package linearboard

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

const (
	teamLabel = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	rowC      = "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd"
)

// The intake S1 read: one query for the team with no project filter. Each
// row is proven the team's, labelled and recent; a row whose reported
// project no bound pair holds is marked, one with no project is not, and
// anything the provider fails to prove refuses the page.
func TestTeamLabelledV2(t *testing.T) {
	s, _, p, scope := testV2(t)
	p.labels = []linearprovider.Label{{ID: teamLabel, Name: "needs-agent", TeamID: teamID, EligibilityKnown: true}}
	const since = "2026-10-22T09:00:00Z"
	row := func(id, team, project string, known bool) linearprovider.IssueV2 {
		return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, TeamID: team, ProjectID: project}, LabelIDs: []string{teamLabel}, ProjectKnown: known, LabelsKnown: true, UpdatedAt: "2026-10-22T10:00:00Z"}
	}
	in := TeamLabelledInputV2{TeamID: teamID, LabelID: teamLabel, UpdatedAfter: since, Limit: 50}
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{row(issueID, teamID, projectA, true), row(parentA, teamID, projectB, true), row(rowC, teamID, "", false)}, NextCursor: "next", Complete: false}}
	page, e := s.TeamLabelledV2(context.Background(), scope, in)
	if e != nil {
		t.Fatal(e)
	}
	// projectB is bound, but with the other team: no pair holds it here.
	want := []TeamIssueRefV2{{ID: issueID}, {ID: parentA, OutOfBinding: true}, {ID: rowC}}
	if !reflect.DeepEqual(page.Items, want) || page.NextCursor != "next" || page.Complete {
		t.Fatal(page)
	}
	q := p.queries[len(p.queries)-1]
	if q.TeamID != teamID || q.ProjectID != "" || q.LabelID != teamLabel || q.UpdatedAfter != since || q.Limit != 50 {
		t.Fatal(q)
	}
	in.Cursor = "next"
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Complete: true}}
	if page, e = s.TeamLabelledV2(context.Background(), scope, in); e != nil || !page.Complete || len(page.Items) != 0 || p.queries[len(p.queries)-1].Cursor != "next" {
		t.Fatal(page, e)
	}
	in.Cursor = ""

	// A sub-team's rows under the parent's filter: a listed team's row is
	// left to that team's own read, an unlisted team's is counted; neither
	// is returned.
	const unlistedTeam = "abababab-abab-4bab-8bab-abababababab"
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{row(issueID, teamID, projectA, true), row(parentA, otherTeamID, projectB, true), row(rowC, unlistedTeam, projectA, true), row("dededede-dede-4ede-8ede-dededededede", unlistedTeam, "", false)}, Complete: true}}
	if page, e = s.TeamLabelledV2(context.Background(), scope, in); e != nil || !reflect.DeepEqual(page.Items, []TeamIssueRefV2{{ID: issueID}}) || page.FilteredOtherTeam != 2 {
		t.Fatal(page, e)
	}

	bad := map[string]linearprovider.Page[linearprovider.IssueV2]{
		"older": {Items: []linearprovider.IssueV2{func() linearprovider.IssueV2 {
			r := row(issueID, teamID, projectA, true)
			r.UpdatedAt = "2026-10-21T09:00:00Z"
			return r
		}()}, Complete: true},
		"unlabelled": {Items: []linearprovider.IssueV2{func() linearprovider.IssueV2 { r := row(issueID, teamID, projectA, true); r.LabelIDs = nil; return r }()}, Complete: true},
		"label by name": {Items: []linearprovider.IssueV2{func() linearprovider.IssueV2 {
			r := row(issueID, teamID, projectA, true)
			r.LabelsKnown, r.LabelNames = false, []string{"other"}
			return r
		}()}, Complete: true},
		"bad id":          {Items: []linearprovider.IssueV2{row("FIX-1", teamID, projectA, true)}, Complete: true},
		"no continuation": {Complete: false},
		"both":            {NextCursor: "x", Complete: true},
		"too many":        {Items: []linearprovider.IssueV2{row(issueID, teamID, projectA, true), row(parentA, teamID, projectA, true)}, Complete: true},
		"stuck":           {NextCursor: "same", Complete: false},
		"long next":       {NextCursor: strings.Repeat("n", 4097), Complete: false},
	}
	for name, pg := range bad {
		p.pages = []linearprovider.Page[linearprovider.IssueV2]{pg}
		in := in
		if name == "too many" {
			in.Limit = 1
		}
		if name == "stuck" {
			in.Cursor = "same"
		}
		if _, e := s.TeamLabelledV2(context.Background(), scope, in); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	// The team must be bound, and the label the team's; nothing is sent
	// otherwise.
	queries := len(p.queries)
	for name, in := range map[string]TeamLabelledInputV2{
		"unbound team": {TeamID: "ffffffff-ffff-4fff-8fff-ffffffffffff", LabelID: teamLabel, UpdatedAfter: since, Limit: 50},
		"other label":  {TeamID: teamID, LabelID: "abababab-abab-4bab-8bab-abababababab", UpdatedAfter: since, Limit: 50},
		"other team's": {TeamID: otherTeamID, LabelID: teamLabel, UpdatedAfter: since, Limit: 50},
		"relative":     {TeamID: teamID, LabelID: teamLabel, UpdatedAfter: "-P1D", Limit: 50},
		"no time":      {TeamID: teamID, LabelID: teamLabel, Limit: 50},
		"limit":        {TeamID: teamID, LabelID: teamLabel, UpdatedAfter: since, Limit: 101},
		"no limit":     {TeamID: teamID, LabelID: teamLabel, UpdatedAfter: since},
		"long cursor":  {TeamID: teamID, LabelID: teamLabel, UpdatedAfter: since, Limit: 50, Cursor: strings.Repeat("c", 4097)},
	} {
		if _, e := s.TeamLabelledV2(context.Background(), scope, in); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	if len(p.queries) != queries {
		t.Fatal("a refused read listed issues")
	}
	// A label that isn't an ID is refused before any Linear call.
	tools := p.tools
	if _, e = s.TeamLabelledV2(context.Background(), scope, TeamLabelledInputV2{TeamID: teamID, LabelID: "needs-agent", UpdatedAfter: since, Limit: 50}); e == nil || p.tools != tools {
		t.Fatal("a label name was sent:", e)
	}
	_, e = s.TeamLabelledV2(context.Background(), scope, TeamLabelledInputV2{TeamID: "ffffffff-ffff-4fff-8fff-ffffffffffff", LabelID: teamLabel, UpdatedAfter: since, Limit: 50})
	if !IsOutOfScope(e) {
		t.Fatal("an unbound team is not out of scope:", e)
	}
}

// A team-board binding has no pairs to rule an issue out: every row goes
// on to IssueV2.
func TestTeamLabelledV2TeamBoard(t *testing.T) {
	s, a, p, scope := testV2(t)
	a.b.Mode, a.b.LinearProjectIDs = "team_board", nil
	p.labels = []linearprovider.Label{{ID: teamLabel, Name: "needs-agent", TeamID: teamID, EligibilityKnown: true}}
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{{Issue: linearprovider.Issue{ID: issueID, TeamID: teamID, ProjectID: projectB}, LabelIDs: []string{teamLabel}, ProjectKnown: true, LabelsKnown: true, UpdatedAt: "2026-10-22T10:00:00Z"}}, Complete: true}}
	page, e := s.TeamLabelledV2(context.Background(), scope, TeamLabelledInputV2{TeamID: teamID, LabelID: teamLabel, UpdatedAfter: "2026-10-22T09:00:00Z", Limit: 50})
	if e != nil || !reflect.DeepEqual(page.Items, []TeamIssueRefV2{{ID: issueID}}) {
		t.Fatal(page, e)
	}
}

// Native rows that name their labels pass only with the selected label's
// unique name.
func TestTeamLabelledV2NamesOnlyRows(t *testing.T) {
	named := func(id string, names ...string) linearprovider.IssueV2 {
		return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, TeamID: teamID, ProjectID: projectA}, ProjectKnown: true, LabelNames: names, UpdatedAt: "2026-10-22T10:00:00Z"}
	}
	s, p, _, _ := resolvingV2(t, nil, []linearprovider.LabelName{{ID: labelA, Name: "Bug"}, {ID: memberA, Name: "Ops"}}, nil)
	_, _, _, scope := testV2(t)
	p.labels = []linearprovider.Label{{ID: labelA, TeamID: teamID, EligibilityKnown: true}}
	in := TeamLabelledInputV2{TeamID: teamID, LabelID: labelA, UpdatedAfter: "2026-10-22T09:00:00Z", Limit: 50}
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{named(issueID, "Ops", "Bug")}, Complete: true}}
	if _, e := s.TeamLabelledV2(context.Background(), scope, in); e != nil {
		t.Fatal(e)
	}
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{named(issueID, "Ops")}, Complete: true}}
	if _, e := s.TeamLabelledV2(context.Background(), scope, in); e == nil {
		t.Fatal("a row without the label's name passed")
	}
}

// bumpingV2 changes the binding while the page is read.
type bumpingV2 struct {
	*providerV2
	bump func()
}

func (p bumpingV2) IssuesV2(ctx context.Context, q linearprovider.IssueQueryV2) (linearprovider.Page[linearprovider.IssueV2], error) {
	p.bump()
	return p.providerV2.IssuesV2(ctx, q)
}

// A binding changed during the read refuses the page.
func TestTeamLabelledV2BindingChangedDuringTheRead(t *testing.T) {
	s, a, p, scope := testV2(t)
	p.labels = []linearprovider.Label{{ID: teamLabel, Name: "needs-agent", TeamID: teamID, EligibilityKnown: true}}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
		return bumpingV2{p, func() { a.b.Revision++ }}, nil
	}
	if _, e := s.TeamLabelledV2(context.Background(), scope, TeamLabelledInputV2{TeamID: teamID, LabelID: teamLabel, UpdatedAfter: "2026-10-22T09:00:00Z", Limit: 50}); e == nil {
		t.Fatal("a page read across a binding change was accepted")
	}
}
