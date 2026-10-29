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

// planAuthority is the server side of a plan, as the fake authority
// answers it: checks are the previews' reserves (confirm false), reserves
// the confirms'.
type planAuthority struct {
	*adminAuthority
	checks, reserves, settles []map[string]any
	// refuse answers every reserve; unproven refuses a confirm as the
	// server's gate does; replay answers a confirm.
	refuse                 error
	unproven               bool
	replay                 map[string]any
	by, planLane, planTeam string
	afterReserve           func()
}

func (a *planAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "linear_write_charge" {
		return budgetAnswer(r)
	}
	switch r.Operation {
	case "project_plan_reserve":
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
			return bus.Result{}, bus.PublicError("forbidden", "project_plan_unproven: this server sets no project plan until its operator sets serve --project-plan-proven after the probe; the preview works")
		}
		if a.afterReserve != nil {
			a.afterReserve()
		}
		if a.replay != nil {
			return bus.Result{Source: "local-authority", Data: a.replay}, nil
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": "reserved", "by": a.by, "lane": a.planLane, "replay": false}}, nil
	case "project_plan_settle":
		a.settles = append(a.settles, r.Args)
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": r.Args["outcome"]}}, nil
	}
	return a.adminAuthority.Execute(ctx, r)
}

// planFake holds projectA's plan fields as get_project gives them (nil is
// none); a save sets the given fields unless ignore is set, then runs
// onSave. readErr fails from the third read on (the confirm's read-back
// after a preview).
type planFake struct {
	*adderFake
	priority, target, start, targetRes, startRes any
	planReason                                   string
	saves                                        []linearprovider.ProjectPlan
	ignore                                       bool
	sendErr, readErr                             error
	onSave                                       func(*planFake)
	// noStart leaves startDate out of the reads.
	noStart bool
}

func (p *planFake) ProjectPlanReason([]string) string { return p.planReason }
func (p *planFake) SaveProjectPlan(_ context.Context, _ string, plan linearprovider.ProjectPlan) error {
	p.saves = append(p.saves, plan)
	if !p.ignore {
		if plan.Priority != nil {
			p.priority = *plan.Priority
		}
		for _, d := range []struct {
			f  linearprovider.Field[*string]
			to *any
		}{{plan.TargetDate, &p.target}, {plan.StartDate, &p.start}} {
			if d.f.Present {
				*d.to = nil
				if d.f.Value != nil {
					*d.to = *d.f.Value
				}
			}
		}
	}
	if p.onSave != nil {
		p.onSave(p)
	}
	return p.sendErr
}
func (p *planFake) ProjectSnapshot(_ context.Context, project string) (linearprovider.ProjectSnapshot, error) {
	p.reads++
	if p.reads > 2 && p.readErr != nil {
		return linearprovider.ProjectSnapshot{}, p.readErr
	}
	s := linearprovider.ProjectSnapshot{ID: project, TeamIDs: append([]string{}, p.teams...), Lead: p.lead, LeadKnown: true, MembersKnown: true}.
		WithField("priority", p.priority).WithField("targetDate", p.target).
		WithField("targetDateResolution", p.targetRes).WithField("startDateResolution", p.startRes).WithField("name", "Plan")
	if !p.noStart {
		s = s.WithField("startDate", p.start)
	}
	return s, nil
}

func planTest(t *testing.T) (*Service, *planAuthority, *planFake, bus.Request) {
	t.Helper()
	s, a, p, scope := adminTest(t)
	auth := &planAuthority{adminAuthority: a, by: "owner"}
	fake := &planFake{adderFake: &adderFake{adminFake: p, teams: []string{teamID}, lead: addLead}, priority: 3, target: "2026-12-31", start: "2026-11-01"}
	s.cfg.Executor = auth
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return fake, nil }
	scope.Session, a.actor, a.name = "", "", ""
	oldEnv, oldWait := agentEnviron, teamAddWait
	agentEnviron = func() []string { return nil }
	teamAddWait = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { agentEnviron, teamAddWait = oldEnv, oldWait })
	return s, auth, fake, scope
}

func intp(n int) *int { return &n }

func dateSet(d string) linearprovider.Field[*string] {
	return linearprovider.Field[*string]{Present: true, Value: &d}
}

var dateClear = linearprovider.Field[*string]{Present: true}

func planPreview(t *testing.T, s *Service, scope bus.Request, in ProjectPlanInput) ProjectPlanResult {
	t.Helper()
	in.ProjectID, in.OperationID = projectA, "plan-1"
	r, e := s.SetProjectPlan(context.Background(), scope, in)
	if e != nil || r.Outcome != "preview" {
		t.Fatal(r, e)
	}
	return r
}

func planConfirm(s *Service, scope bus.Request, in ProjectPlanInput, sha string) (ProjectPlanResult, error) {
	in.ProjectID, in.OperationID, in.Confirm, in.ExpectedPlanSHA256 = projectA, "plan-1", true, sha
	return s.SetProjectPlan(context.Background(), scope, in)
}

// The preview reads the project, shows each field current → planned and
// who confirms, asks the server without confirm, and sends nothing; the
// confirm reserves with the same digest, sends only the given fields,
// reads back before settling, and settles accepted.
func TestProjectPlanPreviewThenConfirm(t *testing.T) {
	s, a, p, scope := planTest(t)
	in := ProjectPlanInput{Priority: intp(1), TargetDate: dateSet("2027-01-15")}
	r := planPreview(t, s, scope, in)
	want := []ProjectPlanChange{{Field: "priority", Current: "medium (3)", Planned: "urgent (1)"}, {Field: "targetDate", Current: "2026-12-31", Planned: "2027-01-15"}}
	if len(p.saves) != 0 || len(a.reserves) != 0 || !reflect.DeepEqual(r.Plan.Changes, want) || !reflect.DeepEqual(r.Plan.Sends, []string{"id", "priority", "targetDate"}) || r.By != "owner" || r.Ask || !strings.Contains(r.Note, "project owner") || !strings.Contains(r.Note, "Nothing else changes") {
		t.Fatal(r, p.saves, a.reserves)
	}
	check := a.checks[0]
	if len(a.checks) != 1 || check["confirm"] != false || check["linear_project"] != projectA || !reflect.DeepEqual(check["fields"], []string{"priority", "targetDate"}) || !reflect.DeepEqual(check["team_ids"], []string{teamID}) || check["plan_sha256"] != r.Plan.PlanSHA256 || check["agent_marker"] != "" {
		t.Fatal(a.checks)
	}
	got, e := planConfirm(s, scope, in, r.Plan.PlanSHA256)
	if e != nil || got.Outcome != "accepted" || len(got.Changed) != 0 || got.By != "owner" {
		t.Fatal(got, e)
	}
	if len(p.saves) != 1 || *p.saves[0].Priority != 1 || *p.saves[0].TargetDate.Value != "2027-01-15" || p.saves[0].StartDate.Present || p.reads != 3 {
		t.Fatal(p.saves, p.reads)
	}
	res := a.reserves[0]
	if len(a.reserves) != 1 || res["plan_sha256"] != r.Plan.PlanSHA256 || res["operation_id"] != "plan-1" || res["agent_marker"] != "" {
		t.Fatal(a.reserves)
	}
	if len(a.settles) != 1 || a.settles[0]["outcome"] != "accepted" || len(a.settles[0]["changed"].([]string)) != 0 {
		t.Fatal(a.settles)
	}
	// Now as planned: nothing is reserved or sent again.
	again, e := planConfirm(s, scope, in, r.Plan.PlanSHA256)
	if e != nil || again.Outcome != "already" || len(p.saves) != 1 || len(a.reserves) != 1 {
		t.Fatal(again, e)
	}
}

// The read-back decides: another field Linear changed is
// accepted_with_changes, named; a planned date going from none to a day
// is no change; a field not as sent is uncertain after the rereads; a
// refusal that changed nothing is not_sent; an unreadable read-back and a
// plan field this plan didn't send moving are uncertain.
func TestProjectPlanReadBackOutcomes(t *testing.T) {
	rejected := &linearprovider.Error{Kind: "provider_rejected"}
	for name, c := range map[string]struct {
		set     func(*planFake)
		outcome string
		settle  string
		changed []string
	}{
		"lead changed":           {func(p *planFake) { p.onSave = func(p *planFake) { p.lead = "" } }, "accepted_with_changes", "accepted", []string{"lead"}},
		"a resolution changed":   {func(p *planFake) { p.onSave = func(p *planFake) { p.startRes = "month" } }, "accepted_with_changes", "accepted", []string{"startDateResolution"}},
		"planned date to a day":  {func(p *planFake) { p.target = nil; p.onSave = func(p *planFake) { p.targetRes = "day" } }, "accepted", "accepted", nil},
		"planned date to month":  {func(p *planFake) { p.onSave = func(p *planFake) { p.targetRes = "month" } }, "accepted_with_changes", "accepted", []string{"targetDateResolution"}},
		"read-back differs":      {func(p *planFake) { p.ignore = true }, "uncertain", "uncertain", nil},
		"refused":                {func(p *planFake) { p.ignore, p.sendErr = true, rejected }, "not_sent", "not_sent", nil},
		"unreadable":             {func(p *planFake) { p.readErr = errors.New("timeout") }, "uncertain", "uncertain", nil},
		"another plan field too": {func(p *planFake) { p.onSave = func(p *planFake) { p.start = "2026-10-01" } }, "uncertain", "uncertain", nil},
	} {
		s, a, p, scope := planTest(t)
		c.set(p)
		in := ProjectPlanInput{TargetDate: dateSet("2027-01-15")}
		r := planPreview(t, s, scope, in)
		got, _ := planConfirm(s, scope, in, r.Plan.PlanSHA256)
		if got.Outcome != c.outcome || !reflect.DeepEqual(got.Changed, c.changed) && c.changed != nil {
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

// Refused before anything is reserved or sent, each with nothing asked of
// the server's reserve on a confirm.
func TestProjectPlanRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		in    ProjectPlanInput
		setup func(*planAuthority, *planFake)
		// confirm previews first, then changes the project with
		// afterPreview and confirms.
		confirm      bool
		afterPreview func(*planFake)
	}{
		"never its name":                      {in: ProjectPlanInput{ProjectID: "Roadmap", Priority: intp(2)}},
		"isn't in this binding":               {in: ProjectPlanInput{ProjectID: newProject, Priority: intp(2)}},
		"at least one of":                     {in: ProjectPlanInput{}},
		"priority is 0-4":                     {in: ProjectPlanInput{Priority: intp(5)}},
		"target date must be a calendar date": {in: ProjectPlanInput{TargetDate: dateSet("2026-02-30")}},
		"start date must be a calendar date":  {in: ProjectPlanInput{StartDate: dateSet("26-11-01")}},
		"team board has no projects":          {in: ProjectPlanInput{Priority: intp(2)}, setup: func(a *planAuthority, _ *planFake) { a.b.Mode, a.b.LinearProjectIDs = "team_board", []string{} }},
		"start date 2027-02-01 would come":    {in: ProjectPlanInput{StartDate: dateSet("2027-02-01"), TargetDate: dateSet("2027-01-15")}},
		"start date 2026-11-01 would come":    {in: ProjectPlanInput{TargetDate: dateSet("2026-10-15")}},
		"start date 2027-03-01 would come":    {in: ProjectPlanInput{StartDate: dateSet("2027-03-01")}},
		"doesn't show its start date":         {in: ProjectPlanInput{TargetDate: dateSet("2027-01-15")}, setup: func(_ *planAuthority, p *planFake) { p.noStart = true }},
		"target date is a quarter in Linear":  {in: ProjectPlanInput{TargetDate: dateSet("2027-01-15")}, setup: func(_ *planAuthority, p *planFake) { p.targetRes = "quarter" }},
		"start date is a half-year in Linear": {in: ProjectPlanInput{StartDate: dateClear}, setup: func(_ *planAuthority, p *planFake) { p.startRes = "halfYear" }},
		"can't set project priority, target":  {in: ProjectPlanInput{Priority: intp(2), TargetDate: dateSet("2027-01-15")}, setup: func(_ *planAuthority, p *planFake) { p.planReason = "save_project has no priority" }},
		"project changed since the preview":   {in: ProjectPlanInput{Priority: intp(2)}, confirm: true, afterPreview: func(p *planFake) { p.target = "2027-02-01" }},
		"could not be reserved":               {in: ProjectPlanInput{Priority: intp(2)}, confirm: true, afterPreview: func(p *planFake) {}, setup: func(a *planAuthority, _ *planFake) { a.unproven = true }},
		"has no lanes, so a session sets no":  {in: ProjectPlanInput{Priority: intp(2)}, confirm: true, afterPreview: func(p *planFake) {}, setup: func(a *planAuthority, _ *planFake) { a.actor, a.name = "ses", "ses" }},
		"no write team of this session's lane": {in: ProjectPlanInput{Priority: intp(2)}, confirm: true, afterPreview: func(p *planFake) {}, setup: func(a *planAuthority, _ *planFake) {
			a.actor, a.name, a.lane, a.laneTeams = "ses", "ses", "far", []string{otherTeamID}
			a.b.Lanes = []bus.Lane{{Name: "far", WriteTeamIDs: []string{otherTeamID}, DefaultTeamID: otherTeamID}, {Name: "near", WriteTeamIDs: []string{teamID}, DefaultTeamID: teamID}}
		}},
	} {
		s, a, p, scope := planTest(t)
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
		in.OperationID = "plan-1"
		var e error
		if c.confirm {
			r, pe := s.SetProjectPlan(context.Background(), scope, in)
			if pe != nil {
				t.Fatal(name, pe)
			}
			c.afterPreview(p)
			in.Confirm, in.ExpectedPlanSHA256 = true, r.Plan.PlanSHA256
		}
		_, e = s.SetProjectPlan(context.Background(), scope, in)
		if e == nil || !strings.Contains(e.Error(), name) || len(p.saves) != 0 || len(a.settles) != 0 {
			t.Fatal(name, e, p.saves, a.settles)
		}
		if name != "could not be reserved" && len(a.reserves) != 0 {
			t.Fatal(name, "reserved", a.reserves)
		}
		if !c.confirm && len(a.checks) != 0 {
			t.Fatal(name, "asked the server", a.checks)
		}
	}
}

// A caller the server refuses (forbidden, outside_lane) gets the preview
// with the refusal and "ask the project owner or a lead", and sends
// nothing; its confirm is refused at the reservation. Any other error of
// the check is the preview's error.
func TestProjectPlanPreviewForNonConfirmers(t *testing.T) {
	for name, refusal := range map[string]error{
		"member":      bus.PublicError("forbidden", "only the project owner, a project lead, a team's lead in the team roles, or the coordinator inside its lane sets a Linear project's priority and dates; preview it and ask the project owner or a lead"),
		"other lane":  bus.PublicError("forbidden", "outside_lane: the project holds no team of lane near that this session may plan for"),
		"over remote": &remoteRefusal{code: "forbidden"},
	} {
		s, a, p, scope := planTest(t)
		a.refuse = refusal
		in := ProjectPlanInput{Priority: intp(2)}
		r := planPreview(t, s, scope, in)
		if !r.Ask || r.By != "" || !strings.Contains(r.Note, "ask the project owner or a lead") || len(p.saves) != 0 || r.Plan.PlanSHA256 == "" {
			t.Fatal(name, r)
		}
		if _, e := planConfirm(s, scope, in, r.Plan.PlanSHA256); e == nil || !strings.Contains(e.Error(), "nothing was sent") || len(p.saves) != 0 || len(a.settles) != 0 {
			t.Fatal(name, e)
		}
	}
	s, a, _, scope := planTest(t)
	a.refuse = bus.PublicError("invalid", "team_ids must list the project's teams, as read")
	if r, e := s.SetProjectPlan(context.Background(), scope, ProjectPlanInput{ProjectID: projectA, OperationID: "plan-1", Priority: intp(2)}); e == nil || r.Ask {
		t.Fatal(r, e)
	}
}

// remoteRefusal is a hosted server's refusal as the remote client reports it:
// a code and no message for a 403.
type remoteRefusal struct{ code string }

func (r *remoteRefusal) Error() string {
	return "cloud request rejected (403, " + r.code + "); no write queued"
}
func (r *remoteRefusal) Rejection() (int, string, string) { return 403, r.code, "" }

// Unproven: the preview says so and works; the confirm's reservation is
// refused with project_plan_unproven and nothing is sent.
func TestProjectPlanUnproven(t *testing.T) {
	s, a, p, scope := planTest(t)
	a.unproven = true
	in := ProjectPlanInput{StartDate: dateSet("2026-11-15")}
	r := planPreview(t, s, scope, in)
	if !strings.Contains(r.Note, "project_plan_unproven") || r.Ask {
		t.Fatal(r)
	}
	if _, e := planConfirm(s, scope, in, r.Plan.PlanSHA256); e == nil || !strings.Contains(e.Error(), "project_plan_unproven") || len(p.saves) != 0 || len(a.settles) != 0 {
		t.Fatal(e, p.saves)
	}
}

// A date is cleared (sent as null) and reads back as none.
func TestProjectPlanClearDate(t *testing.T) {
	s, a, p, scope := planTest(t)
	in := ProjectPlanInput{TargetDate: dateClear}
	r := planPreview(t, s, scope, in)
	if r.Plan.Changes[0] != (ProjectPlanChange{Field: "targetDate", Current: "2026-12-31", Planned: "none"}) {
		t.Fatal(r.Plan.Changes)
	}
	got, e := planConfirm(s, scope, in, r.Plan.PlanSHA256)
	if e != nil || got.Outcome != "accepted" || len(p.saves) != 1 || !p.saves[0].TargetDate.Present || p.saves[0].TargetDate.Value != nil || p.target != nil || a.settles[0]["outcome"] != "accepted" {
		t.Fatal(got, e, p.saves)
	}
}

// The MCP route: a priority name is its number, a null date clears, a bad
// date is refused before anything is read.
func TestProjectPlanExecuteV2(t *testing.T) {
	s, a, p, scope := planTest(t)
	args := map[string]any{"linear_project": projectA, "priority": "high", "start_date": nil, "operation_id": "plan-1"}
	out, e := s.ExecuteV2(context.Background(), scope, "project_plan", args)
	r, _ := out.(ProjectPlanResult)
	if e != nil || r.Outcome != "preview" || r.Plan.Changes[0].Planned != "high (2)" || r.Plan.Changes[1] != (ProjectPlanChange{Field: "startDate", Current: "2026-11-01", Planned: "none"}) {
		t.Fatal(out, e)
	}
	args["confirm"], args["expected_plan_sha256"] = true, r.Plan.PlanSHA256
	out, e = s.ExecuteV2(context.Background(), scope, "project_plan", args)
	if r, _ = out.(ProjectPlanResult); e != nil || r.Outcome != "accepted" || len(p.saves) != 1 || *p.saves[0].Priority != 2 || p.saves[0].StartDate.Value != nil || !p.saves[0].StartDate.Present {
		t.Fatal(out, e, p.saves)
	}
	reads := p.reads
	for _, bad := range []map[string]any{
		{"linear_project": projectA, "target_date": "2027-13-01", "operation_id": "plan-2"},
		{"linear_project": projectA, "priority": 7, "operation_id": "plan-2"},
		{"linear_project": projectA, "priority": 2, "operation_id": "plan-2", "team_id": teamID},
	} {
		if _, e = s.ExecuteV2(context.Background(), scope, "project_plan", bad); e == nil || p.reads != reads || len(a.checks) != 1 {
			t.Fatal(bad, e)
		}
	}
}

// ClawBridge's agent marker goes to the server with the preview's check
// and the confirm's reservation: the server refuses a sessionless agent.
func TestProjectPlanSendsTheAgentMarker(t *testing.T) {
	s, a, _, scope := planTest(t)
	agentEnviron = func() []string { return []string{"PATH=/bin", "CLAUDECODE=1"} }
	in := ProjectPlanInput{Priority: intp(2)}
	r := planPreview(t, s, scope, in)
	if _, e := planConfirm(s, scope, in, r.Plan.PlanSHA256); e != nil || a.checks[0]["agent_marker"] != "CLAUDECODE" || a.reserves[0]["agent_marker"] != "CLAUDECODE" {
		t.Fatal(e, a.checks, a.reserves)
	}
}

// Reviewer (a): the binding and the lane are checked again after the
// reservation, right before the send; a change settles not_sent. A replay
// of a reservation never settled is uncertain and sends nothing.
func TestProjectPlanRecheckAndReplay(t *testing.T) {
	s, a, p, scope := planTest(t)
	in := ProjectPlanInput{Priority: intp(2)}
	r := planPreview(t, s, scope, in)
	a.afterReserve = func() { a.b.Revision++ }
	got, e := planConfirm(s, scope, in, r.Plan.PlanSHA256)
	if e == nil || got.Outcome != "not_sent" || len(p.saves) != 0 || len(a.settles) != 1 || a.settles[0]["outcome"] != "not_sent" {
		t.Fatal(got, e, a.settles)
	}
	// A coordinator session whose lane is re-pinned after the reservation.
	s, a, p, scope = planTest(t)
	a.actor, a.name, a.lane, a.laneTeams, a.by, a.planLane = "ses", "ses", "near", []string{teamID}, "coordinator", "near"
	a.b.Lanes = []bus.Lane{{Name: "near", WriteTeamIDs: []string{teamID}, DefaultTeamID: teamID}}
	scope.Session = "ses"
	r = planPreview(t, s, scope, in)
	if r.By != "coordinator" || !strings.Contains(r.Note, "inside lane") {
		t.Fatal(r)
	}
	a.afterReserve = func() { a.lane, a.laneTeams = "far", []string{otherTeamID} }
	if got, e = planConfirm(s, scope, in, r.Plan.PlanSHA256); e == nil || !strings.Contains(e.Error(), "outside_lane") || len(p.saves) != 0 || a.settles[0]["outcome"] != "not_sent" {
		t.Fatal(got, e)
	}
	s, a, p, scope = planTest(t)
	r = planPreview(t, s, scope, in)
	a.replay = map[string]any{"state": "reserved", "by": "owner", "replay": true}
	if got, e = planConfirm(s, scope, in, r.Plan.PlanSHA256); e != nil || got.Outcome != "uncertain" || len(p.saves) != 0 || len(a.settles) != 0 {
		t.Fatal(got, e)
	}
}

// R3, as FR10: a member without a session confirms as a team's lead only
// while Linear states their own user active and not a guest; the check
// runs before the reservation.
func TestProjectPlanTeamLeadEligible(t *testing.T) {
	for name, guest := range map[string]bool{"active": false, "guest": true} {
		s, a, p, scope := planTest(t)
		a.role, a.by, a.planTeam = "member", "team_lead", teamID
		p.me, p.users = addLead, []linearprovider.WorkspaceUser{{ID: addLead, Active: true, ActiveKnown: true, Guest: guest, GuestKnown: true}}
		in := ProjectPlanInput{Priority: intp(2)}
		r := planPreview(t, s, scope, in)
		if !strings.Contains(r.Note, "team "+teamID+"'s lead") {
			t.Fatal(name, r.Note)
		}
		got, e := planConfirm(s, scope, in, r.Plan.PlanSHA256)
		if !guest && (e != nil || got.Outcome != "accepted" || len(a.reserves) != 1) {
			t.Fatal(name, got, e)
		}
		if guest && (e == nil || !strings.Contains(e.Error(), "active, non-guest") || len(a.reserves) != 0 || len(p.saves) != 0) {
			t.Fatal(name, e)
		}
	}
}
