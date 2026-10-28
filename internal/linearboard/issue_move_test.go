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
	moveBacklogA = "a1000000-0000-4000-8000-000000000001"
	moveTodoA    = "a1000000-0000-4000-8000-000000000002"
	moveBacklogB = "b1000000-0000-4000-8000-000000000001"
	moveReadyB   = "b1000000-0000-4000-8000-000000000002"
	moveBugA     = "a2000000-0000-4000-8000-000000000001"
	moveBugB     = "b2000000-0000-4000-8000-000000000001"
	moveDocsA    = "a2000000-0000-4000-8000-000000000002"
)

// moveFake is the V2 fake that also moves: like native Linear, it answers
// with the new team's identifier and keeps the issue's UUID.
type moveFake struct {
	*providerV2
	moves     []linearprovider.MoveInputV2
	children  int
	moveErr   error
	validErr  error
	afterMove func(*linearprovider.IssueV2)
}

func (p *moveFake) ValidateMoveV2(linearprovider.MoveInputV2) error { return p.validErr }
func (p *moveFake) SubIssues(context.Context, string) (int, error) {
	p.tools++
	return p.children, nil
}
func (p *moveFake) MoveV2(_ context.Context, in linearprovider.MoveInputV2) (linearprovider.IssueV2, error) {
	p.tools++
	p.writes++
	p.moves = append(p.moves, in)
	if p.moveErr != nil {
		return linearprovider.IssueV2{}, p.moveErr
	}
	i := p.issues[in.ID]
	i.TeamID, i.StateID, i.Identifier = in.TeamID, in.StateID, "NEW-158"
	i.LabelNames = nil
	for _, l := range p.labels {
		if contains(in.Labels, l.ID) {
			i.LabelNames = append(i.LabelNames, l.Name)
		}
	}
	if in.ClearCycle {
		i.CycleSet = false
	}
	if p.afterMove != nil {
		p.afterMove(&i)
	}
	p.issues[in.ID] = i
	return p.named(i), nil
}

// moveAuthority answers the server's side of a move: who may confirm (the
// owner without a session, or the coordinator), the reservation and the
// settle.
type moveAuthority struct {
	*authorityV2
	coordinator bool
	reserves    []map[string]any
	settles     []map[string]any
	reserveErr  error
	replay      map[string]any
}

func (a *moveAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "taskboard_access":
		result, e := a.authorityV2.Execute(ctx, r)
		access := result.Data.(bus.TaskboardAccess)
		access.Coordinator = a.coordinator && r.Args["coordinator_scope"] == 1
		result.Data = access
		return result, e
	case "issue_move_reserve":
		if a.reserveErr != nil {
			return bus.Result{}, a.reserveErr
		}
		a.reserves = append(a.reserves, r.Args)
		if a.replay != nil {
			return bus.Result{Source: "local-authority", Data: a.replay}, nil
		}
		by := "owner"
		if a.actor != "" {
			by = "coordinator"
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": "reserved", "by": by, "lane": a.lane, "replay": false}}, nil
	case "issue_move_settle":
		a.settles = append(a.settles, r.Args)
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": r.Args["outcome"]}}, nil
	}
	return a.authorityV2.Execute(ctx, r)
}

// moveTest is a team board on teamID and otherTeamID, the owner without a
// session, and a native-shaped issue in teamID's Todo with label Bug, no
// project, parent, cycle or sub-issues.
func moveTest(t *testing.T) (*Service, *moveAuthority, *moveFake, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	a.b.Mode, a.b.LinearProjectIDs = "team_board", []string{}
	a.actor, a.name = "", ""
	scope.Session = ""
	scope.Args = map[string]any{}
	p.stateByName = true
	p.statuses = []linearprovider.Status{
		{ID: moveBacklogA, Name: "Backlog", TeamID: teamID, Type: "backlog"}, {ID: moveTodoA, Name: "Todo", TeamID: teamID, Type: "unstarted"},
		{ID: moveBacklogB, Name: "Backlog", TeamID: otherTeamID, Type: "backlog"}, {ID: moveReadyB, Name: "Ready", TeamID: otherTeamID, Type: "unstarted"},
	}
	p.labels = []linearprovider.Label{
		{ID: moveBugA, Name: "Bug", TeamID: teamID, EligibilityKnown: true}, {ID: moveDocsA, Name: "Docs", TeamID: teamID, EligibilityKnown: true},
		{ID: moveBugB, Name: "Bug", TeamID: otherTeamID, EligibilityKnown: true},
	}
	two := 2
	p.issues[issueID] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueID, Identifier: "OLD-1", TeamID: teamID, StateID: moveTodoA}, ProjectKnown: true, LabelNames: []string{"Bug"}, ParentKnown: true, AssigneeKnown: true, Priority: &two, PriorityKnown: true, CycleKnown: true, CycleSet: true, EstimateKnown: true, UpdatedAt: "2026-10-28T12:00:00.000Z"}
	f := &moveFake{providerV2: p}
	auth := &moveAuthority{authorityV2: a}
	s.cfg.Executor = auth
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return f, nil }
	return s, auth, f, scope
}

func (s *Service) previewMove(t *testing.T, scope bus.Request) MoveResultV2 {
	t.Helper()
	r, e := s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: issueID, TeamID: otherTeamID})
	if e != nil {
		t.Fatal(e)
	}
	return r
}

// A preview sends nothing and maps the state by type, the label by name,
// and clears the cycle; the confirm reserves before it sends one move with
// those IDs, reads it back as accepted, and settles with the identifiers.
func TestMovePreviewThenConfirm(t *testing.T) {
	s, a, p, scope := moveTest(t)
	r := s.previewMove(t, scope)
	plan := r.Plan
	if r.Outcome != "preview" || p.writes != 0 || len(a.reserves) != 0 || plan.ToStateID != moveReadyB || plan.StateBy != "type" || len(plan.Labels) != 1 || plan.Labels[0].ToID != moveBugB || plan.Cycle != "cleared" || plan.Estimate != "none" || len(plan.Refusals) != 0 || !strings.Contains(r.Note, "owner or a project lead") {
		t.Fatal(r)
	}
	p.after = nil
	got, e := s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: issueID, TeamID: otherTeamID, OperationID: "mv-1", Confirm: true, ExpectedPlanSHA256: plan.PlanSHA256})
	if e != nil || got.Outcome != "accepted" || got.NewIdentifier != "NEW-158" || got.By != "owner" || len(got.Unverified) != 0 {
		t.Fatal(got, e)
	}
	if len(p.moves) != 1 || p.moves[0].TeamID != otherTeamID || p.moves[0].StateID != moveReadyB || len(p.moves[0].Labels) != 1 || p.moves[0].Labels[0] != moveBugB || !p.moves[0].ClearCycle || p.moves[0].ClearEstimate {
		t.Fatal(p.moves)
	}
	if len(a.reserves) != 1 || a.reserves[0]["plan_sha256"] != plan.PlanSHA256 || a.reserves[0]["from_team"] != teamID || a.reserves[0]["to_team"] != otherTeamID {
		t.Fatal(a.reserves)
	}
	if len(a.settles) != 1 || a.settles[0]["outcome"] != "accepted" || a.settles[0]["old_identifier"] != "OLD-1" || a.settles[0]["new_identifier"] != "NEW-158" {
		t.Fatal(a.settles)
	}
}

// The plan changed since the preview, a missing digest or operation ID,
// and a server refusal (unproven, or not the owner's) all send nothing.
func TestMoveConfirmRefusedBeforeSending(t *testing.T) {
	s, a, p, scope := moveTest(t)
	plan := s.previewMove(t, scope).Plan
	confirm := MoveInputV2{IssueID: issueID, TeamID: otherTeamID, OperationID: "mv-1", Confirm: true, ExpectedPlanSHA256: plan.PlanSHA256}
	i := p.issues[issueID]
	i.UpdatedAt = "2026-10-28T12:05:00.000Z"
	p.issues[issueID] = i
	if _, e := s.MoveIssueV2(context.Background(), scope, confirm); e == nil || !strings.Contains(e.Error(), "plan changed since the preview") {
		t.Fatal("a changed issue:", e)
	}
	confirm.ExpectedPlanSHA256 = s.previewMove(t, scope).Plan.PlanSHA256
	for _, in := range []MoveInputV2{{IssueID: issueID, TeamID: otherTeamID, Confirm: true, ExpectedPlanSHA256: confirm.ExpectedPlanSHA256}, {IssueID: issueID, TeamID: otherTeamID, OperationID: "x", Confirm: true}} {
		if _, e := s.MoveIssueV2(context.Background(), scope, in); e == nil {
			t.Fatal("an incomplete confirm:", in)
		}
	}
	a.reserveErr = errors.New("issue_move_unproven: this server confirms no team move")
	if _, e := s.MoveIssueV2(context.Background(), scope, confirm); e == nil || !strings.Contains(e.Error(), "unproven") {
		t.Fatal("an unproven server:", e)
	}
	a.reserveErr = nil
	p.validErr = errors.New("capability_unavailable")
	if _, e := s.MoveIssueV2(context.Background(), scope, confirm); e == nil || !strings.Contains(e.Error(), "can't express") || len(a.reserves) != 0 {
		t.Fatal("a provider that can't move reserved:", e, a.reserves)
	}
	if p.writes != 0 || len(a.settles) != 0 {
		t.Fatal(p.writes, a.settles)
	}
}

// The plan refuses, and lists, what it can't map or keep whole: a label
// with no target, a state with no single match, a sub-issue, a parent, a
// parent read that didn't say, a project without the target team; a
// confirm of a refused plan sends nothing.
func TestMovePlanRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(*moveFake)
		want  string
	}{
		"label": {func(p *moveFake) {
			i := p.issues[issueID]
			i.LabelNames = []string{"Bug", "Docs"}
			p.issues[issueID] = i
		}, "no label for: Docs"},
		"state": {func(p *moveFake) {
			p.statuses = append(p.statuses, linearprovider.Status{ID: "b1000000-0000-4000-8000-000000000003", Name: "Next", TeamID: otherTeamID, Type: "unstarted"})
		}, "no single target state"},
		"children":       {func(p *moveFake) { p.children = 2 }, "sub-issues"},
		"parent":         {func(p *moveFake) { i := p.issues[issueID]; i.ParentIdentifier = "OLD-0"; p.issues[issueID] = i }, "is a sub-issue"},
		"parent unknown": {func(p *moveFake) { i := p.issues[issueID]; i.ParentKnown = false; p.issues[issueID] = i }, "didn't say whether the issue has a parent"},
		"project":        {func(p *moveFake) { i := p.issues[issueID]; i.ProjectID = projectA; p.issues[issueID] = i }, "project doesn't include team"},
		"triage":         {func(p *moveFake) { p.statuses[1].Type, p.statuses[3].Type = "triage", "started" }, "no triage"},
		"ambiguous label": {func(p *moveFake) {
			p.labels = append(p.labels, linearprovider.Label{ID: "b2000000-0000-4000-8000-000000000002", Name: "Bug", TeamID: otherTeamID, EligibilityKnown: true})
		}, "more than one in a team"},
	} {
		s, a, p, scope := moveTest(t)
		c.setup(p)
		r := s.previewMove(t, scope)
		if !strings.Contains(strings.Join(r.Plan.Refusals, "; "), c.want) {
			t.Fatal(name, r.Plan.Refusals)
		}
		_, e := s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: issueID, TeamID: otherTeamID, OperationID: "x", Confirm: true, ExpectedPlanSHA256: r.Plan.PlanSHA256})
		if e == nil || !strings.Contains(e.Error(), c.want) || p.writes != 0 || len(a.reserves) != 0 {
			t.Fatal(name, "confirmed:", e)
		}
	}
	// Same team, and an unbound team, aren't planned at all.
	s, _, _, scope := moveTest(t)
	for _, team := range []string{teamID, "e9000000-0000-4000-8000-000000000009"} {
		if _, e := s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: issueID, TeamID: team}); e == nil {
			t.Fatal("planned a move to", team)
		}
	}
}

// The read-back proves each field; one Linear changed is uncertain, named,
// settled so and never retried; a failed send is uncertain too.
func TestMoveReadBackUncertain(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*linearprovider.IssueV2)
		field  string
	}{
		"labels":   {func(i *linearprovider.IssueV2) { i.LabelNames = nil }, "labels"},
		"state":    {func(i *linearprovider.IssueV2) { i.StateID = moveBacklogB }, "state"},
		"cycle":    {func(i *linearprovider.IssueV2) { i.CycleSet = true }, "cycle"},
		"estimate": {func(i *linearprovider.IssueV2) { three := 3.0; i.Estimate = &three }, "estimate"},
		"priority": {func(i *linearprovider.IssueV2) { one := 1; i.Priority = &one }, "priority"},
		"team":     {func(i *linearprovider.IssueV2) { i.TeamID = teamID }, "team"},
		"same id":  {func(i *linearprovider.IssueV2) { i.Identifier = "OLD-1" }, "identifier"},
	} {
		s, a, p, scope := moveTest(t)
		plan := s.previewMove(t, scope).Plan
		p.afterMove = c.change
		got, e := s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: issueID, TeamID: otherTeamID, OperationID: "mv", Confirm: true, ExpectedPlanSHA256: plan.PlanSHA256})
		if e != nil || got.Outcome != "uncertain" || !contains(got.Unverified, c.field) || len(p.moves) != 1 || a.settles[0]["outcome"] != "uncertain" {
			t.Fatal(name, got, e)
		}
	}
	s, a, p, scope := moveTest(t)
	plan := s.previewMove(t, scope).Plan
	p.moveErr = errors.New("transport")
	got, e := s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: issueID, TeamID: otherTeamID, OperationID: "mv", Confirm: true, ExpectedPlanSHA256: plan.PlanSHA256})
	if e == nil || got.Outcome != "uncertain" || len(p.moves) != 1 || a.settles[0]["outcome"] != "uncertain" {
		t.Fatal("a failed send:", got, e)
	}
	// A replayed reservation that never settled sends nothing again.
	s, a, p, scope = moveTest(t)
	plan = s.previewMove(t, scope).Plan
	a.replay = map[string]any{"state": "reserved", "replay": true, "by": "owner"}
	got, e = s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: issueID, TeamID: otherTeamID, OperationID: "mv", Confirm: true, ExpectedPlanSHA256: plan.PlanSHA256})
	if e != nil || got.Outcome != "uncertain" || p.writes != 0 || !strings.Contains(got.Note, "nothing was sent again") {
		t.Fatal("replay:", got, e)
	}
}

// N1: a session that isn't the coordinator, a coordinator without lanes or
// without a lane, a lane missing the source or the target, and a pin that
// changed between the plan and the send are refused; the coordinator
// inside its lane confirms.
func TestMoveCoordinatorLane(t *testing.T) {
	lanes := []bus.Lane{{Name: "ai", WriteTeamIDs: []string{teamID, otherTeamID}, DefaultTeamID: teamID}, {Name: "one", WriteTeamIDs: []string{teamID}, DefaultTeamID: teamID}, {Name: "other", WriteTeamIDs: []string{otherTeamID}, DefaultTeamID: otherTeamID}}
	setup := func(t *testing.T, coordinator bool, withLanes bool, lane string, teams []string) (*Service, *moveAuthority, *moveFake, bus.Request) {
		s, a, p, scope := moveTest(t)
		a.actor, a.name, scope.Session = "ses", "ses", "ses"
		scope.Args = map[string]any{"instance": "i"}
		a.coordinator = coordinator
		if withLanes {
			a.b.Lanes, a.b.MappingVersion = lanes, 3
			a.lane, a.laneTeams = lane, teams
		}
		return s, a, p, scope
	}
	confirm := func(s *Service, scope bus.Request) (MoveResultV2, error) {
		plan := s.previewMove(t, scope).Plan
		return s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: issueID, TeamID: otherTeamID, OperationID: "mv", Confirm: true, ExpectedPlanSHA256: plan.PlanSHA256})
	}
	for name, c := range map[string]struct {
		coordinator, lanes bool
		lane               string
		teams              []string
		want               string
	}{
		"not the coordinator": {false, true, "ai", []string{teamID, otherTeamID}, "only the project's coordinator"},
		"no lanes":            {true, false, "", nil, "no lanes"},
		"no lane":             {true, true, bus.LaneReadOnly, []string{}, "outside_lane"},
		"target outside":      {true, true, "one", []string{teamID}, "outside_lane"},
		"source outside":      {true, true, "other", []string{otherTeamID}, "outside_lane"},
	} {
		s, a, p, scope := setup(t, c.coordinator, c.lanes, c.lane, c.teams)
		if _, e := confirm(s, scope); e == nil || !strings.Contains(e.Error(), c.want) || p.writes != 0 || len(a.reserves) != 0 {
			t.Fatal(name, e)
		}
	}
	s, a, p, scope := setup(t, true, true, "ai", []string{teamID, otherTeamID})
	if r := s.previewMove(t, scope); !strings.Contains(r.Note, "your lane's teams") {
		t.Fatal(r.Note)
	}
	got, e := confirm(s, scope)
	if e != nil || got.Outcome != "accepted" || got.By != "coordinator" || len(p.moves) != 1 {
		t.Fatal(got, e)
	}
	// The pin changes after the reservation: nothing is sent, and the
	// reservation is settled not_sent.
	s, a, p, scope = setup(t, true, true, "ai", []string{teamID, otherTeamID})
	a.afterAccess = func(n int) {
		if len(a.reserves) == 1 {
			a.lane, a.laneTeams = "one", []string{teamID}
		}
	}
	if _, e = confirm(s, scope); e == nil || !strings.Contains(e.Error(), "lane changed") || p.writes != 0 || len(a.settles) != 1 || a.settles[0]["outcome"] != "not_sent" {
		t.Fatal("a pin changed:", e, p.writes, a.settles)
	}
}
