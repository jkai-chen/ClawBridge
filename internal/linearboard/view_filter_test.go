package linearboard

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

// Synthetic IDs, nobody's.
const (
	vfTeam     = "10000000-0000-4000-8000-000000000001"
	vfOther    = "10000000-0000-4000-8000-000000000002"
	vfProject  = "20000000-0000-4000-8000-000000000001"
	vfProject2 = "20000000-0000-4000-8000-000000000002"
	vfLabel    = "30000000-0000-4000-8000-000000000001"
	vfLabel2   = "30000000-0000-4000-8000-000000000002"
	vfState    = "40000000-0000-4000-8000-000000000001"
	vfState2   = "40000000-0000-4000-8000-000000000002"
	vfMember   = "50000000-0000-4000-8000-000000000001"
	vfMember2  = "50000000-0000-4000-8000-000000000002"
)

func vfScope() ViewScope {
	return ViewScope{
		Team:     vfTeam,
		Projects: map[string]bool{vfProject: true, vfProject2: true},
		Labels:   map[string]bool{vfLabel: true, vfLabel2: true},
		States:   map[string]bool{vfState: true, vfState2: true},
		Members:  map[string]bool{vfMember: true, vfMember2: true},
	}
}

func mustFilter(t *testing.T, text string) ViewFilter {
	t.Helper()
	f, err := ParseViewFilter([]byte(text))
	if err != nil {
		t.Fatal(text, err)
	}
	return f
}

// The subset: each key in its accepted shapes, alone and under one and,
// is available to agents; anything else is recorded in Uses, not refused.
func TestViewFilterSubset(t *testing.T) {
	for _, text := range []string{
		`{"team":{"id":{"eq":"` + vfTeam + `"}}}`,
		`{"project":{"id":{"eq":"` + vfProject + `"}}}`,
		`{"project":{"id":{"in":["` + vfProject + `","` + vfProject2 + `"]}}}`,
		`{"labels":{"some":{"id":{"eq":"` + vfLabel + `"}}}}`,
		`{"labels":{"some":{"id":{"in":["` + vfLabel + `"]}}}}`,
		`{"state":{"type":{"in":["started","unstarted"]}}}`,
		`{"state":{"id":{"in":["` + vfState + `"]}}}`,
		`{"assignee":{"id":{"eq":"` + vfMember + `"}}}`,
		`{"assignee":{"null":true}}`,
		`{"priority":{"eq":1}}`,
		`{"priority":{"in":[1,2]}}`,
		`{"priority":{"lte":2}}`,
		`{"priority":{"gt":1}}`,
		`{"and":[{"labels":{"some":{"id":{"eq":"` + vfLabel + `"}}}},{"state":{"type":{"in":["started"]}}}]}`,
		`{"team":{"id":{"eq":"` + vfTeam + `"}},"priority":{"eq":1}}`,
	} {
		f := mustFilter(t, text)
		if f.AgentFilter() != AgentFilterAvailable || len(f.Uses) != 0 || len(f.clauses) == 0 {
			t.Fatal(text, f.Uses)
		}
		if err := f.CheckIDs(vfScope()); err != nil {
			t.Fatal(text, err)
		}
	}
	for text, uses := range map[string][]string{
		`{"or":[{"priority":{"eq":1}}]}`:                                        {"or"},
		`{"not":{"priority":{"eq":1}}}`:                                         {"not"},
		`{"title":{"contains":"x"}}`:                                            {"title"},
		`{"team":{"id":{"in":["` + vfTeam + `"]}}}`:                             {"team.id.in"},
		`{"team":{"key":{"eq":"ENG"}}}`:                                         {"team.key"},
		`{"labels":{"every":{"id":{"eq":"` + vfLabel + `"}}}}`:                  {"labels.every"},
		`{"labels":{"some":{"name":{"eq":"Bug"}}}}`:                             {"labels.some.name"},
		`{"state":{"name":{"eq":"Done"}}}`:                                      {"state.name"},
		`{"state":{"type":{"eq":"started"}}}`:                                   {"state.type.eq"},
		`{"assignee":{"isMe":{"eq":true}}}`:                                     {"assignee.isMe"},
		`{"assignee":{"null":false}}`:                                           {"assignee.null"},
		`{"and":[{"and":[{"priority":{"eq":1}}]}]}`:                             {"and"},
		`{"and":[{"priority":{"eq":1}},{"or":[]},{"cycle":{"id":{"eq":"x"}}}]}`: {"cycle", "or"},
	} {
		f := mustFilter(t, text)
		if f.AgentFilter() != AgentFilterUnavailable || !reflect.DeepEqual(f.Uses, uses) || f.clauses != nil {
			t.Fatal(text, f.Uses)
		}
		if ok, unknown := f.Match(linearprovider.IssueV2{}, nil); ok || unknown != "agent_filter_unavailable" {
			t.Fatal("an unavailable filter matched", text)
		}
	}
	nine := `{"and":[` + strings.Repeat(`{"priority":{"eq":1}},`, 8) + `{"priority":{"eq":1}}]}`
	if f := mustFilter(t, nine); !reflect.DeepEqual(f.Uses, []string{"and"}) {
		t.Fatal("nine parts:", f.Uses)
	}
}

// Shapes and values inside the subset that are wrong are refused, as are
// texts that aren't one bounded object.
func TestViewFilterRefusals(t *testing.T) {
	many := make([]string, 21)
	for i := range many {
		many[i] = `"` + vfProject + `"`
	}
	for _, text := range []string{
		``, `[]`, `{}`, `"x"`, `{"priority":{"eq":1}} {}`, `{`,
		`{"priority":{"eq":5}}`, `{"priority":{"eq":-1}}`, `{"priority":{"eq":1.5}}`, `{"priority":{"eq":"1"}}`,
		`{"priority":{"in":[]}}`, `{"priority":{"in":[1,2,3,4,0,1]}}`,
		`{"state":{"type":{"in":["done"]}}}`, `{"state":{"type":{"in":[]}}}`,
		`{"state":{"id":{"in":["not-an-id"]}}}`,
		`{"project":{"id":{"eq":"P-ENG-1"}}}`,
		`{"project":{"id":{"in":[` + strings.Join(many, ",") + `]}}}`,
		`{"assignee":{"id":{"eq":"me"}}}`,
		`{"team":{"id":{"eq":"ABCDEF00-0000-4000-8000-00000000000A"}}}`,
		`{"title":{"eq":"ABCDEF00-0000-4000-8000-00000000000A"}}`,
		`{"a":` + strings.Repeat(`{"a":`, 12) + `1` + strings.Repeat(`}`, 13) + `}`,
		`{"title":{"eq":"` + strings.Repeat("x", 16<<10) + `"}}`,
	} {
		if _, err := ParseViewFilter([]byte(text)); !errors.Is(err, ErrViewFilter) {
			if len(text) > 80 {
				text = text[:80]
			}
			t.Fatal("accepted", text, err)
		}
	}
}

// Every UUID anywhere, under keys the subset doesn't evaluate too, must be
// the view's team or the binding's; the refusal names its path. Another
// bound team is still another team.
func TestViewFilterCheckIDs(t *testing.T) {
	outside := "60000000-0000-4000-8000-000000000001"
	for text, path := range map[string]string{
		`{"team":{"id":{"eq":"` + vfOther + `"}}}`:                                                 "filter.team.id.eq",
		`{"project":{"id":{"in":["` + vfProject + `","` + outside + `"]}}}`:                        "filter.project.id.in[1]",
		`{"or":[{"cycle":{"id":{"eq":"` + outside + `"}}}]}`:                                       "filter.or[0].cycle.id.eq",
		`{"creator":{"id":{"eq":"` + outside + `"}}}`:                                              "filter.creator.id.eq",
		`{"and":[{"priority":{"eq":1}},{"subscribers":{"some":{"id":{"eq":"` + outside + `"}}}}]}`: "filter.and[1].subscribers.some.id.eq",
	} {
		scope := vfScope()
		scope.Members[vfOther] = false
		err := mustFilter(t, text).CheckIDs(scope)
		if !errors.Is(err, ErrViewFilter) || !strings.Contains(err.Error(), path+" names an ID outside") {
			t.Fatal(text, err)
		}
	}
	// IDs of each kind pass, wherever they sit; plain strings aren't IDs.
	ok := `{"or":[{"creator":{"id":{"eq":"` + vfMember2 + `"}}},{"labels":{"some":{"id":{"eq":"` + vfLabel2 + `"}}}},{"state":{"id":{"in":["` + vfState2 + `"]}}},{"title":{"contains":"team"}}]}`
	if err := mustFilter(t, ok).CheckIDs(vfScope()); err != nil {
		t.Fatal(err)
	}
}

// The canonical text sorts keys and keeps numbers exactly, so equal
// filters written differently share a digest.
func TestViewFilterCanonical(t *testing.T) {
	a := mustFilter(t, `{ "priority": {"eq": 1}, "team": {"id": {"eq": "`+vfTeam+`"}} }`)
	b := mustFilter(t, `{"team":{"id":{"eq":"`+vfTeam+`"}},"priority":{"eq":1}}`)
	if a.Canonical != b.Canonical || a.SHA256 != b.SHA256 || a.Canonical != `{"priority":{"eq":1},"team":{"id":{"eq":"`+vfTeam+`"}}}` || len(a.SHA256) != 64 {
		t.Fatal(a.Canonical, b.Canonical)
	}
	if c := mustFilter(t, `{"priority":{"eq":2}}`); c.SHA256 == a.SHA256 {
		t.Fatal("different filters share a digest")
	}
}

func vfRow(mod func(*linearprovider.IssueV2)) linearprovider.IssueV2 {
	p := 2
	r := linearprovider.IssueV2{
		Issue:         linearprovider.Issue{TeamID: vfTeam, ProjectID: vfProject, StateID: vfState},
		ProjectKnown:  true,
		LabelIDs:      []string{vfLabel},
		LabelsKnown:   true,
		AssigneeID:    vfMember,
		AssigneeKnown: true,
		Priority:      &p,
		PriorityKnown: true,
	}
	if mod != nil {
		mod(&r)
	}
	return r
}

// Match: each clause on rows, every clause must hold, and a row lacking
// what a clause needs fails closed with a named unknown.
func TestViewFilterMatch(t *testing.T) {
	types := map[string]string{vfState: "started", vfState2: "completed"}
	type c struct {
		filter  string
		row     linearprovider.IssueV2
		match   bool
		unknown string
	}
	for i, tc := range []c{
		{`{"team":{"id":{"eq":"` + vfTeam + `"}}}`, vfRow(nil), true, ""},
		{`{"team":{"id":{"eq":"` + vfTeam + `"}}}`, vfRow(func(r *linearprovider.IssueV2) { r.TeamID = vfOther }), false, ""},
		{`{"project":{"id":{"in":["` + vfProject2 + `","` + vfProject + `"]}}}`, vfRow(nil), true, ""},
		{`{"project":{"id":{"eq":"` + vfProject2 + `"}}}`, vfRow(nil), false, ""},
		{`{"project":{"id":{"eq":"` + vfProject + `"}}}`, vfRow(func(r *linearprovider.IssueV2) { r.ProjectKnown = false }), false, "project_unknown"},
		{`{"labels":{"some":{"id":{"in":["` + vfLabel2 + `","` + vfLabel + `"]}}}}`, vfRow(nil), true, ""},
		{`{"labels":{"some":{"id":{"eq":"` + vfLabel2 + `"}}}}`, vfRow(nil), false, ""},
		{`{"labels":{"some":{"id":{"eq":"` + vfLabel + `"}}}}`, vfRow(func(r *linearprovider.IssueV2) { r.LabelsKnown = false }), false, "labels_unknown"},
		{`{"state":{"type":{"in":["started"]}}}`, vfRow(nil), true, ""},
		{`{"state":{"type":{"in":["completed","canceled"]}}}`, vfRow(nil), false, ""},
		{`{"state":{"type":{"in":["started"]}}}`, vfRow(func(r *linearprovider.IssueV2) { r.StateID = "" }), false, "state_unknown"},
		{`{"state":{"type":{"in":["started"]}}}`, vfRow(func(r *linearprovider.IssueV2) { r.StateID = vfOther }), false, "state_unknown"},
		{`{"state":{"id":{"in":["` + vfState + `"]}}}`, vfRow(nil), true, ""},
		{`{"state":{"id":{"in":["` + vfState2 + `"]}}}`, vfRow(nil), false, ""},
		{`{"state":{"id":{"in":["` + vfState + `"]}}}`, vfRow(func(r *linearprovider.IssueV2) { r.StateID = "" }), false, "state_unknown"},
		{`{"assignee":{"id":{"eq":"` + vfMember + `"}}}`, vfRow(nil), true, ""},
		{`{"assignee":{"id":{"eq":"` + vfMember2 + `"}}}`, vfRow(nil), false, ""},
		{`{"assignee":{"id":{"eq":"` + vfMember + `"}}}`, vfRow(func(r *linearprovider.IssueV2) { r.AssigneeID = "" }), false, ""},
		{`{"assignee":{"null":true}}`, vfRow(nil), false, ""},
		{`{"assignee":{"null":true}}`, vfRow(func(r *linearprovider.IssueV2) { r.AssigneeID = "" }), true, ""},
		{`{"assignee":{"null":true}}`, vfRow(func(r *linearprovider.IssueV2) { r.AssigneeID, r.AssigneeKnown = "", false }), false, "assignee_unknown"},
		{`{"priority":{"eq":2}}`, vfRow(nil), true, ""},
		{`{"priority":{"eq":1}}`, vfRow(nil), false, ""},
		{`{"priority":{"in":[1,2]}}`, vfRow(nil), true, ""},
		{`{"priority":{"in":[3,4]}}`, vfRow(nil), false, ""},
		{`{"priority":{"eq":0}}`, vfRow(func(r *linearprovider.IssueV2) { r.Priority, r.PriorityKnown = nil, false }), false, "priority_unknown"},
		{`{"priority":{"eq":2}}`, vfRow(func(r *linearprovider.IssueV2) { r.PriorityKnown = false }), false, "priority_unknown"},
		{`{"and":[{"labels":{"some":{"id":{"eq":"` + vfLabel + `"}}}},{"state":{"type":{"in":["started"]}}},{"priority":{"in":[1,2]}}]}`, vfRow(nil), true, ""},
		{`{"and":[{"labels":{"some":{"id":{"eq":"` + vfLabel + `"}}}},{"state":{"type":{"in":["completed"]}}}]}`, vfRow(nil), false, ""},
		{`{"priority":{"eq":2},"assignee":{"id":{"eq":"` + vfMember2 + `"}}}`, vfRow(nil), false, ""},
	} {
		match, unknown := mustFilter(t, tc.filter).Match(tc.row, types)
		if match != tc.match || unknown != tc.unknown {
			t.Fatal(i, tc.filter, match, unknown)
		}
	}
}

// Projects gives the query's projects, or nil when the filter doesn't
// narrow by project.
func TestViewFilterProjects(t *testing.T) {
	if p := mustFilter(t, `{"and":[{"priority":{"eq":1}},{"project":{"id":{"in":["`+vfProject+`","`+vfProject2+`"]}}}]}`).Projects(); !reflect.DeepEqual(p, []string{vfProject, vfProject2}) {
		t.Fatal(p)
	}
	if p := mustFilter(t, `{"priority":{"eq":1}}`).Projects(); p != nil {
		t.Fatal(p)
	}
}

// Priority comparisons are Linear's: plain numeric, with no priority as 0.
// Pinned to the tester's read-only probe on team FJA (27 Oct,
// live/keyprobe-ledger.txt): all issues had priority 0, and Linear's own
// IssueFilter returned all of them for lte 2 and none for gte 2.
func TestViewFilterPriorityComparisonsAreLinears(t *testing.T) {
	none, high, low := 0, 2, 4
	for _, tc := range []struct {
		filter string
		p      int
		match  bool
	}{
		{`{"priority":{"lte":2}}`, none, true},  // the probe: lte 2 includes no priority
		{`{"priority":{"gte":2}}`, none, false}, // the probe: gte 2 excludes it
		{`{"priority":{"lte":2}}`, high, true},
		{`{"priority":{"lte":2}}`, low, false},
		{`{"priority":{"lt":2}}`, high, false},
		{`{"priority":{"gt":2}}`, low, true},
		{`{"priority":{"gte":1}}`, none, false},
		{`{"and":[{"priority":{"lte":2}},{"priority":{"gte":1}}]}`, none, false},
		{`{"and":[{"priority":{"lte":2}},{"priority":{"gte":1}}]}`, high, true},
	} {
		p := tc.p
		row := vfRow(func(r *linearprovider.IssueV2) { r.Priority = &p })
		if ok, unknown := mustFilter(t, tc.filter).Match(row, nil); ok != tc.match || unknown != "" {
			t.Fatal(tc.filter, tc.p, ok, unknown)
		}
	}
	if _, err := ParseViewFilter([]byte(`{"priority":{"gte":5}}`)); err == nil {
		t.Fatal("gte 5 accepted")
	}
}
