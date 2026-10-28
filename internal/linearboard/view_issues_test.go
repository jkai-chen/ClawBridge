package linearboard

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

const (
	viIssue1 = "10101010-1010-4010-8010-101010101010"
	viIssue2 = "20202020-2020-4020-8020-202020202020"
	viIssue3 = "30303030-3030-4030-8030-303030303030"
)

func viewRow(id string, priority *int, labels ...string) linearprovider.IssueV2 {
	return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, TeamID: teamID, ProjectID: projectA}, ProjectKnown: true, LabelIDs: labels, LabelsKnown: true, Priority: priority, PriorityKnown: priority != nil}
}

func storeView(t *testing.T, a *viewAuthority, filter string, bound bool) {
	t.Helper()
	a.stored = map[string]any{"view_id": vwView, "team_id": teamID, "name": "Urgent bugs", "filter": filter, "filter_sha256": mustFilterSHA(t, filter), "filter_as_of": "2026-10-27T21:00:00.000Z", "team_bound": bound}
}

// K3: issues by a view are the plain issues rows the filter matches, on the
// view's team only, with filter_as_of and agent_filter; a row whose
// priority isn't known is left out and counted.
func TestViewIssuesFiltersRows(t *testing.T) {
	s, a, p, scope := viewWriteTest(t)
	storeView(t, a, `{"and":[{"labels":{"some":{"id":{"eq":"`+vwLabel+`"}}}},{"priority":{"in":[1,2]}}]}`, true)
	one, three := 1, 3
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{viewRow(viIssue1, &one, vwLabel), viewRow(viIssue2, &three, vwLabel), viewRow(viIssue3, nil, vwLabel)}, Complete: true}}
	got, e := s.ViewIssuesV2(context.Background(), scope, vwView, IssuesInputV2{Limit: 50})
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Items) != 1 || got.Items[0].ID != viIssue1 || !got.Complete || got.FilterAsOf != "2026-10-27T21:00:00.000Z" || got.AgentFilter != "available" || got.ViewID != vwView || !reflect.DeepEqual(got.Unknown, map[string]int{"priority_unknown": 1}) {
		t.Fatal(got)
	}
	// One query, on the view's team only.
	if len(p.queries) != 1 || p.queries[0].TeamID != teamID || p.queries[0].ProjectID != projectA || p.queries[0].LabelID != "" {
		t.Fatal(p.queries)
	}
	// The answer's keys are the plain issues answer's, plus the view's.
	plain, _ := json.Marshal(linearprovider.Page[linearprovider.IssueV2]{Items: []linearprovider.IssueV2{}})
	viewed, _ := json.Marshal(got)
	if keys(t, viewed) != keys(t, plain)+",agent_filter,filter_as_of,unknown,view_id" {
		t.Fatal(keys(t, viewed), keys(t, plain))
	}
	// Through ExecuteV2, as the MCP tool calls it.
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{viewRow(viIssue1, &one, vwLabel)}, Complete: true}}
	out, e := s.ExecuteV2(context.Background(), scope, "issues", map[string]any{"limit": float64(10), "view_id": vwView})
	if page, ok := out.(ViewIssuesPage); e != nil || !ok || len(page.Items) != 1 {
		t.Fatal(out, e)
	}
}

func keys(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var out []string
	for k := range m {
		if k != "agent_filter" && k != "filter_as_of" && k != "unknown" && k != "view_id" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	extra := []string{}
	for _, k := range []string{"agent_filter", "filter_as_of", "unknown", "view_id"} {
		if _, ok := m[k]; ok {
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return strings.Join(out, ",")
	}
	return strings.Join(out, ",") + "," + strings.Join(extra, ",")
}

// A state-type filter reads the team's states once and evaluates rows by
// their state's type.
func TestViewIssuesStateType(t *testing.T) {
	s, a, p, scope := viewWriteTest(t)
	storeView(t, a, `{"state":{"type":{"in":["started"]}}}`, true)
	row := viewRow(viIssue1, nil)
	// The fake's only state is issueID, of no type: the row is unknown.
	row.StateID = issueID
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{row}, Complete: true}}
	got, e := s.ViewIssuesV2(context.Background(), scope, vwView, IssuesInputV2{Limit: 50})
	if e != nil || len(got.Items) != 0 || got.Unknown["state_unknown"] != 1 {
		t.Fatal(got, e)
	}
}

// Refused without reading Linear: an unknown view, one whose team left the
// binding, one whose filter agents can't use, and view_id with label_id.
func TestViewIssuesRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		filter string
		bound  bool
		store  bool
		in     IssuesInputV2
		want   string
	}{
		"unknown":     {in: IssuesInputV2{Limit: 5}, want: "not_found"},
		"left":        {filter: `{"priority":{"eq":1}}`, store: true, in: IssuesInputV2{Limit: 5}, want: "left the binding"},
		"unavailable": {filter: `{"or":[{"priority":{"eq":1}},{"priority":{"gt":3}}]}`, bound: true, store: true, in: IssuesInputV2{Limit: 5}, want: "agent_filter: unavailable (uses: or)"},
		"label":       {filter: `{"priority":{"eq":1}}`, bound: true, store: true, in: IssuesInputV2{Limit: 5, LabelID: vwLabel}, want: "don't combine"},
	} {
		s, a, p, scope := viewWriteTest(t)
		if tc.store {
			storeView(t, a, tc.filter, tc.bound)
		}
		if _, e := s.ViewIssuesV2(context.Background(), scope, vwView, tc.in); e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Fatal(name, e)
		}
		if len(p.queries) != 0 {
			t.Fatal(name, "read Linear")
		}
	}
}

// L4: a view matching no row answers items [], never null.
func TestViewIssuesEmptyIsAnEmptyList(t *testing.T) {
	s, a, p, scope := viewWriteTest(t)
	storeView(t, a, `{"priority":{"eq":1}}`, true)
	three := 3
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{viewRow(viIssue2, &three)}, Complete: true}}
	got, e := s.ViewIssuesV2(context.Background(), scope, vwView, IssuesInputV2{Limit: 50})
	raw, _ := json.Marshal(got)
	if e != nil || !strings.Contains(string(raw), `"items":[]`) {
		t.Fatal(string(raw), e)
	}
}
