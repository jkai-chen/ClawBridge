package linearboard

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	ksGroup = "d1d1d1d1-0000-4000-8000-000000000001"
	ksEng   = "d1d1d1d1-0000-4000-8000-000000000002"
	ksRev   = "d1d1d1d1-0000-4000-8000-000000000003"
	ksOther = "d1d1d1d1-0000-4000-8000-000000000009"
)

// kindAuthority answers the project kind setting and records what a seed
// stores.
type kindAuthority struct {
	*authorityV2
	setting map[string]any
	records []map[string]any
}

func (a *kindAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "project_kind_get":
		return bus.Result{Source: "local-authority", Data: a.setting}, nil
	case "project_kind_record":
		a.records = append(a.records, r.Args)
		return bus.Result{Source: "local-authority", Data: map[string]any{}}, nil
	}
	return a.authorityV2.Execute(ctx, r)
}

// kindProvider plays the owner's MCP project label tools: team labels are
// listed with the team, workspace labels always; a create adds the label.
type kindProvider struct {
	*providerV2
	team, workspace []linearprovider.ProjectLabelRow
	creates         []string
	createErr       error
	answerNoID      bool
	// inWorkspace lands a create among the workspace's labels; wrongID
	// answers another ID than the label made.
	inWorkspace, wrongID bool
}

func (k *kindProvider) ProjectLabelsReason() string { return "" }
func (k *kindProvider) ProjectLabels(_ context.Context, team string) ([]linearprovider.ProjectLabelRow, error) {
	out := append([]linearprovider.ProjectLabelRow{}, k.workspace...)
	if team != "" {
		out = append(out, k.team...)
	}
	return out, nil
}
func (k *kindProvider) create(name, parent string, group bool) (string, error) {
	k.creates = append(k.creates, name)
	if k.createErr != nil {
		return "", k.createErr
	}
	id := map[string]string{"Type": ksGroup, "Engineering": ksEng, "Review": ksRev}[name]
	row := linearprovider.ProjectLabelRow{ID: id, Name: name, TeamName: "Team"}
	if group {
		row.Group, row.GroupType = true, "singleSelect"
	} else {
		row.ParentName, row.ParentGroupType = "Type", "singleSelect"
	}
	if k.inWorkspace {
		row.TeamName = ""
		k.workspace = append(k.workspace, row)
	} else {
		k.team = append(k.team, row)
	}
	switch {
	case k.answerNoID:
		return "", nil
	case k.wrongID:
		return ksOther, nil
	}
	return id, nil
}
func (k *kindProvider) CreateProjectLabelGroup(_ context.Context, _, name string) (string, error) {
	return k.create(name, "", true)
}
func (k *kindProvider) CreateProjectLabelChoice(_ context.Context, _, parent, name string) (string, error) {
	return k.create(name, parent, false)
}

func kindTest(t *testing.T) (*Service, *kindAuthority, *kindProvider, bus.Request) {
	t.Helper()
	s, a, p, _ := testV2(t)
	a.actor, a.name, a.role = "", "", "owner"
	bound := true
	ka := &kindAuthority{authorityV2: a, setting: map[string]any{"revision": 3, "team_id": teamID, "scope": "team", "group_name": "Type", "values": []any{map[string]any{"name": "Engineering"}, map[string]any{"name": "Review"}}, "team_bound": bound}}
	s.cfg.Executor = ka
	k := &kindProvider{providerV2: p}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return k, nil }
	return s, ka, k, bus.Request{Workspace: "ws", Project: "prj", Args: map[string]any{}}
}

func confirmSeed(t *testing.T, s *Service, scope bus.Request) (KindSeedResult, error) {
	t.Helper()
	pre, e := s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1"})
	if e != nil || !pre.Preview {
		t.Fatal("preview:", pre, e)
	}
	return s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1", Confirm: true, ExpectedPlanSHA: pre.PlanSHA})
}

// A fresh team: the preview plans the group and each value as creates and
// sends nothing; the confirm makes the group first, then each value under
// it, proves each in the team's list, and records the IDs as seeded.
func TestKindSeedCreates(t *testing.T) {
	s, a, k, scope := kindTest(t)
	pre, e := s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1"})
	if e != nil || pre.Group.Action != "create" || pre.Values[0].Action != "create" || pre.Refused != 0 || len(k.creates) != 0 || !strings.HasPrefix(pre.Scope, "scope: team ") {
		t.Fatal(pre, e)
	}
	got, e := confirmSeed(t, s, scope)
	if e != nil || got.Stopped != "" || !got.Recorded || got.Group.LabelID != ksGroup || got.Values[1].LabelID != ksRev || got.Values[0].Outcome != "accepted" {
		t.Fatal(got, e)
	}
	if strings.Join(k.creates, ",") != "Type,Engineering,Review" {
		t.Fatal(k.creates)
	}
	rec := a.records[0]
	if rec["action"] != "seeded" || rec["proof"] != "team_list" || rec["group_label_id"] != ksGroup || rec["expected_revision"] != int64(3) {
		t.Fatal(rec)
	}
	// The same confirm again sends nothing: the ledger answers each label.
	if _, e = s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1", Confirm: true, ExpectedPlanSHA: pre.PlanSHA}); len(k.creates) != 3 {
		t.Fatal("a re-run sent again:", k.creates, e)
	}
}

// An existing live single-select group and its choices are adopted by ID;
// a missing value is created under it; other choices are left untouched.
func TestKindSeedAdopts(t *testing.T) {
	s, a, k, scope := kindTest(t)
	k.team = []linearprovider.ProjectLabelRow{
		{ID: ksGroup, Name: "Type", Group: true, GroupType: "singleSelect", TeamName: "Team"},
		{ID: ksEng, Name: "Engineering", ParentName: "Type", ParentGroupType: "singleSelect", TeamName: "Team"},
		{ID: ksOther, Name: "Legacy", ParentName: "Type", ParentGroupType: "singleSelect", TeamName: "Team"},
	}
	got, e := confirmSeed(t, s, scope)
	if e != nil || got.Group.Action != "adopt" || got.Values[0].Action != "adopt" || got.Values[1].Action != "create" || !got.Recorded || len(got.Untouched) != 1 || got.Untouched[0] != "Legacy" {
		t.Fatal(got, e)
	}
	if strings.Join(k.creates, ",") != "Review" || a.records[0]["action"] != "adopted" {
		t.Fatal(k.creates, a.records)
	}
}

// Refused before anything is sent (K4, L1, L2): a workspace label with a
// kind's name, a team label of another kind with it, a multi-select or
// archived group, two groups, a case-only difference; a team that left the
// binding; a session; no setting.
func TestKindSeedRefusals(t *testing.T) {
	ws := func(name string) []linearprovider.ProjectLabelRow {
		return []linearprovider.ProjectLabelRow{{ID: ksOther, Name: name}}
	}
	for name, tc := range map[string]struct {
		team, workspace []linearprovider.ProjectLabelRow
		want            string
	}{
		"workspace group name": {nil, ws("type"), "exists_in_workspace"},
		"workspace value name": {nil, ws("Review"), "exists_in_workspace"},
		"team plain label":     {[]linearprovider.ProjectLabelRow{{ID: ksOther, Name: "Engineering", TeamName: "Team"}}, nil, "isn't this kind's"},
		"multi-select":         {[]linearprovider.ProjectLabelRow{{ID: ksGroup, Name: "Type", Group: true, GroupType: "multiSelect"}}, nil, "multi-select"},
		"archived group":       {[]linearprovider.ProjectLabelRow{{ID: ksGroup, Name: "Type", Group: true, GroupType: "singleSelect", Archived: true}}, nil, "archived"},
		"two groups":           {[]linearprovider.ProjectLabelRow{{ID: ksGroup, Name: "Type", Group: true, GroupType: "singleSelect"}, {ID: ksOther, Name: "type", Group: true, GroupType: "singleSelect"}}, nil, "several"},
		"case only":            {[]linearprovider.ProjectLabelRow{{ID: ksGroup, Name: "TYPE", Group: true, GroupType: "singleSelect"}}, nil, "exactly that name"},
	} {
		s, _, k, scope := kindTest(t)
		k.team, k.workspace = tc.team, tc.workspace
		pre, e := s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1"})
		reasons := pre.Group.Reason
		for _, v := range pre.Values {
			reasons += " " + v.Reason
		}
		if e != nil || pre.Refused == 0 || !strings.Contains(reasons, tc.want) {
			t.Fatal(name, pre, e)
		}
		if _, e = s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1", Confirm: true, ExpectedPlanSHA: pre.PlanSHA}); e == nil || !strings.Contains(e.Error(), "nothing was sent") || len(k.creates) != 0 {
			t.Fatal(name, "confirmed:", e, k.creates)
		}
	}
	s, a, k, scope := kindTest(t)
	a.setting["team_bound"] = false
	if _, e := s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1"}); e == nil || !strings.Contains(e.Error(), "left the binding") {
		t.Fatal(e)
	}
	a.setting = map[string]any{"revision": 0, "scope": "team", "group_name": "Type", "values": []any{}}
	if _, e := s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1"}); e == nil || !strings.Contains(e.Error(), "no project kind is set") {
		t.Fatal(e)
	}
	scope.Session = "s"
	if _, e := s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1"}); e == nil || !strings.Contains(e.Error(), "a session can't") || len(k.creates) != 0 {
		t.Fatal(e)
	}
}

// The confirm carries the plan: none, or another, sends nothing.
func TestKindSeedConfirmHoldsThePlan(t *testing.T) {
	s, _, k, scope := kindTest(t)
	for want, sha := range map[string]string{"repeat the command": "", "plan changed": strings.Repeat("a", 64)} {
		if _, e := s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1", Confirm: true, ExpectedPlanSHA: sha}); e == nil || !strings.Contains(e.Error(), want) || len(k.creates) != 0 {
			t.Fatal(want, e)
		}
	}
}

// A create Linear refuses stops the seed, final for that label; one whose
// answer is lost is proven by the team's list; nothing is recorded until
// every label is proven.
func TestKindSeedOutcomes(t *testing.T) {
	s, a, k, scope := kindTest(t)
	k.createErr = &linearprovider.Error{Kind: "provider_rejected"}
	got, e := confirmSeed(t, s, scope)
	if e != nil || got.Stopped != "Type" || got.Group.Outcome != "refused" || len(a.records) != 0 {
		t.Fatal(got, e)
	}
	s, a, k, scope = kindTest(t)
	k.answerNoID = true
	got, e = confirmSeed(t, s, scope)
	if e != nil || !got.Recorded || got.Group.LabelID != ksGroup || got.Values[0].LabelID != ksEng {
		t.Fatal(got, e)
	}
	s, a, k, scope = kindTest(t)
	k.createErr = errors.New("connection reset")
	got, e = confirmSeed(t, s, scope)
	if e != nil || got.Stopped != "Type" || got.Group.Outcome != "uncertain" || len(a.records) != 0 {
		t.Fatal(got, e)
	}
}

// The proof (K3): a label made outside the team (in the workspace's list),
// or whose read shows another ID than the create answered, is uncertain.
func TestKindSeedProof(t *testing.T) {
	for name, set := range map[string]func(*kindProvider){
		"in the workspace": func(k *kindProvider) { k.inWorkspace = true },
		"another ID":       func(k *kindProvider) { k.wrongID = true },
	} {
		s, a, k, scope := kindTest(t)
		set(k)
		got, e := confirmSeed(t, s, scope)
		if e != nil || got.Stopped != "Type" || got.Group.Outcome != "uncertain" || len(a.records) != 0 {
			t.Fatal(name, got, e)
		}
	}
}

// Adopting: a choice is the group's only by its parent; a value named only
// by another group's choice is refused as taken; a case-only difference is
// refused (review L2).
func TestKindSeedAdoptChecksTheChoices(t *testing.T) {
	group := linearprovider.ProjectLabelRow{ID: ksGroup, Name: "Type", Group: true, GroupType: "singleSelect", TeamName: "Team"}
	for name, tc := range map[string]struct {
		choice linearprovider.ProjectLabelRow
		want   string
	}{
		"another group's": {linearprovider.ProjectLabelRow{ID: ksEng, Name: "Engineering", ParentName: "Area", ParentGroupType: "singleSelect", TeamName: "Team"}, "isn't this kind's"},
		"case only":       {linearprovider.ProjectLabelRow{ID: ksEng, Name: "engineering", ParentName: "Type", ParentGroupType: "singleSelect", TeamName: "Team"}, "exactly that name"},
	} {
		s, _, k, scope := kindTest(t)
		k.team = []linearprovider.ProjectLabelRow{group, tc.choice}
		pre, e := s.KindSeed(context.Background(), scope, KindSeedInput{OperationID: "seed-1"})
		if e != nil || pre.Values[0].Action != "refused" || !strings.Contains(pre.Values[0].Reason, tc.want) {
			t.Fatal(name, pre, e)
		}
	}
}
