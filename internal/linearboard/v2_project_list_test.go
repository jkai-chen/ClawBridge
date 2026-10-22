package linearboard

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

var _ teamProjectLister = (*linearprovider.Provider)(nil)

const trashedTwin = "88888888-8888-4888-8888-888888888888"
const unboundProject = "99999999-8888-4888-8888-888888888888"

// listingV2 answers list_projects per team from probe-shaped pages and counts
// every binding read.
type listingV2 struct {
	countingBindingV2
	pages   map[string][]linearprovider.Page[linearprovider.ProjectListing]
	cursors []string
	lists   int
	err     error
}

func (p *listingV2) TeamProjects(_ context.Context, team string, o linearprovider.PageOptions) (linearprovider.Page[linearprovider.ProjectListing], error) {
	p.lists++
	p.cursors = append(p.cursors, o.Cursor)
	if p.err != nil {
		return linearprovider.Page[linearprovider.ProjectListing]{}, p.err
	}
	pages := p.pages[team]
	if len(pages) == 0 {
		return linearprovider.Page[linearprovider.ProjectListing]{Items: []linearprovider.ProjectListing{}, Complete: true}, nil
	}
	p.pages[team] = pages[1:]
	return pages[0], nil
}

func row(id, name string, trashed bool, teams ...string) linearprovider.ProjectListing {
	return linearprovider.ProjectListing{Project: linearprovider.Project{ID: id, Name: name, TeamIDs: teams}, Trashed: trashed}
}
func complete(rows ...linearprovider.ProjectListing) []linearprovider.Page[linearprovider.ProjectListing] {
	return []linearprovider.Page[linearprovider.ProjectListing]{{Items: rows, Complete: true}}
}

// newListing returns the testV2 binding (teams teamID and otherTeamID, projects
// A and B) with each team's list as Linear returned it in the probe: newest
// first, a trashed project sharing a bound project's name, and an unbound one.
func newListing(t *testing.T) (*listingV2, *providerV2, *int, *int) {
	_, _, p, _ := testV2(t)
	teams, projects := 0, 0
	l := &listingV2{countingBindingV2: countingBindingV2{providerV2: p, teams: &teams, projects: &projects}, pages: map[string][]linearprovider.Page[linearprovider.ProjectListing]{
		teamID:      complete(row(unboundProject, "Unbound", false, teamID), row(projectA, "Same name", false, teamID), row(trashedTwin, "Same name", true, teamID)),
		otherTeamID: complete(row(projectB, "B", false, otherTeamID)),
	}}
	return l, p, &teams, &projects
}

func TestLinearV2BindingVerifiesProjectsFromOneListPerTeam(t *testing.T) {
	_, a, p, _ := testV2(t)
	want, e := verifyBindingV2(context.Background(), p, a.b)
	if e != nil {
		t.Fatal(e)
	}
	l, _, teams, projects := newListing(t)
	got, e := verifyBindingV2(context.Background(), l, a.b)
	if e != nil {
		t.Fatal(e)
	}
	if *teams != 2 || l.lists != 2 || *projects != 0 {
		t.Fatalf("reads teams=%d lists=%d projects=%d", *teams, l.lists, *projects)
	}
	for n := range got.Projects {
		got.Projects[n].Name = "" // providerV2's get_project returns no names.
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batched proof differs\n got %+v\nwant %+v", got, want)
	}
}

func TestLinearV2BindingChecksTrashedMissingOrInconsistentProjectsIndividually(t *testing.T) {
	_, a, _, _ := testV2(t)
	for name, change := range map[string]func(*listingV2){
		// The bound project itself is trashed; its live twin's name never matches.
		"trashed": func(l *listingV2) {
			l.pages[otherTeamID] = complete(row(projectB, "B", true, otherTeamID), row(trashedTwin, "B", false, otherTeamID))
		},
		"missing": func(l *listingV2) { l.pages[otherTeamID] = complete() },
		"listed differently under two teams": func(l *listingV2) {
			l.pages[teamID] = complete(row(projectB, "B", false, teamID, otherTeamID), row(projectA, "A", false, teamID))
		},
		"beyond the page cap": func(l *listingV2) {
			pages := []linearprovider.Page[linearprovider.ProjectListing]{}
			for n := 0; n < maxProjectListPages; n++ {
				pages = append(pages, linearprovider.Page[linearprovider.ProjectListing]{Items: []linearprovider.ProjectListing{}, NextCursor: "c", Complete: false})
			}
			l.pages[otherTeamID] = append(pages, complete(row(projectB, "B", false, otherTeamID))...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			l, _, _, projects := newListing(t)
			change(l)
			if _, e := verifyBindingV2(context.Background(), l, a.b); e != nil {
				t.Fatal(e)
			}
			if *projects != 1 {
				t.Fatalf("get_project reads = %d, want 1 (project B only)", *projects)
			}
		})
	}
}

func TestLinearV2BindingFromListingKeepsFullSetSemantics(t *testing.T) {
	_, a, _, _ := testV2(t)
	l, _, _, _ := newListing(t)
	// Project B is live but no longer in otherTeamID: that team is uncovered.
	l.pages[otherTeamID] = complete()
	l.pages[teamID] = complete(row(projectA, "A", false, teamID), row(projectB, "B", false, teamID))
	if _, e := verifyBindingV2(context.Background(), l, a.b); e == nil || !strings.Contains(e.Error(), "not represented") {
		t.Fatal("uncovered team accepted:", e)
	}
	// A listed project outside every allowed team is refused, as by get_project.
	l, _, _, _ = newListing(t)
	l.pages[otherTeamID] = complete()
	l.providerV2.projects[projectB] = []string{"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}
	if _, e := verifyBindingV2(context.Background(), l, a.b); e == nil || !strings.Contains(e.Error(), "no verified allowed team") {
		t.Fatal("foreign project accepted:", e)
	}
}

func TestLinearV2BindingListingFollowsTheCursor(t *testing.T) {
	_, a, _, _ := testV2(t)
	l, _, _, projects := newListing(t)
	l.pages[otherTeamID] = []linearprovider.Page[linearprovider.ProjectListing]{
		{Items: []linearprovider.ProjectListing{row(trashedTwin, "B", true, otherTeamID)}, NextCursor: trashedTwin},
		{Items: []linearprovider.ProjectListing{row(projectB, "B", false, otherTeamID)}, Complete: true},
	}
	if _, e := verifyBindingV2(context.Background(), l, a.b); e != nil || *projects != 0 {
		t.Fatal(e, *projects)
	}
	if !reflect.DeepEqual(l.cursors, []string{"", "", trashedTwin}) {
		t.Fatal(l.cursors)
	}
}

func TestLinearV2BindingListingFailures(t *testing.T) {
	_, a, _, _ := testV2(t)
	// A listing Linear cannot serve as probed falls back to get_project.
	for _, kind := range []string{"capability_unavailable", "invalid_response", "scope_metadata_missing", "pagination_unavailable", "result_limit"} {
		l, _, _, projects := newListing(t)
		l.err = &linearprovider.Error{Kind: kind}
		if _, e := verifyBindingV2(context.Background(), l, a.b); e != nil || *projects != 2 {
			t.Fatal(kind, e, *projects)
		}
	}
	// Anything else, including a rate limit, fails the command without more calls.
	for _, err := range []error{&linearprovider.Error{Kind: "rate_limited", NotSent: true}, &linearprovider.Error{Kind: "unavailable"}, &linearprovider.Error{Kind: "invalid_response", NotSent: true}, errors.New("synthetic")} {
		l, _, _, projects := newListing(t)
		l.err = err
		if _, e := verifyBindingV2(context.Background(), l, a.b); !errors.Is(e, err) || *projects != 0 || l.lists != 1 {
			t.Fatal(err, e, *projects, l.lists)
		}
	}
}
