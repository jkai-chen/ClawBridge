package linearboard

import (
	"clawbridge/internal/linearprovider"
	"context"
	"strings"
	"testing"
)

const (
	pjGroup  = "e1e1e1e1-e1e1-4e1e-8e1e-e1e1e1e1e1e1"
	pjReview = "e2e2e2e2-e2e2-4e2e-8e2e-e2e2e2e2e2e2"
	pjOldTag = "e3e3e3e3-e3e3-4e3e-8e3e-e3e3e3e3e3e3"
	pjMulti  = "e4e4e4e4-e4e4-4e4e-8e4e-e4e4e4e4e4e4"
	pjWork   = "e5e5e5e5-e5e5-4e5e-8e5e-e5e5e5e5e5e5"
	pjOther  = "e6e6e6e6-e6e6-4e6e-8e6e-e6e6e6e6e6e6"
)

func projectLabels() []KeyProjectLabel {
	single := func(id, team string) KeyProjectLabel {
		return KeyProjectLabel{ID: id, TeamID: team, ParentID: pjGroup, ParentIsGroup: true, ParentGroupType: "singleSelect"}
	}
	return []KeyProjectLabel{
		{ID: pjGroup, TeamID: teamID, Group: true},
		single(pjReview, teamID),
		single(pjWork, ""), // a workspace label
		single(pjOther, otherTeamID),
		{ID: pjOldTag, TeamID: teamID, ParentID: pjGroup, ParentIsGroup: true, ParentGroupType: "singleSelect", Archived: true},
		{ID: pjMulti, TeamID: teamID, ParentID: pjGroup, ParentIsGroup: true, ParentGroupType: "multiSelect"},
	}
}

func projectInput(labels ...string) ViewWriteInput {
	return ViewWriteInput{Action: "create", Kind: "project", Team: teamID, Name: "Review", Filter: []byte(`{"labels":{"some":{"id":{"in":["` + strings.Join(labels, `","`) + `"]}}}}`), OperationID: "pv-1"}
}

// A project view names only live choices of a single-select group, of the
// view's team or the workspace, proven by ID with the owner's key (D1–D3);
// its filter is project labels only; it's unavailable to agents.
func TestProjectViewWrite(t *testing.T) {
	s, a, _, scope := viewWriteTest(t)
	key := &fakeKey{labels: projectLabels()}
	got, e := s.ViewWrite(context.Background(), scope, projectInput(pjReview, pjWork), key)
	if e != nil || !got.Preview || got.ViewKind != "project" || got.AgentFilter != "unavailable" || !strings.HasPrefix(got.Scope, "scope: team Team (") {
		t.Fatal(got, e)
	}
	in := projectInput(pjReview, pjWork)
	in.Confirm, in.ExpectedRevision, in.ExpectedFilterSHA = true, got.BindingRevision, got.FilterSHA
	if got, e = s.ViewWrite(context.Background(), scope, in, key); e != nil || got.Outcome != "accepted" || !got.Recorded {
		t.Fatal(got, e)
	}
	if key.kinds[0] != "project" || a.records[0]["kind"] != "project" || a.records[0]["agent_filter"] != "unavailable" {
		t.Fatal(key.kinds, a.records[0])
	}
	for name, tc := range map[string]struct {
		in   ViewWriteInput
		key  ViewKey
		want string
	}{
		"other team": {projectInput(pjOther), key, "belongs to another team"},
		"archived":   {projectInput(pjOldTag), key, "isn't a live project label"},
		"unknown":    {projectInput("e9e9e9e9-e9e9-4e9e-8e9e-e9e9e9e9e9e9"), key, "isn't a live project label"},
		"group":      {projectInput(pjGroup), key, "single-select"},
		"multi":      {projectInput(pjMulti), key, "single-select"},
		"no key":     {projectInput(pjReview), nil, "with your Linear API key"},
		"other key":  {ViewWriteInput{Action: "create", Kind: "project", Team: teamID, Name: "Review", Filter: []byte(`{"name":{"eq":"x"}}`), OperationID: "pv-2"}, key, "a project view's filter is"},
		"too many":   {projectInput(strings.Split(strings.Repeat(pjReview+",", 51), ",")[:51]...), key, "1 to 50"},
	} {
		if _, e := s.ViewWrite(context.Background(), scope, tc.in, tc.key); e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Fatal(name, e)
		}
	}
}

// A read-back of another kind is uncertain.
func TestProjectViewReadBackKind(t *testing.T) {
	s, _, _, scope := viewWriteTest(t)
	key := &fakeKey{labels: projectLabels(), back: func(v KeyView) (KeyView, error) { v.Kind = "issue"; return v, nil }}
	in := projectInput(pjReview)
	pre, e := s.ViewWrite(context.Background(), scope, in, key)
	if e != nil {
		t.Fatal(e)
	}
	in.Confirm, in.ExpectedRevision, in.ExpectedFilterSHA = true, pre.BindingRevision, pre.FilterSHA
	got, e := s.ViewWrite(context.Background(), scope, in, key)
	if e != nil || got.Outcome != "uncertain" || len(got.Mismatch) != 1 || got.Mismatch[0] != "kind" {
		t.Fatal(got, e)
	}
}

// Agents filter issues only by issue views.
func TestViewIssuesRefusesAProjectView(t *testing.T) {
	s, a, p, scope := viewWriteTest(t)
	storeView(t, a, `{"labels":{"some":{"id":{"eq":"`+vwLabel+`"}}}}`, true)
	a.stored["kind"] = "project"
	if _, e := s.ViewIssuesV2(context.Background(), scope, vwView, IssuesInputV2{Limit: 5}); e == nil || !strings.Contains(e.Error(), "only by issue views") || len(p.queries) != 0 {
		t.Fatal(e)
	}
}

// L3: a choice of an archived group, or of a group the key can't see, isn't
// live.
func TestProjectViewRefusesAnArchivedGroup(t *testing.T) {
	s, _, _, scope := viewWriteTest(t)
	for name, labels := range map[string][]KeyProjectLabel{
		"archived group": append([]KeyProjectLabel{{ID: pjGroup, TeamID: teamID, Group: true, Archived: true}}, projectLabels()[1:]...),
		"unseen group":   projectLabels()[1:],
	} {
		key := &fakeKey{labels: labels}
		if _, e := s.ViewWrite(context.Background(), scope, projectInput(pjReview), key); e == nil || !strings.Contains(e.Error(), "archived label group") {
			t.Fatal(name, e)
		}
	}
}

// L2: an operation ID used for an issue view's create can't replay that
// receipt for a project view with the same team, name and filter.
func TestViewKindIsInTheOperation(t *testing.T) {
	s, _, p, scope := viewWriteTest(t)
	labels := append(projectLabels(), KeyProjectLabel{ID: vwLabel, TeamID: teamID, ParentID: pjGroup, ParentIsGroup: true, ParentGroupType: "singleSelect"})
	p.labels = append(p.labels, linearprovider.Label{ID: vwLabel, TeamID: teamID})
	key := &fakeKey{labels: labels}
	filter := `{"labels":{"some":{"id":{"in":["` + vwLabel + `"]}}}}`
	confirm := func(kind string) (ViewWriteResult, error) {
		in := ViewWriteInput{Action: "create", Kind: kind, Team: teamID, Name: "Review", Filter: []byte(filter), OperationID: "same-op"}
		pre, e := s.ViewWrite(context.Background(), scope, in, key)
		if e != nil {
			t.Fatal(kind, e)
		}
		in.Confirm, in.ExpectedRevision, in.ExpectedFilterSHA = true, pre.BindingRevision, pre.FilterSHA
		return s.ViewWrite(context.Background(), scope, in, key)
	}
	if got, e := confirm("issue"); e != nil || got.Outcome != "accepted" {
		t.Fatal(got, e)
	}
	if got, e := confirm("project"); e == nil {
		t.Fatal("a project create replayed an issue receipt:", got)
	}
	if len(key.creates) != 1 {
		t.Fatal(key.creates)
	}
}
