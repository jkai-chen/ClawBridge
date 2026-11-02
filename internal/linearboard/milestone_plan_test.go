package linearboard

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	mpAlpha = "f7000000-0000-4000-8000-0000000000a1"
	mpBeta  = "f7000000-0000-4000-8000-0000000000b1"
	mpNew   = "f7000000-0000-4000-8000-0000000000c1"
	mpOther = "f7000000-0000-4000-8000-0000000000d1"
	mpDup   = "f7000000-0000-4000-8000-0000000000e1"
)

// msAuthority is the server side of a milestone plan, as the fake authority
// answers it.
type msAuthority struct {
	*adminAuthority
	checks, reserves, settles []map[string]any
	refuse                    error
	unproven                  bool
	replay                    map[string]any
	by, planLane, planTeam    string
	afterReserve              func()
}

func (a *msAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "linear_write_charge" {
		return budgetAnswer(r)
	}
	switch r.Operation {
	case "milestone_plan_reserve":
		confirm := r.Args["confirm"] == true
		if confirm {
			a.reserves = append(a.reserves, r.Args)
		} else {
			a.checks = append(a.checks, r.Args)
		}
		if a.refuse != nil {
			return bus.Result{}, a.refuse
		}
		if !confirm {
			return bus.Result{Source: "local-authority", Data: map[string]any{"preview": true, "by": a.by, "lane": a.planLane, "team_id": a.planTeam, "proven": !a.unproven}}, nil
		}
		if a.unproven {
			return bus.Result{}, bus.PublicError("forbidden", "milestone_plan_unproven: this server creates and updates no milestone until its operator sets serve --milestone-plan-proven after the live sitting; the preview works")
		}
		if a.afterReserve != nil {
			a.afterReserve()
		}
		if a.replay != nil {
			return bus.Result{Source: "local-authority", Data: a.replay}, nil
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": "reserved", "by": a.by, "lane": a.planLane, "replay": false}}, nil
	case "milestone_plan_settle":
		a.settles = append(a.settles, r.Args)
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": r.Args["outcome"]}}, nil
	}
	return a.adminAuthority.Execute(ctx, r)
}

// msPlanFake holds projectA's milestones as list_milestones gives them; a
// save changes them unless ignore is set, then runs onSave. readErr fails
// the lists from the third on (the confirm's read-back after a preview and
// the confirm's own read).
type msPlanFake struct {
	*planFake
	ms      []linearprovider.Milestone
	reason  string
	saves   []linearprovider.MilestonePlan
	saveErr error
	ignore  bool
	noID    bool
	// savedID, when set, is the ID the save answers with for a create.
	savedID string
	onSave  func(*msPlanFake)
	lists   int
	readErr error
}

func (p *msPlanFake) MilestonePlanReason(bool, []string) string { return p.reason }
func (p *msPlanFake) ListMilestones(_ context.Context, project string) ([]linearprovider.Milestone, error) {
	p.lists++
	if p.lists > 2 && p.readErr != nil {
		return nil, p.readErr
	}
	return append([]linearprovider.Milestone{}, p.ms...), nil
}
func (p *msPlanFake) SaveMilestone(_ context.Context, plan linearprovider.MilestonePlan) (string, error) {
	p.saves = append(p.saves, plan)
	id := ""
	if !p.ignore {
		if plan.ID == "" {
			m := linearprovider.Milestone{ID: mpNew, Name: *plan.Name, TargetDateKnown: true}
			if plan.TargetDate.Present {
				m.TargetDate = plan.TargetDate.Value
			}
			p.ms = append(p.ms, m)
			id = mpNew
		}
		for i := range p.ms {
			if p.ms[i].ID != plan.ID {
				continue
			}
			if plan.Name != nil {
				p.ms[i].Name = *plan.Name
			}
			if plan.TargetDate.Present {
				p.ms[i].TargetDate, p.ms[i].TargetDateKnown = plan.TargetDate.Value, true
			}
		}
	}
	if p.savedID != "" {
		id = p.savedID
	}
	if p.noID {
		id = ""
	}
	if p.onSave != nil {
		p.onSave(p)
	}
	return id, p.saveErr
}

func msDay(s string) *string { return &s }

func msPlanTest(t *testing.T) (*Service, *msAuthority, *msPlanFake, bus.Request) {
	t.Helper()
	s, a, p, scope := planTest(t)
	auth := &msAuthority{adminAuthority: a.adminAuthority, by: "owner"}
	order := 1.0
	fake := &msPlanFake{planFake: p, ms: []linearprovider.Milestone{
		{ID: mpAlpha, Name: "Alpha", TargetDate: msDay("2026-11-30"), TargetDateKnown: true, SortOrder: &order},
		{ID: mpBeta, Name: "Beta", TargetDateKnown: true},
	}}
	s.cfg.Executor = auth
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return fake, nil }
	return s, auth, fake, scope
}

func mpIn(in MilestonePlanInput) MilestonePlanInput {
	in.ProjectID, in.OperationID = projectA, "ms-1"
	return in
}

func msPreview(t *testing.T, s *Service, scope bus.Request, in MilestonePlanInput) MilestonePlanResult {
	t.Helper()
	r, e := s.SetMilestonePlan(context.Background(), scope, mpIn(in))
	if e != nil || r.Outcome != "preview" {
		t.Fatal(r, e)
	}
	return r
}

func msConfirm(s *Service, scope bus.Request, in MilestonePlanInput, sha string) (MilestonePlanResult, error) {
	in = mpIn(in)
	in.Confirm, in.ExpectedPlanSHA256 = true, sha
	return s.SetMilestonePlan(context.Background(), scope, in)
}

func msName(n string) *string { return &n }

// A create previews current → planned and who confirms, asks the server
// without confirm, and sends nothing; the confirm reserves with the same
// digest and the name's hash (N1), sends only the given fields, reads the
// milestones back before settling, and settles accepted with the new ID.
func TestMilestoneCreatePreviewThenConfirm(t *testing.T) {
	s, a, p, scope := msPlanTest(t)
	in := MilestonePlanInput{Name: msName("  Gamma "), TargetDate: dateSet("2026-12-15")}
	r := msPreview(t, s, scope, in)
	want := []MilestonePlanChange{{Field: "name", Current: "-", Planned: "Gamma"}, {Field: "targetDate", Current: "none", Planned: "2026-12-15"}}
	if len(p.saves) != 0 || len(a.reserves) != 0 || r.Plan.Op != "create" || !reflect.DeepEqual(r.Plan.Changes, want) || !reflect.DeepEqual(r.Plan.Sends, []string{"project", "name", "targetDate"}) || r.By != "owner" || r.Ask || len(r.Plan.Flags) != 0 || !strings.Contains(r.Note, "Nothing else changes") {
		t.Fatal(r, p.saves, a.reserves)
	}
	check := a.checks[0]
	if len(a.checks) != 1 || check["confirm"] != false || check["linear_project"] != projectA || check["op"] != "create" || !reflect.DeepEqual(check["fields"], []string{"name", "targetDate"}) || !reflect.DeepEqual(check["team_ids"], []string{teamID}) || check["plan_sha256"] != r.Plan.PlanSHA256 || check["name_sha256"] != linearprovider.MilestoneNameHash("gamma") || check["milestone"] != nil {
		t.Fatal(a.checks)
	}
	got, e := msConfirm(s, scope, in, r.Plan.PlanSHA256)
	if e != nil || got.Outcome != "accepted" || len(got.Changed) != 0 || got.By != "owner" || got.MilestoneID != mpNew {
		t.Fatal(got, e)
	}
	if len(p.saves) != 1 || p.saves[0].ID != "" || *p.saves[0].Name != "Gamma" || *p.saves[0].TargetDate.Value != "2026-12-15" || p.saves[0].Project != projectA {
		t.Fatal(p.saves)
	}
	if len(a.reserves) != 1 || a.reserves[0]["plan_sha256"] != r.Plan.PlanSHA256 || a.reserves[0]["name_sha256"] != linearprovider.MilestoneNameHash("GAMMA") {
		t.Fatal(a.reserves)
	}
	if len(a.settles) != 1 || a.settles[0]["outcome"] != "accepted" || a.settles[0]["milestone"] != mpNew || len(a.settles[0]["changed"].([]string)) != 0 {
		t.Fatal(a.settles)
	}
	// A second create of the name is a duplicate now: refused, nothing sent.
	if _, e = s.SetMilestonePlan(context.Background(), scope, mpIn(MilestonePlanInput{Name: msName("GAMMA")})); e == nil || !strings.Contains(e.Error(), "already has a milestone") || len(p.saves) != 1 {
		t.Fatal(e)
	}
}

// An update previews the milestone's current values and sends the milestone
// as its UUID with only the given fields.
func TestMilestoneUpdatePreviewThenConfirm(t *testing.T) {
	s, a, p, scope := msPlanTest(t)
	in := MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Alpha two"), TargetDate: dateSet("2026-12-01")}
	r := msPreview(t, s, scope, in)
	want := []MilestonePlanChange{{Field: "name", Current: "Alpha", Planned: "Alpha two"}, {Field: "targetDate", Current: "2026-11-30", Planned: "2026-12-01"}}
	if r.Plan.Op != "update" || r.Plan.MilestoneID != mpAlpha || !reflect.DeepEqual(r.Plan.Changes, want) || !reflect.DeepEqual(r.Plan.Sends, []string{"project", "id", "name", "targetDate"}) {
		t.Fatal(r)
	}
	if a.checks[0]["milestone"] != mpAlpha || a.checks[0]["op"] != "update" || a.checks[0]["name_sha256"] != nil {
		t.Fatal(a.checks)
	}
	got, e := msConfirm(s, scope, in, r.Plan.PlanSHA256)
	if e != nil || got.Outcome != "accepted" || len(p.saves) != 1 || p.saves[0].ID != mpAlpha || *p.saves[0].Name != "Alpha two" {
		t.Fatal(got, e, p.saves)
	}
	if a.settles[0]["outcome"] != "accepted" || a.settles[0]["milestone"] != nil {
		t.Fatal(a.settles)
	}
	// Now as planned: nothing is reserved or sent again.
	again, e := msConfirm(s, scope, in, r.Plan.PlanSHA256)
	if e != nil || again.Outcome != "already" || len(p.saves) != 1 || len(a.reserves) != 1 {
		t.Fatal(again, e)
	}
}

// A clear of the target date is sent as null and reads back as none; a
// rename alone sends no date.
func TestMilestoneClearTargetDate(t *testing.T) {
	s, a, p, scope := msPlanTest(t)
	in := MilestonePlanInput{MilestoneID: mpAlpha, TargetDate: dateClear}
	r := msPreview(t, s, scope, in)
	if r.Plan.Changes[0] != (MilestonePlanChange{Field: "targetDate", Current: "2026-11-30", Planned: "none"}) {
		t.Fatal(r.Plan.Changes)
	}
	got, e := msConfirm(s, scope, in, r.Plan.PlanSHA256)
	if e != nil || got.Outcome != "accepted" || len(p.saves) != 1 || !p.saves[0].TargetDate.Present || p.saves[0].TargetDate.Value != nil || p.saves[0].Name != nil || a.settles[0]["outcome"] != "accepted" {
		t.Fatal(got, e, p.saves)
	}
}

// Q4: a milestone date after the project's target date is flagged in the
// preview and the confirm still goes; so is an update that leaves an
// already-later date alone.
func TestMilestoneDateAfterTheProjectIsFlagged(t *testing.T) {
	s, _, p, scope := msPlanTest(t)
	in := MilestonePlanInput{Name: msName("Late"), TargetDate: dateSet("2027-03-01")}
	r := msPreview(t, s, scope, in)
	if len(r.Plan.Flags) != 1 || !strings.Contains(r.Plan.Flags[0], "2027-03-01") || !strings.Contains(r.Plan.Flags[0], "2026-12-31") {
		t.Fatal(r.Plan.Flags)
	}
	if got, e := msConfirm(s, scope, in, r.Plan.PlanSHA256); e != nil || got.Outcome != "accepted" || len(got.Plan.Flags) != 1 || len(p.saves) != 1 {
		t.Fatal("a flagged plan was refused:", got, e)
	}
	// On the project's own date, or before it, or with no date: no flag.
	for _, d := range []linearprovider.Field[*string]{dateSet("2026-12-31"), dateSet("2026-12-30"), dateClear, {}} {
		r = msPreview(t, s, scope, MilestonePlanInput{Name: msName("Other " + derefDate(d.Value)), TargetDate: d})
		if len(r.Plan.Flags) != 0 {
			t.Fatal(d, r.Plan.Flags)
		}
	}
	// An update of the name only, on a milestone already later.
	p.ms[0].TargetDate = msDay("2027-06-01")
	if r = msPreview(t, s, scope, MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Alpha 2")}); len(r.Plan.Flags) != 1 {
		t.Fatal(r.Plan.Flags)
	}
	// The project has no target date: nothing to be after.
	p.planFake.target = nil
	if r = msPreview(t, s, scope, MilestonePlanInput{Name: msName("Latest"), TargetDate: dateSet("2027-03-01")}); len(r.Plan.Flags) != 0 {
		t.Fatal(r.Plan.Flags)
	}
}

// The read-back decides on an update: another field Linear changed is
// accepted_with_changes, named; a field not as sent is uncertain after the
// rereads; a refusal that changed nothing is not_sent; an unreadable
// read-back is uncertain.
func TestMilestoneReadBackOutcomes(t *testing.T) {
	rejected := &linearprovider.Error{Kind: "provider_rejected"}
	six := 6.0
	for name, c := range map[string]struct {
		in      MilestonePlanInput
		set     func(*msPlanFake)
		outcome string
		settle  string
		changed []string
	}{
		"date changed too":    {MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Alpha 2")}, func(p *msPlanFake) { p.onSave = func(p *msPlanFake) { p.ms[0].TargetDate = msDay("2027-02-01") } }, "accepted_with_changes", "accepted", []string{"targetDate"}},
		"name changed too":    {MilestonePlanInput{MilestoneID: mpAlpha, TargetDate: dateSet("2026-12-02")}, func(p *msPlanFake) { p.onSave = func(p *msPlanFake) { p.ms[0].Name = "Renamed" } }, "accepted_with_changes", "accepted", []string{"name"}},
		"sort order changed":  {MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Alpha 2")}, func(p *msPlanFake) { p.onSave = func(p *msPlanFake) { p.ms[0].SortOrder = &six } }, "accepted_with_changes", "accepted", []string{"sortOrder"}},
		"read-back differs":   {MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Alpha 2")}, func(p *msPlanFake) { p.ignore = true }, "uncertain", "uncertain", nil},
		"date differs":        {MilestonePlanInput{MilestoneID: mpAlpha, TargetDate: dateSet("2026-12-02")}, func(p *msPlanFake) { p.ignore = true }, "uncertain", "uncertain", nil},
		"date unknown":        {MilestonePlanInput{MilestoneID: mpAlpha, TargetDate: dateSet("2026-12-02")}, func(p *msPlanFake) { p.onSave = func(p *msPlanFake) { p.ms[0].TargetDateKnown = false } }, "uncertain", "uncertain", nil},
		"refused":             {MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Alpha 2")}, func(p *msPlanFake) { p.ignore, p.saveErr = true, rejected }, "not_sent", "not_sent", nil},
		"refused but applied": {MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Alpha 2")}, func(p *msPlanFake) { p.saveErr = rejected }, "accepted", "accepted", nil},
		"unreadable":          {MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Alpha 2")}, func(p *msPlanFake) { p.readErr = errors.New("timeout") }, "uncertain", "uncertain", nil},
		"vanished":            {MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Alpha 2")}, func(p *msPlanFake) { p.onSave = func(p *msPlanFake) { p.ms = p.ms[1:] } }, "uncertain", "uncertain", nil},
	} {
		s, a, p, scope := msPlanTest(t)
		c.set(p)
		r := msPreview(t, s, scope, c.in)
		got, _ := msConfirm(s, scope, c.in, r.Plan.PlanSHA256)
		if got.Outcome != c.outcome || c.changed != nil && !reflect.DeepEqual(got.Changed, c.changed) {
			t.Fatal(name, got)
		}
		if len(a.settles) != 1 || a.settles[0]["outcome"] != c.settle || c.changed != nil && !reflect.DeepEqual(a.settles[0]["changed"], c.changed) {
			t.Fatal(name, a.settles)
		}
		if c.outcome == "uncertain" && !strings.Contains(got.Note, "tell the owner") || len(p.saves) != 1 {
			t.Fatal(name, got.Note, p.saves)
		}
	}
}

// An uncertain create (N1): the settle finds the milestone by the ID the
// save returned; with none, by its folded name among the milestones that
// are new since the read; with two, or none, it stays uncertain.
func TestMilestoneUncertainCreate(t *testing.T) {
	rejected := &linearprovider.Error{Kind: "provider_rejected"}
	timeout := &linearprovider.Error{Kind: "unavailable"}
	for name, c := range map[string]struct {
		set       func(*msPlanFake)
		outcome   string
		milestone string
	}{
		"by the returned ID":                         {func(p *msPlanFake) {}, "accepted", mpNew},
		"by the name, the save named none":           {func(p *msPlanFake) { p.noID = true }, "accepted", mpNew},
		"the save timed out, the milestone is there": {func(p *msPlanFake) { p.noID, p.saveErr = true, timeout }, "accepted", mpNew},
		"the save timed out, nothing is there":       {func(p *msPlanFake) { p.ignore, p.saveErr = true, timeout }, "uncertain", ""},
		"the returned ID isn't listed":               {func(p *msPlanFake) { p.ignore, p.savedID = true, mpOther }, "uncertain", ""},
		"the returned ID has another name": {func(p *msPlanFake) {
			p.savedID = mpOther
			p.onSave = func(p *msPlanFake) {
				p.ms = append(p.ms, linearprovider.Milestone{ID: mpOther, Name: "Else", TargetDateKnown: true})
			}
		}, "uncertain", ""},
		"two new milestones of the name": {func(p *msPlanFake) {
			p.noID = true
			p.onSave = func(p *msPlanFake) {
				p.ms = append(p.ms, linearprovider.Milestone{ID: mpDup, Name: "gamma", TargetDateKnown: true})
			}
		}, "uncertain", ""},
		"refused and nothing is there": {func(p *msPlanFake) { p.ignore, p.saveErr = true, rejected }, "not_sent", ""},
	} {
		s, a, p, scope := msPlanTest(t)
		c.set(p)
		in := MilestonePlanInput{Name: msName("Gamma")}
		r := msPreview(t, s, scope, in)
		got, _ := msConfirm(s, scope, in, r.Plan.PlanSHA256)
		want := c.outcome
		if got.Outcome != want || got.MilestoneID != c.milestone {
			t.Fatal(name, got)
		}
		if len(a.settles) != 1 || a.settles[0]["outcome"] != c.outcome {
			t.Fatal(name, a.settles)
		}
		if c.milestone != "" && a.settles[0]["milestone"] != c.milestone || c.milestone == "" && a.settles[0]["milestone"] != nil {
			t.Fatal(name, a.settles)
		}
		if len(p.saves) != 1 {
			t.Fatal(name, "sent", len(p.saves))
		}
	}
	// A namesake that appears after the read is not claimed when the save
	// made nothing and named nothing.
	s, a, p, scope := msPlanTest(t)
	in := MilestonePlanInput{Name: msName("Gamma")}
	r := msPreview(t, s, scope, in)
	p.ignore, p.noID, p.saveErr = true, true, timeout
	p.onSave = func(p *msPlanFake) {}
	got, _ := msConfirm(s, scope, in, r.Plan.PlanSHA256)
	if got.Outcome != "uncertain" || a.settles[0]["outcome"] != "uncertain" {
		t.Fatal(got)
	}
}

// The name rules (N2), the duplicate check after case folding and NFC, an
// update of a milestone that isn't the project's, and the other checks:
// refused before anything is reserved or sent.
func TestMilestonePlanRefusals(t *testing.T) {
	long := strings.Repeat("x", 81)
	for name, c := range map[string]struct {
		in    MilestonePlanInput
		setup func(*msAuthority, *msPlanFake)
		// confirm previews first, then changes the project with afterPreview
		// and confirms.
		confirm      bool
		afterPreview func(*msPlanFake)
	}{
		"never its name":                        {in: MilestonePlanInput{ProjectID: "Roadmap", Name: msName("Gamma")}},
		"isn't in this binding":                 {in: MilestonePlanInput{ProjectID: newProject, Name: msName("Gamma")}},
		"never by its name":                     {in: MilestonePlanInput{MilestoneID: "Alpha", Name: msName("Gamma")}},
		"a new milestone needs a name":          {in: MilestonePlanInput{TargetDate: dateSet("2026-12-01")}},
		"changes the name or the target date":   {in: MilestonePlanInput{MilestoneID: mpAlpha}},
		"1–80 characters":                       {in: MilestonePlanInput{Name: &long}},
		"1–80 characters ":                      {in: MilestonePlanInput{Name: msName("   ")}},
		"control character":                     {in: MilestonePlanInput{Name: msName("a\x1b[31mb")}},
		"control character ":                    {in: MilestonePlanInput{Name: msName("two\nlines")}},
		"bidirectional control":                 {in: MilestonePlanInput{Name: msName("evil\u202etxt")}},
		"bidirectional control ":                {in: MilestonePlanInput{Name: msName("iso\u2066late")}},
		"zero-width":                            {in: MilestonePlanInput{Name: msName("zero\u200bwidth")}},
		"zero-width ":                           {in: MilestonePlanInput{Name: msName("bom\ufeffname")}},
		"already has a milestone named \"Beta":  {in: MilestonePlanInput{Name: msName("beta")}},
		"already has a milestone named \"Alpha": {in: MilestonePlanInput{Name: msName("ALPHA")}},
		"already has a milestone named \"Caf": {in: MilestonePlanInput{Name: msName("Cafe\u0301")}, setup: func(_ *msAuthority, p *msPlanFake) {
			p.ms = append(p.ms, linearprovider.Milestone{ID: mpDup, Name: "Caf\u00e9", TargetDateKnown: true})
		}},
		"already has a milestone named \"Bet":  {in: MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("BETA")}},
		"milestone_not_in_project":             {in: MilestonePlanInput{MilestoneID: mpOther, Name: msName("Gamma")}},
		"target date must be a calendar date":  {in: MilestonePlanInput{Name: msName("Gamma"), TargetDate: dateSet("2026-02-30")}},
		"target date must be a calendar date ": {in: MilestonePlanInput{Name: msName("Gamma"), TargetDate: dateSet("2101-01-01")}},
		"team board has no projects":           {in: MilestonePlanInput{Name: msName("Gamma")}, setup: func(a *msAuthority, _ *msPlanFake) { a.b.Mode, a.b.LinearProjectIDs = "team_board", []string{} }},
		"can't create or update milestones":    {in: MilestonePlanInput{Name: msName("Gamma")}, setup: func(_ *msAuthority, p *msPlanFake) { p.reason = "save_milestone has no project" }},
		"changed since the preview":            {in: MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Gamma")}, confirm: true, afterPreview: func(p *msPlanFake) { p.ms[0].TargetDate = msDay("2026-12-12") }},
		"changed since the preview ": {in: MilestonePlanInput{Name: msName("Gamma")}, confirm: true, afterPreview: func(p *msPlanFake) {
			p.ms = append(p.ms, linearprovider.Milestone{ID: mpDup, Name: "Else", TargetDateKnown: true})
		}},
		"changed since the preview  ":           {in: MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Gamma")}, confirm: true, afterPreview: func(p *msPlanFake) { p.ms[0].Name = "Renamed" }},
		"changed since the preview   ":          {in: MilestonePlanInput{Name: msName("Gamma")}, confirm: true, afterPreview: func(p *msPlanFake) { p.planFake.target = "2027-05-05" }},
		"could not be reserved":                 {in: MilestonePlanInput{Name: msName("Gamma")}, confirm: true, afterPreview: func(p *msPlanFake) {}, setup: func(a *msAuthority, _ *msPlanFake) { a.unproven = true }},
		"has no lanes, so a session changes no": {in: MilestonePlanInput{Name: msName("Gamma")}, confirm: true, afterPreview: func(p *msPlanFake) {}, setup: func(a *msAuthority, _ *msPlanFake) { a.actor, a.name = "ses", "ses" }},
		"no write team of this session's lane": {in: MilestonePlanInput{Name: msName("Gamma")}, confirm: true, afterPreview: func(p *msPlanFake) {}, setup: func(a *msAuthority, _ *msPlanFake) {
			a.actor, a.name, a.lane, a.laneTeams = "ses", "ses", "far", []string{otherTeamID}
			a.b.Lanes = []bus.Lane{{Name: "far", WriteTeamIDs: []string{otherTeamID}, DefaultTeamID: otherTeamID}, {Name: "near", WriteTeamIDs: []string{teamID}, DefaultTeamID: teamID}}
		}},
	} {
		s, a, p, scope := msPlanTest(t)
		if c.setup != nil {
			c.setup(a, p)
		}
		if a.actor != "" {
			scope.Session = a.actor
		}
		in := c.in
		if in.ProjectID == "" {
			in.ProjectID = projectA
		}
		in.OperationID = "ms-1"
		var e error
		if c.confirm {
			r, pe := s.SetMilestonePlan(context.Background(), scope, in)
			if pe != nil {
				t.Fatal(name, pe)
			}
			c.afterPreview(p)
			in.Confirm, in.ExpectedPlanSHA256 = true, r.Plan.PlanSHA256
		}
		_, e = s.SetMilestonePlan(context.Background(), scope, in)
		if e == nil || !strings.Contains(e.Error(), strings.TrimSpace(name)) || len(p.saves) != 0 || len(a.settles) != 0 {
			t.Fatal(name, e, p.saves, a.settles)
		}
		if strings.TrimSpace(name) != "could not be reserved" && len(a.reserves) != 0 {
			t.Fatal(name, "reserved", a.reserves)
		}
		if !c.confirm && len(a.checks) != 0 {
			t.Fatal(name, "asked the server", a.checks)
		}
	}
}

// A milestone of another project is refused for an update, nothing sent; a
// rename to its own name in another case is fine.
func TestMilestoneUpdateNeedsTheProjectsMilestone(t *testing.T) {
	s, a, p, scope := msPlanTest(t)
	_, e := s.SetMilestonePlan(context.Background(), scope, mpIn(MilestonePlanInput{MilestoneID: mpOther, TargetDate: dateSet("2026-12-01")}))
	if e == nil || !strings.HasPrefix(e.Error(), "milestone_not_in_project") || len(p.saves) != 0 || len(a.checks) != 0 {
		t.Fatal(e)
	}
	if _, e = s.SetMilestonePlan(context.Background(), scope, mpIn(MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("ALPHA")})); e != nil {
		t.Fatal("a rename to its own name in another case:", e)
	}
}

// The names in a preview are terminal-safe (N4), the digest binds the
// unmodified name, and a milestone name Linear holds with a control
// character can't move the cursor.
func TestMilestonePreviewNamesAreSafe(t *testing.T) {
	s, _, p, scope := msPlanTest(t)
	p.ms[0].Name = "Al\x1b[31mpha\u202e"
	r := msPreview(t, s, scope, MilestonePlanInput{MilestoneID: mpAlpha, Name: msName("Alpha 2")})
	if cur := r.Plan.Changes[0].Current; strings.ContainsAny(cur, "\x1b\u202e") || !strings.Contains(cur, `\x1b`) || !strings.Contains(cur, `\u202e`) {
		t.Fatal(cur)
	}
	// A create that collides with such a name says so safely.
	p.ms[1].Name = "Be\ta"
	_, e := s.SetMilestonePlan(context.Background(), scope, mpIn(MilestonePlanInput{Name: msName("Be\u200ba")}))
	if e == nil || !strings.Contains(e.Error(), "zero-width") {
		t.Fatal(e)
	}
}

// A caller the server refuses gets the preview with the refusal and "ask the
// project owner or a lead", and sends nothing; its confirm is refused at the
// reservation. Any other error of the check is the preview's error.
func TestMilestonePreviewForNonConfirmers(t *testing.T) {
	for name, refusal := range map[string]error{
		"member":      bus.PublicError("forbidden", "only the project owner, a project lead, a team's lead in the team roles, or the coordinator inside its lane creates or updates a Linear project's milestones; preview it and ask the project owner or a lead"),
		"other lane":  bus.PublicError("forbidden", "outside_lane: the project holds no team of lane near that this session may plan for"),
		"over remote": &remoteRefusal{code: "forbidden"},
	} {
		s, a, p, scope := msPlanTest(t)
		a.refuse = refusal
		in := MilestonePlanInput{Name: msName("Gamma")}
		r := msPreview(t, s, scope, in)
		if !r.Ask || r.By != "" || !strings.Contains(r.Note, "ask the project owner or a lead") || len(p.saves) != 0 || r.Plan.PlanSHA256 == "" {
			t.Fatal(name, r)
		}
		if _, e := msConfirm(s, scope, in, r.Plan.PlanSHA256); e == nil || !strings.Contains(e.Error(), "nothing was sent") || len(p.saves) != 0 || len(a.settles) != 0 {
			t.Fatal(name, e)
		}
	}
	s, a, _, scope := msPlanTest(t)
	a.refuse = bus.PublicError("invalid", "team_ids must list the project's teams, as read")
	if r, e := s.SetMilestonePlan(context.Background(), scope, mpIn(MilestonePlanInput{Name: msName("Gamma")})); e == nil || r.Ask {
		t.Fatal(r, e)
	}
}

// Unproven: the preview says so and works; the confirm's reservation is
// refused with milestone_plan_unproven and nothing is sent.
func TestMilestonePlanUnproven(t *testing.T) {
	s, a, p, scope := msPlanTest(t)
	a.unproven = true
	in := MilestonePlanInput{Name: msName("Gamma")}
	r := msPreview(t, s, scope, in)
	if !strings.Contains(r.Note, "milestone_plan_unproven") || r.Ask {
		t.Fatal(r)
	}
	if _, e := msConfirm(s, scope, in, r.Plan.PlanSHA256); e == nil || !strings.Contains(e.Error(), "milestone_plan_unproven") || len(p.saves) != 0 || len(a.settles) != 0 {
		t.Fatal(e, p.saves)
	}
}

// The MCP route: a null date clears, a missing name leaves it, a bad type
// is refused before anything is read, and an unknown argument is refused.
func TestMilestonePlanExecuteV2(t *testing.T) {
	s, a, p, scope := msPlanTest(t)
	args := map[string]any{"project": projectA, "milestone": mpAlpha, "target_date": nil, "operation_id": "ms-1"}
	out, e := s.ExecuteV2(context.Background(), scope, "milestone_plan", args)
	r, _ := out.(MilestonePlanResult)
	if e != nil || r.Outcome != "preview" || r.Plan.Changes[0] != (MilestonePlanChange{Field: "targetDate", Current: "2026-11-30", Planned: "none"}) {
		t.Fatal(out, e)
	}
	args["confirm"], args["expected_plan_sha256"] = true, r.Plan.PlanSHA256
	out, e = s.ExecuteV2(context.Background(), scope, "milestone_plan", args)
	if r, _ = out.(MilestonePlanResult); e != nil || r.Outcome != "accepted" || len(p.saves) != 1 || !p.saves[0].TargetDate.Present || p.saves[0].TargetDate.Value != nil || p.saves[0].Name != nil {
		t.Fatal(out, e, p.saves)
	}
	lists := p.lists
	for _, bad := range []map[string]any{
		{"project": projectA, "name": 5, "operation_id": "ms-2"},
		{"project": projectA, "name": nil, "operation_id": "ms-2"},
		{"project": projectA, "name": "Gamma", "target_date": 5, "operation_id": "ms-2"},
		{"project": projectA, "name": "Gamma", "target_date": "2027-13-01", "operation_id": "ms-2"},
		{"project": projectA, "name": "Gamma", "operation_id": "ms-2", "description": "x"},
		{"project": projectA, "name": "Gamma", "operation_id": "ms-2", "team_id": teamID},
		{"project": projectA, "milestone": "Alpha", "name": "Gamma", "operation_id": "ms-2"},
	} {
		if _, e = s.ExecuteV2(context.Background(), scope, "milestone_plan", bad); e == nil || p.lists != lists || len(a.checks) != 1 {
			t.Fatal(bad, e)
		}
	}
}

// ClawBridge's agent marker goes to the server with the preview's check and
// the confirm's reservation.
func TestMilestonePlanSendsTheAgentMarker(t *testing.T) {
	s, a, _, scope := msPlanTest(t)
	agentEnviron = func() []string { return []string{"PATH=/bin", "CLAUDECODE=1"} }
	in := MilestonePlanInput{Name: msName("Gamma")}
	r := msPreview(t, s, scope, in)
	if _, e := msConfirm(s, scope, in, r.Plan.PlanSHA256); e != nil || a.checks[0]["agent_marker"] != "CLAUDECODE" || a.reserves[0]["agent_marker"] != "CLAUDECODE" {
		t.Fatal(e, a.checks, a.reserves)
	}
}

// The binding and the lane are checked again after the reservation, right
// before the send; a change settles not_sent. A replay of a reservation
// never settled is uncertain and sends nothing.
func TestMilestonePlanRecheckAndReplay(t *testing.T) {
	s, a, p, scope := msPlanTest(t)
	in := MilestonePlanInput{Name: msName("Gamma")}
	r := msPreview(t, s, scope, in)
	a.afterReserve = func() { a.b.Revision++ }
	got, e := msConfirm(s, scope, in, r.Plan.PlanSHA256)
	if e == nil || got.Outcome != "not_sent" || len(p.saves) != 0 || len(a.settles) != 1 || a.settles[0]["outcome"] != "not_sent" {
		t.Fatal(got, e, a.settles)
	}
	s, a, p, scope = msPlanTest(t)
	a.actor, a.name, a.lane, a.laneTeams, a.by, a.planLane = "ses", "ses", "near", []string{teamID}, "coordinator", "near"
	a.b.Lanes = []bus.Lane{{Name: "near", WriteTeamIDs: []string{teamID}, DefaultTeamID: teamID}}
	scope.Session = "ses"
	r = msPreview(t, s, scope, in)
	if r.By != "coordinator" || !strings.Contains(r.Note, "inside lane") {
		t.Fatal(r)
	}
	a.afterReserve = func() { a.lane, a.laneTeams = "far", []string{otherTeamID} }
	if got, e = msConfirm(s, scope, in, r.Plan.PlanSHA256); e == nil || !strings.Contains(e.Error(), "outside_lane") || len(p.saves) != 0 || a.settles[0]["outcome"] != "not_sent" {
		t.Fatal(got, e)
	}
	s, a, p, scope = msPlanTest(t)
	r = msPreview(t, s, scope, in)
	a.replay = map[string]any{"state": "reserved", "by": "owner", "replay": true}
	if got, e = msConfirm(s, scope, in, r.Plan.PlanSHA256); e != nil || got.Outcome != "uncertain" || len(p.saves) != 0 || len(a.settles) != 0 {
		t.Fatal(got, e)
	}
	// A settled replay answers its state and milestone.
	a.replay = map[string]any{"state": "accepted", "by": "owner", "replay": true, "milestone": mpNew}
	if got, e = msConfirm(s, scope, in, r.Plan.PlanSHA256); e != nil || got.Outcome != "accepted" || got.MilestoneID != mpNew || len(p.saves) != 0 {
		t.Fatal(got, e)
	}
}

// A team's lead without a session confirms only while Linear states their
// own user active and not a guest; the check runs before the reservation.
func TestMilestonePlanTeamLeadEligible(t *testing.T) {
	for name, guest := range map[string]bool{"active": false, "guest": true} {
		s, a, p, scope := msPlanTest(t)
		a.role, a.by, a.planTeam = "member", "team_lead", teamID
		p.me, p.users = addLead, []linearprovider.WorkspaceUser{{ID: addLead, Active: true, ActiveKnown: true, Guest: guest, GuestKnown: true}}
		in := MilestonePlanInput{Name: msName("Gamma")}
		r := msPreview(t, s, scope, in)
		if !strings.Contains(r.Note, "team "+teamID+"'s lead") {
			t.Fatal(name, r.Note)
		}
		got, e := msConfirm(s, scope, in, r.Plan.PlanSHA256)
		if !guest && (e != nil || got.Outcome != "accepted" || len(a.reserves) != 1) {
			t.Fatal(name, got, e)
		}
		if guest && (e == nil || !strings.Contains(e.Error(), "active, non-guest") || len(a.reserves) != 0 || len(p.saves) != 0) {
			t.Fatal(name, e)
		}
	}
}

var _ = time.Second
