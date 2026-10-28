package linearboard

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	kaProjA    = projectA
	kaProjB    = projectB
	kaCustomer = "c2c2c2c2-0000-4000-8000-0000000000c1"
	kaForeign  = "c2c2c2c2-0000-4000-8000-0000000000f1"
)

// applyProvider plays a board: each project's labels by ID, named through
// the label rows; carriers come from the same map. save replaces a set.
type applyProvider struct {
	*kindProvider
	labels  map[string][]string
	foreign map[string]string // label ID → name, for labels the team read doesn't list
	// hidden labels are on a project but left out of its read by name.
	hidden map[string]bool
	saves  [][]string
	// beforeSave and afterSave change the board around the write.
	beforeSave, afterSave func()
	reads                 int
}

func (a *applyProvider) ProjectKindApplyReason() string { return "" }
func (a *applyProvider) name(id string) string {
	for _, r := range append(append([]linearprovider.ProjectLabelRow{}, a.team...), a.workspace...) {
		if r.ID == id {
			return r.Name
		}
	}
	return a.foreign[id]
}
func (a *applyProvider) ProjectLabelNames(_ context.Context, project string) ([]string, error) {
	a.reads++
	names := []string{}
	for _, id := range a.labels[project] {
		if !a.hidden[id] {
			names = append(names, a.name(id))
		}
	}
	return names, nil
}
func (a *applyProvider) ProjectsWithLabel(_ context.Context, label string) ([]string, error) {
	a.reads++
	var out []string
	for p, ids := range a.labels {
		if slices.Contains(ids, label) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out, nil
}
func (a *applyProvider) SetProjectLabels(_ context.Context, project string, labels []string) error {
	if a.beforeSave != nil {
		a.beforeSave()
	}
	a.saves = append(a.saves, append([]string{project}, labels...))
	a.labels[project] = append([]string{}, labels...)
	if a.afterSave != nil {
		a.afterSave()
	}
	return nil
}

// applyTestKind: a seeded kind Type {Engineering, Review} on the team; two
// bound projects, A carrying Engineering and a team label Customer, B none.
func applyTestKind(t *testing.T) (*Service, *applyProvider, bus.Request) {
	t.Helper()
	s, a, k, scope := kindTest(t)
	a.setting = map[string]any{"revision": 4, "team_id": teamID, "scope": "team", "group_name": "Type", "group_label_id": ksGroup,
		"values": []any{map[string]any{"name": "Engineering", "label_id": ksEng}, map[string]any{"name": "Review", "label_id": ksRev}}, "team_bound": true}
	k.team = []linearprovider.ProjectLabelRow{
		{ID: ksGroup, Name: "Type", Group: true, GroupType: "singleSelect", TeamName: "Team"},
		{ID: ksEng, Name: "Engineering", ParentName: "Type", ParentGroupType: "singleSelect", TeamName: "Team"},
		{ID: ksRev, Name: "Review", ParentName: "Type", ParentGroupType: "singleSelect", TeamName: "Team"},
		{ID: kaCustomer, Name: "Customer", TeamName: "Team"},
	}
	ap := &applyProvider{kindProvider: k, labels: map[string][]string{kaProjA: {ksEng, kaCustomer}, kaProjB: {}}, foreign: map[string]string{}}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return ap, nil }
	return s, ap, scope
}

func kindFile(pairs ...string) []byte {
	parts := []string{}
	for i := 0; i < len(pairs); i += 2 {
		parts = append(parts, `{"project":"`+pairs[i]+`","kind":"`+pairs[i+1]+`"}`)
	}
	return []byte(`{"projects":[` + strings.Join(parts, ",") + `]}`)
}

func applyConfirm(t *testing.T, s *Service, scope bus.Request, in KindApplyInput) (KindApplyResult, error) {
	t.Helper()
	pre := in
	pre.Confirm, pre.Canary, pre.SkipCanary = false, false, false
	got, e := s.KindApply(context.Background(), scope, pre)
	if e != nil {
		t.Fatal("preview:", e)
	}
	in.Confirm, in.ExpectedPlanSHA = true, got.PlanSHA
	return s.KindApply(context.Background(), scope, in)
}

// The preview reads each project's labels by ID, plans the write (remove
// only this group's other value, add the kind, keep the rest), counts its
// reads and sends nothing. The first confirm must be a canary of one
// project; then the full apply writes the rest, each read back by ID.
func TestKindApplyCanaryThenAll(t *testing.T) {
	s, ap, scope := applyTestKind(t)
	file := kindFile(kaProjA, "Review", kaProjB, "Engineering")
	pre, e := s.KindApply(context.Background(), scope, KindApplyInput{File: file, OperationID: "apply-1"})
	if e != nil || pre.Refused != 0 || pre.Reads == 0 || len(ap.saves) != 0 {
		t.Fatal(pre, e)
	}
	a := pre.Entries[0]
	if a.Action != "set" || a.Add != ksRev || !reflect.DeepEqual(a.Remove, []string{ksEng}) || !reflect.DeepEqual(a.Labels, sorted(ksEng, kaCustomer)) {
		t.Fatal(a)
	}
	if _, e = applyConfirm(t, s, scope, KindApplyInput{File: file, OperationID: "apply-1"}); e == nil || !strings.Contains(e.Error(), "canary") || len(ap.saves) != 0 {
		t.Fatal("a full apply ran without a canary:", e)
	}
	got, e := applyConfirm(t, s, scope, KindApplyInput{File: file, OperationID: "apply-1", Canary: true})
	if e != nil || !got.Canary || got.Entries[0].Outcome != "accepted" || got.Entries[1].Outcome != "" || len(ap.saves) != 1 {
		t.Fatal(got, e)
	}
	if !reflect.DeepEqual(ap.labels[kaProjA], sorted(kaCustomer, ksRev)) {
		t.Fatal(ap.labels[kaProjA])
	}
	// The full apply: A is already done (its preview now says unchanged).
	got, e = applyConfirm(t, s, scope, KindApplyInput{File: file, OperationID: "apply-1"})
	if e != nil || got.Stopped != "" || got.Entries[0].Action != "unchanged" || got.Entries[1].Outcome != "accepted" || len(ap.saves) != 2 {
		t.Fatal(got, e)
	}
	if !reflect.DeepEqual(ap.labels[kaProjB], []string{ksEng}) {
		t.Fatal(ap.labels[kaProjB])
	}
}

func sorted(ids ...string) []string {
	out := append([]string{}, ids...)
	slices.Sort(out)
	return out
}

// Refused before anything is sent: a project outside the binding, an
// unknown kind, a label of another team on the project (it can't be
// proven by ID), an unseeded setting, a session; a stale or missing plan.
func TestKindApplyRefusals(t *testing.T) {
	s, ap, scope := applyTestKind(t)
	ap.labels[kaProjB] = []string{kaForeign}
	ap.foreign[kaForeign] = "Other team's"
	outside := "c2c2c2c2-0000-4000-8000-0000000000ee"
	pre, e := s.KindApply(context.Background(), scope, KindApplyInput{File: kindFile(outside, "Review", kaProjA, "Testing", kaProjB, "Review"), OperationID: "apply-1"})
	if e != nil || pre.Refused != 3 {
		t.Fatal(pre, e)
	}
	for i, want := range []string{"isn't in this project's binding", "not a kind", "can't be proven by ID"} {
		if !strings.Contains(pre.Entries[i].Reason, want) {
			t.Fatal(i, pre.Entries[i])
		}
	}
	if _, e = s.KindApply(context.Background(), scope, KindApplyInput{File: kindFile(kaProjB, "Review"), OperationID: "apply-1", Confirm: true, ExpectedPlanSHA: pre.PlanSHA, Canary: true}); e == nil || len(ap.saves) != 0 {
		t.Fatal(e)
	}
	ap.labels[kaProjB] = []string{}
	for want, sha := range map[string]string{"repeat the command": "", "plan changed": strings.Repeat("a", 64)} {
		if _, e = s.KindApply(context.Background(), scope, KindApplyInput{File: kindFile(kaProjB, "Review"), OperationID: "apply-1", Confirm: true, ExpectedPlanSHA: sha, Canary: true}); e == nil || !strings.Contains(e.Error(), want) || len(ap.saves) != 0 {
			t.Fatal(want, e)
		}
	}
	for name, file := range map[string]string{"empty": `{"projects":[]}`, "twice": string(kindFile(kaProjB, "Review", kaProjB, "Review")), "extra key": `{"projects":[],"x":1}`} {
		if _, e = s.KindApply(context.Background(), scope, KindApplyInput{File: []byte(file), OperationID: "apply-1"}); e == nil {
			t.Fatal(name)
		}
	}
	scope.Session = "s"
	if _, e = s.KindApply(context.Background(), scope, KindApplyInput{File: kindFile(kaProjB, "Review"), OperationID: "apply-1"}); e == nil || !strings.Contains(e.Error(), "a session can't") {
		t.Fatal(e)
	}
}

// K2 and the re-read guard: a change between the preview and the confirm
// refuses before sending; one between the save and its read-back is
// uncertain ("changed"), names the project and is never sent again.
func TestKindApplyConcurrentEdits(t *testing.T) {
	s, ap, scope := applyTestKind(t)
	file := kindFile(kaProjB, "Review")
	pre, e := s.KindApply(context.Background(), scope, KindApplyInput{File: file, OperationID: "apply-1"})
	if e != nil {
		t.Fatal(e)
	}
	ap.labels[kaProjB] = []string{kaCustomer}
	got, e := s.KindApply(context.Background(), scope, KindApplyInput{File: file, OperationID: "apply-1", Confirm: true, ExpectedPlanSHA: pre.PlanSHA, Canary: true})
	if e == nil || !strings.Contains(e.Error(), "plan changed") || len(ap.saves) != 0 {
		t.Fatal(got, e)
	}
	ap.labels[kaProjB] = []string{}
	ap.afterSave = func() { ap.labels[kaProjB] = append(ap.labels[kaProjB], kaCustomer) }
	got, e = applyConfirm(t, s, scope, KindApplyInput{File: file, OperationID: "apply-2", Canary: true})
	if e != nil || got.Stopped != kaProjB || got.Entries[0].Outcome != "uncertain" || !strings.Contains(got.Entries[0].Reason, "changed") || !strings.Contains(got.Entries[0].Reason, kaProjB) {
		t.Fatal(got, e)
	}
	ap.afterSave = nil
	saves := len(ap.saves)
	in := KindApplyInput{File: file, OperationID: "apply-2", Confirm: true, Canary: true}
	again, _ := s.KindApply(context.Background(), scope, KindApplyInput{File: file, OperationID: "apply-2"})
	in.ExpectedPlanSHA = again.PlanSHA
	if _, _ = s.KindApply(context.Background(), scope, in); len(ap.saves) != saves {
		t.Fatal("an uncertain write was sent again")
	}
}

// --skip-canary needs a canary of these same label IDs; it doesn't combine
// with --canary.
func TestKindApplySkipCanary(t *testing.T) {
	s, ap, scope := applyTestKind(t)
	file := kindFile(kaProjB, "Review")
	if _, e := applyConfirm(t, s, scope, KindApplyInput{File: file, OperationID: "apply-1", SkipCanary: true}); e == nil || !strings.Contains(e.Error(), "--skip-canary needs") || len(ap.saves) != 0 {
		t.Fatal(e)
	}
	if _, e := applyConfirm(t, s, scope, KindApplyInput{File: file, OperationID: "apply-1", Canary: true, SkipCanary: true}); e == nil || len(ap.saves) != 0 {
		t.Fatal(e)
	}
	if _, e := applyConfirm(t, s, scope, KindApplyInput{File: file, OperationID: "apply-1", Canary: true}); e != nil {
		t.Fatal(e)
	}
	ap.labels[kaProjA] = []string{}
	if got, e := applyConfirm(t, s, scope, KindApplyInput{File: kindFile(kaProjA, "Engineering"), OperationID: "apply-3", SkipCanary: true}); e != nil || got.Entries[0].Outcome != "accepted" {
		t.Fatal(got, e)
	}
}

// K1: two labels of one name on a project (a team one and a workspace one)
// can't be told apart by name: refused.
func TestKindApplyRefusesTwoLabelsOfOneName(t *testing.T) {
	s, ap, scope := applyTestKind(t)
	ap.workspace = []linearprovider.ProjectLabelRow{{ID: kaForeign, Name: "Customer"}}
	ap.labels[kaProjB] = []string{kaCustomer, kaForeign}
	pre, e := s.KindApply(context.Background(), scope, KindApplyInput{File: kindFile(kaProjB, "Review"), OperationID: "apply-1"})
	if e != nil || pre.Refused != 1 || !strings.Contains(pre.Entries[0].Reason, "can't be proven by ID") {
		t.Fatal(pre, e)
	}
	// Confirming that very plan is refused, nothing sent.
	if _, e = s.KindApply(context.Background(), scope, KindApplyInput{File: kindFile(kaProjB, "Review"), OperationID: "apply-1", Confirm: true, ExpectedPlanSHA: pre.PlanSHA, Canary: true}); e == nil || !strings.Contains(e.Error(), "refused") || len(ap.saves) != 0 {
		t.Fatal(e)
	}
}

// A canary proves label IDs: after the setting's revision moves with the
// same IDs, --skip-canary goes on; a plain confirm needs a new canary.
func TestKindApplySkipCanaryAcrossRevisions(t *testing.T) {
	s, ap, scope := applyTestKind(t)
	a := s.cfg.Executor.(*kindAuthority)
	if _, e := applyConfirm(t, s, scope, KindApplyInput{File: kindFile(kaProjB, "Review"), OperationID: "apply-1", Canary: true}); e != nil {
		t.Fatal(e)
	}
	a.setting["revision"] = 5
	ap.labels[kaProjA] = []string{}
	if _, e := applyConfirm(t, s, scope, KindApplyInput{File: kindFile(kaProjA, "Engineering"), OperationID: "apply-2"}); e == nil || !strings.Contains(e.Error(), "canary") {
		t.Fatal(e)
	}
	if got, e := applyConfirm(t, s, scope, KindApplyInput{File: kindFile(kaProjA, "Engineering"), OperationID: "apply-2", SkipCanary: true}); e != nil || got.Entries[0].Outcome != "accepted" {
		t.Fatal(got, e)
	}
}

// Review M1: a retired label on the project that get_project doesn't name
// (but the label's carriers show) would be dropped by the save: refused.
func TestKindApplyRefusesAnUnshownRetiredLabel(t *testing.T) {
	s, ap, scope := applyTestKind(t)
	ap.team = append(ap.team, linearprovider.ProjectLabelRow{ID: kaForeign, Name: "Old kind", TeamName: "Team", Archived: true})
	ap.labels[kaProjB] = []string{kaForeign}
	ap.hidden = map[string]bool{kaForeign: true}
	pre, e := s.KindApply(context.Background(), scope, KindApplyInput{File: kindFile(kaProjB, "Review"), OperationID: "apply-1"})
	if e != nil || pre.Refused != 1 || !strings.Contains(pre.Entries[0].Reason, "archived or retired label") || len(ap.saves) != 0 {
		t.Fatal(pre, e)
	}
}
