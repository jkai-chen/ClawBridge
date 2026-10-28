package linearboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const vwHand = "c9c9c9c9-c9c9-4c9c-8c9c-c9c9c9c9c9c9"

// staticViews lists the same views on every call.
type staticViews struct {
	*providerV2
	views []linearprovider.View
}

func (v staticViews) Views(_ context.Context, team, kind string, _ linearprovider.PageOptions) (linearprovider.ViewPage, error) {
	var out []linearprovider.View
	for _, x := range v.views {
		if x.TeamID == team && (kind == "" || x.Kind == kind) {
			out = append(out, x)
		}
	}
	return linearprovider.ViewPage{Page: linearprovider.Page[linearprovider.View]{Items: out, Complete: true}}, nil
}

func applyTest(t *testing.T, views ...linearprovider.View) (*Service, *viewAuthority, bus.Request) {
	t.Helper()
	s, a, p, scope := viewWriteTest(t)
	v := staticViews{p, views}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return v, nil }
	return s, a, scope
}

func viewSet(entries ...map[string]any) []byte {
	raw, _ := json.Marshal(map[string]any{"views": entries})
	return raw
}

func entry(name, filter string) map[string]any {
	return map[string]any{"team": teamID, "name": name, "filter": json.RawMessage(filter)}
}

// The preview plans each entry, sends nothing, and gives each its own
// operation ID; a stored view with the same filter is unchanged, with
// another filter an update; a free name is a create.
func TestViewApplyPlans(t *testing.T) {
	s, a, scope := applyTest(t, linearprovider.View{ID: vwView, Name: "Urgent bugs", Kind: "issue", TeamID: teamID})
	storeView(t, a, `{"priority":{"eq":1}}`, true)
	// Linear still has the stored view as ClawBridge wrote it.
	key := &fakeKey{last: KeyView{ID: vwView, Name: "Urgent bugs", TeamID: teamID, Shared: true, Filter: `{"priority":{"eq":1}}`}}
	file := viewSet(entry("Urgent bugs", `{"priority":{"eq":1}}`), entry("Open bugs", `{"labels":{"some":{"id":{"eq":"`+vwLabel+`"}}}}`))
	got, e := s.ViewApply(context.Background(), scope, ViewApplyInput{File: file, OperationID: "set-1"}, key)
	if e != nil || !got.Preview || got.Refused != 0 || len(got.Entries) != 2 {
		t.Fatal(got, e)
	}
	if got.Entries[0].Action != "unchanged" || got.Entries[1].Action != "create" || !strings.HasPrefix(got.Entries[1].Scope, "scope: team ") {
		t.Fatal(got.Entries)
	}
	if got.Entries[0].OperationID == got.Entries[1].OperationID || !strings.HasPrefix(got.Entries[1].OperationID, "set-1-") {
		t.Fatal(got.Entries)
	}
	if len(key.creates)+len(key.updates) != 0 {
		t.Fatal("a preview sent")
	}
	file = viewSet(entry("Urgent bugs", `{"priority":{"in":[1,2]}}`))
	if got, e = s.ViewApply(context.Background(), scope, ViewApplyInput{File: file, OperationID: "set-1"}, key); e != nil || got.Entries[0].Action != "update" {
		t.Fatal(got, e)
	}
	// Confirmed: the create and the update are sent once each; a re-run
	// sends nothing more.
	file = viewSet(entry("Urgent bugs", `{"priority":{"in":[1,2]}}`), entry("Open bugs", `{"labels":{"some":{"id":{"eq":"`+vwLabel+`"}}}}`))
	in := ViewApplyInput{File: file, OperationID: "set-2"}
	got, e = confirmApply(t, s, scope, in, key)
	if e != nil || got.Preview || got.Stopped != "" || got.Entries[0].Result == nil || got.Entries[1].Result == nil {
		t.Fatal(got, e)
	}
	// The same confirm again sends nothing more: the ledger answers each
	// entry (against a real store the plan has changed, and it's refused).
	in.Confirm, in.ExpectedPlanSHA = true, got.PlanSHA
	if again, e := s.ViewApply(context.Background(), scope, in, key); e != nil && !strings.Contains(e.Error(), "plan changed") {
		t.Fatal(again, e)
	}
	if len(key.creates) != 1 || len(key.updates) != 1 {
		t.Fatal(key.creates, key.updates)
	}
}

// A name a view made in Linear holds is refused without --adopt; with it,
// and exactly one such view, it's an adoption whose preview shows the view
// as it is now (A2) and the keys agents lose (A3); two such views refuse
// (A1). A refused entry refuses the whole confirm.
func TestViewApplyAdoption(t *testing.T) {
	hand := linearprovider.View{ID: vwHand, Name: "Triage", Kind: "issue", TeamID: teamID}
	s, a, scope := applyTest(t, hand)
	old := `{"or":[{"priority":{"eq":1}}]}`
	key := &fakeKey{back: func(KeyView) (KeyView, error) {
		return KeyView{ID: vwHand, Name: "Triage", TeamID: teamID, Shared: true, Filter: old, CreatorID: "u-1", CreatorName: "Ada", UpdatedAt: "2026-10-20T10:00:00.000Z"}, nil
	}}
	file := viewSet(entry("Triage", `{"priority":{"eq":1}}`))
	got, e := s.ViewApply(context.Background(), scope, ViewApplyInput{File: file, OperationID: "set-3"}, key)
	if e != nil || got.Refused != 1 || !strings.Contains(got.Entries[0].Reason, `--adopt "Triage"`) {
		t.Fatal(got, e)
	}
	if _, e = s.ViewApply(context.Background(), scope, ViewApplyInput{File: file, OperationID: "set-3", Confirm: true}, key); e == nil || !strings.Contains(e.Error(), "nothing was sent") || len(key.updates) != 0 {
		t.Fatal(e)
	}
	got, e = s.ViewApply(context.Background(), scope, ViewApplyInput{File: file, OperationID: "set-3", Adopt: []string{"triage"}}, key)
	if e != nil || got.Refused != 0 || got.Entries[0].Action != "adopt" || got.Entries[0].ViewID != vwHand {
		t.Fatal(got, e)
	}
	r := got.Entries[0].Replaces
	if r == nil || r.Filter != old || r.CreatorName != "Ada" || r.UpdatedAt != "2026-10-20T10:00:00.000Z" || len(r.GoesAway) != 1 || r.GoesAway[0] != "or" || r.FilterSHA != mustFilterSHA(t, old) {
		t.Fatal(r)
	}
	// Confirmed: one update of the adopted view, read back, recorded as
	// an adoption with the old filter's digest.
	key.back = func(v KeyView) (KeyView, error) {
		if len(key.updates) == 0 {
			return KeyView{ID: vwHand, Name: "Triage", TeamID: teamID, Shared: true, Filter: old}, nil
		}
		return v, nil
	}
	got, e = confirmApply(t, s, scope, ViewApplyInput{File: file, OperationID: "set-3", Adopt: []string{"Triage"}}, key)
	if e != nil || got.Stopped != "" || got.Entries[0].Result == nil || got.Entries[0].Result.Outcome != "accepted" || len(key.updates) != 1 || len(key.creates) != 0 {
		t.Fatal(got, e, key.updates)
	}
	rec := a.records[len(a.records)-1]
	if rec["action"] != "adopt" || rec["view_id"] != vwHand || rec["old_filter_sha256"] != mustFilterSHA(t, old) {
		t.Fatal(rec)
	}

	// Two views by that name: an adoption needs exactly one.
	s, _, scope = applyTest(t, hand, linearprovider.View{ID: vwView, Name: "triage", Kind: "issue", TeamID: teamID})
	if got, e = s.ViewApply(context.Background(), scope, ViewApplyInput{File: file, OperationID: "set-4", Adopt: []string{"Triage"}}, key); e != nil || got.Refused != 1 || !strings.Contains(got.Entries[0].Reason, "exactly one") {
		t.Fatal(got, e)
	}
}

// A5: a view that is gone by the time it's read is refused, and the
// adoption never creates one, at the preview and at the send.
func TestViewApplyAdoptionNeverCreates(t *testing.T) {
	s, _, scope := applyTest(t, linearprovider.View{ID: vwHand, Name: "Triage", Kind: "issue", TeamID: teamID})
	key := &fakeKey{back: func(KeyView) (KeyView, error) { return KeyView{}, fmt.Errorf("%w: gone", ErrKeyNotFound) }}
	file := viewSet(entry("Triage", `{"priority":{"eq":1}}`))
	got, e := s.ViewApply(context.Background(), scope, ViewApplyInput{File: file, OperationID: "set-5", Adopt: []string{"Triage"}}, key)
	if e != nil || got.Refused != 1 || !strings.Contains(got.Entries[0].Reason, "never creates") {
		t.Fatal(got, e)
	}
	// Gone between the read and the send: Linear's not found on the update
	// is a refusal, and nothing is created.
	reads := 0
	key = &fakeKey{back: func(v KeyView) (KeyView, error) {
		reads++
		return KeyView{ID: vwHand, Name: "Triage", TeamID: teamID, Shared: true, Filter: `{}`}, nil
	}, err: nil}
	gone := &goneOnUpdate{fakeKey: key}
	got, e = confirmApply(t, s, scope, ViewApplyInput{File: file, OperationID: "set-6", Adopt: []string{"Triage"}}, gone)
	if e != nil || got.Stopped != "Triage" || !strings.Contains(got.Entries[0].Reason, "never creates") || len(gone.creates) != 0 {
		t.Fatal(got, e)
	}
}

type goneOnUpdate struct{ *fakeKey }

func (g *goneOnUpdate) UpdateView(_ context.Context, id, _ string, _ json.RawMessage) (KeyView, error) {
	g.updates = append(g.updates, id)
	return KeyView{}, fmt.Errorf("%w: gone", ErrKeyNotFound)
}

// The file is read strictly.
func TestViewApplyFileRefusals(t *testing.T) {
	s, _, scope := applyTest(t)
	many := make([]map[string]any, 51)
	for i := range many {
		many[i] = entry(fmt.Sprintf("v%d", i), `{"priority":{"eq":1}}`)
	}
	for name, in := range map[string]ViewApplyInput{
		"unknown key": {File: []byte(`{"views":[{"team":"` + teamID + `","name":"a","filter":{},"color":"red"}]}`), OperationID: "x"},
		"empty":       {File: []byte(`{"views":[]}`), OperationID: "x"},
		"too many":    {File: viewSet(many...), OperationID: "x"},
		"duplicate":   {File: viewSet(entry("A", `{}`), entry("a", `{}`)), OperationID: "x"},
		"bad team":    {File: []byte(`{"views":[{"team":"ENG","name":"a","filter":{}}]}`), OperationID: "x"},
		"no filter":   {File: []byte(`{"views":[{"team":"` + teamID + `","name":"a"}]}`), OperationID: "x"},
		"adopt name":  {File: viewSet(entry("A", `{}`)), OperationID: "x", Adopt: []string{"B"}},
		"op id":       {File: viewSet(entry("A", `{}`)), OperationID: strings.Repeat("x", 101)},
	} {
		if _, e := s.ViewApply(context.Background(), scope, in, &fakeKey{}); e == nil {
			t.Fatal("accepted", name)
		}
	}
	scope.Session = "ses"
	if _, e := s.ViewApply(context.Background(), scope, ViewApplyInput{File: viewSet(entry("A", `{"priority":{"eq":1}}`)), OperationID: "x"}, &fakeKey{}); e == nil || !strings.Contains(e.Error(), "a session can't") {
		t.Fatal(e)
	}
}

// A1: the view to adopt must be a shared view of the entry's team, as the
// owner's key reads it now.
func TestViewApplyAdoptionTeamAndSharing(t *testing.T) {
	for name, back := range map[string]KeyView{
		"other team": {ID: vwHand, Name: "Triage", TeamID: otherTeamID, Shared: true, Filter: `{}`},
		"private":    {ID: vwHand, Name: "Triage", TeamID: teamID, Shared: false, Filter: `{}`},
	} {
		s, _, scope := applyTest(t, linearprovider.View{ID: vwHand, Name: "Triage", Kind: "issue", TeamID: teamID})
		b := back
		key := &fakeKey{back: func(KeyView) (KeyView, error) { return b, nil }}
		got, e := s.ViewApply(context.Background(), scope, ViewApplyInput{File: viewSet(entry("Triage", `{"priority":{"eq":1}}`)), OperationID: "set-7", Adopt: []string{"Triage"}}, key)
		if e != nil || got.Refused != 1 || !strings.Contains(got.Entries[0].Reason, "isn't a shared view of this team") {
			t.Fatal(name, got, e)
		}
	}
}

// A confirmed apply leaves an unchanged entry alone and goes on.
func TestViewApplySkipsUnchanged(t *testing.T) {
	s, a, scope := applyTest(t, linearprovider.View{ID: vwView, Name: "Urgent bugs", Kind: "issue", TeamID: teamID})
	storeView(t, a, `{"priority":{"eq":1}}`, true)
	key := &fakeKey{}
	file := viewSet(entry("Urgent bugs", `{"priority":{"eq":1}}`), entry("Open bugs", `{"priority":{"eq":2}}`))
	got, e := confirmApply(t, s, scope, ViewApplyInput{File: file, OperationID: "set-8"}, key)
	if e != nil || got.Stopped != "" || got.Entries[0].Result != nil || got.Entries[1].Result == nil || len(key.updates) != 0 || len(key.creates) != 1 {
		t.Fatal(got, e, key.updates, key.creates)
	}
}

// confirmApply previews in, then confirms it with the preview's plan.
func confirmApply(t *testing.T, s *Service, scope bus.Request, in ViewApplyInput, key ViewKey) (ViewApplyResult, error) {
	t.Helper()
	in.Confirm = false
	pre, e := s.ViewApply(context.Background(), scope, in, key)
	if e != nil || pre.PlanSHA == "" {
		t.Fatal("preview:", pre, e)
	}
	in.Confirm, in.ExpectedPlanSHA = true, pre.PlanSHA
	return s.ViewApply(context.Background(), scope, in, key)
}

// A confirm applies only the plan its preview showed: without the
// preview's plan_sha256, or with another, nothing is sent.
func TestViewApplyConfirmHoldsThePlan(t *testing.T) {
	s, _, scope := applyTest(t)
	key := &fakeKey{}
	file := viewSet(entry("Open bugs", `{"priority":{"eq":2}}`))
	for want, sha := range map[string]string{"repeat the command the preview printed": "", "plan changed": strings.Repeat("a", 64)} {
		got, e := s.ViewApply(context.Background(), scope, ViewApplyInput{File: file, OperationID: "set-9", Confirm: true, ExpectedPlanSHA: sha}, key)
		if e == nil || !strings.Contains(e.Error(), want) || len(key.creates) != 0 || got.PlanSHA == "" {
			t.Fatal(want, got, e)
		}
	}
}
