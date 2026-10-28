package linearboard

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

// pagedLabels answers each team's labels from its own pages, in order, and
// records the cursors it was asked for.
type pagedLabels struct {
	*providerV2
	pages   map[string][]linearprovider.Page[linearprovider.Label]
	cursors map[string][]string
	// during runs inside each read; wrongTeam answers another team's ID.
	during    func()
	wrongTeam bool
}

func (p *pagedLabels) GetTeam(ctx context.Context, id string) (linearprovider.Team, error) {
	if p.wrongTeam {
		return linearprovider.Team{ID: otherTeamID}, nil
	}
	return p.providerV2.GetTeam(ctx, id)
}

func (p *pagedLabels) Labels(_ context.Context, team string, o linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	p.cursors[team] = append(p.cursors[team], o.Cursor)
	if p.during != nil {
		p.during()
	}
	n := len(p.cursors[team]) - 1
	if n >= len(p.pages[team]) {
		return linearprovider.Page[linearprovider.Label]{Complete: true}, nil
	}
	return p.pages[team][n], nil
}

func mapLabel(id, name, team string) linearprovider.Label {
	return linearprovider.Label{ID: id, Name: name, TeamID: team, EligibilityKnown: true}
}

// FR6: every bound team's labels by name in one call, per team, never
// merged: the same name in two teams keeps both IDs. Group and archived
// labels are left out, a name used twice in a team is a duplicate, and
// each team is paged to its end.
func TestLabelMapV2ListsEachTeam(t *testing.T) {
	const (
		bugA   = "a0000000-0000-4000-8000-000000000001"
		bugB   = "a0000000-0000-4000-8000-000000000002"
		group  = "a0000000-0000-4000-8000-000000000003"
		old    = "a0000000-0000-4000-8000-000000000004"
		areaX  = "a0000000-0000-4000-8000-000000000005"
		areaY  = "a0000000-0000-4000-8000-000000000006"
		global = "a0000000-0000-4000-8000-000000000007"
		alien  = "a0000000-0000-4000-8000-000000000008"
	)
	s, _, p, scope := testV2(t)
	grouped, archived := mapLabel(group, "Type", teamID), mapLabel(old, "Old", teamID)
	grouped.Group, archived.Archived = true, true
	workspace := linearprovider.Label{ID: global, Name: "Blocked", Global: true, EligibilityKnown: true}
	f := &pagedLabels{providerV2: p, cursors: map[string][]string{}, pages: map[string][]linearprovider.Page[linearprovider.Label]{
		teamID: {
			{Items: []linearprovider.Label{mapLabel(bugA, "Bug", teamID), grouped, archived, mapLabel(areaX, "Docs", teamID)}, NextCursor: "more"},
			{Items: []linearprovider.Label{mapLabel(areaY, "Docs", teamID), workspace, mapLabel(alien, "Alien", otherTeamID), mapLabel(bugA, "Bug", teamID)}, Complete: true},
		},
		otherTeamID: {{Items: []linearprovider.Label{mapLabel(bugB, "Bug", otherTeamID), workspace}, Complete: true}},
	}}
	s.cfg.Open = openV2(f)
	got, e := s.LabelMapV2(context.Background(), scope)
	if e != nil {
		t.Fatal(e)
	}
	want := []TeamLabelsV2{
		{TeamID: teamID, Default: true, Labels: map[string]string{"Bug": bugA, "Blocked": global}, Duplicates: map[string][]string{"Docs": {areaX, areaY}}, Complete: true},
		{TeamID: otherTeamID, Labels: map[string]string{"Bug": bugB, "Blocked": global}, Complete: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if !reflect.DeepEqual(f.cursors[teamID], []string{"", "more"}) {
		t.Fatal(f.cursors)
	}
}

// A team with more labels than the bound says so; a page that doesn't
// advance or a label that contradicts itself fails the call.
func TestLabelMapV2BoundsAndChecks(t *testing.T) {
	s, _, p, scope := testV2(t)
	endless := []linearprovider.Page[linearprovider.Label]{}
	for i := 0; i < labelMapPages+1; i++ {
		endless = append(endless, linearprovider.Page[linearprovider.Label]{NextCursor: "c" + strings.Repeat("x", i+1)})
	}
	f := &pagedLabels{providerV2: p, cursors: map[string][]string{}, pages: map[string][]linearprovider.Page[linearprovider.Label]{teamID: endless}}
	s.cfg.Open = openV2(f)
	got, e := s.LabelMapV2(context.Background(), scope)
	if e != nil || got[0].Complete || len(f.cursors[teamID]) != labelMapPages || !got[1].Complete {
		t.Fatal(got, e, len(f.cursors[teamID]))
	}
	for name, page := range map[string]linearprovider.Page[linearprovider.Label]{
		"stuck":         {NextCursor: ""},
		"contradiction": {Items: []linearprovider.Label{mapLabel(labelA, "One", teamID), mapLabel(labelA, "Two", teamID)}, Complete: true},
		"invalid":       {Items: []linearprovider.Label{mapLabel("x", "One", teamID)}, Complete: true},
	} {
		f = &pagedLabels{providerV2: p, cursors: map[string][]string{}, pages: map[string][]linearprovider.Page[linearprovider.Label]{teamID: {page}}}
		s.cfg.Open = openV2(f)
		if _, e = s.LabelMapV2(context.Background(), scope); e == nil {
			t.Fatal(name, "accepted")
		}
	}
}

// The binding is proven before any label is read, and a binding that
// changes during the call fails it.
func TestLabelMapV2ProvesTheBinding(t *testing.T) {
	s, a, p, scope := testV2(t)
	f := &pagedLabels{providerV2: p, cursors: map[string][]string{}, wrongTeam: true}
	s.cfg.Open = openV2(f)
	if _, e := s.LabelMapV2(context.Background(), scope); e == nil || len(f.cursors) != 0 {
		t.Fatal("labels read for an unproven binding:", e, f.cursors)
	}
	f = &pagedLabels{providerV2: p, cursors: map[string][]string{}, during: func() { a.b.Revision++ }}
	s.cfg.Open = openV2(f)
	if _, e := s.LabelMapV2(context.Background(), scope); e == nil || !strings.Contains(e.Error(), "binding_changed") {
		t.Fatal("a binding changed mid-call accepted:", e)
	}
}
