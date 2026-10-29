package linearboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	// landed applies the move before answering moveErr; vanish then makes
	// the issue unreadable.
	landed, vanish bool
	// childrenErr fails Children; stay lists children Linear doesn't
	// move with their parent.
	childrenErr error
	stay        map[string]bool
	// unlink lists children that follow but lose their parent link.
	unlink map[string]bool
}

func (p *moveFake) ValidateMoveV2(linearprovider.MoveInputV2) error { return p.validErr }
func (p *moveFake) SubIssues(_ context.Context, id string) (int, error) {
	p.tools++
	if p.children > 0 && id == issueID {
		return p.children, nil
	}
	return len(p.kidsOf(id)), nil
}

// kidsOf is every issue whose parent is id, by UUID or identifier.
func (p *moveFake) kidsOf(id string) []linearprovider.IssueV2 {
	parent := p.issues[id]
	var out []linearprovider.IssueV2
	for _, i := range p.issues {
		if i.ID != id && (i.ParentID == id || (i.ParentIdentifier != "" && i.ParentIdentifier == parent.Identifier)) {
			out = append(out, p.named(i))
		}
	}
	slices.SortFunc(out, func(x, y linearprovider.IssueV2) int { return strings.Compare(x.ID, y.ID) })
	return out
}

func (p *moveFake) Children(_ context.Context, id string, max int) ([]linearprovider.IssueV2, error) {
	p.tools++
	kids := p.kidsOf(id)
	if p.childrenErr != nil || len(kids) > max {
		return nil, errors.Join(p.childrenErr, errors.New("result_limit"))
	}
	return kids, nil
}
func (p *moveFake) MoveV2(_ context.Context, in linearprovider.MoveInputV2) (linearprovider.IssueV2, error) {
	p.tools++
	p.writes++
	p.moves = append(p.moves, in)
	if p.moveErr != nil && !p.landed {
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
	// Like Linear, the children follow their parent: the new team, its
	// state of the same type, a new identifier, the same label names.
	for n, kid := range p.kidsOf(in.ID) {
		k := p.issues[kid.ID]
		// Linear names the parent by its identifier, now the new one.
		if k.ParentIdentifier != "" {
			k.ParentIdentifier = i.Identifier
		}
		if p.unlink[k.ID] {
			k.ParentIdentifier = ""
		}
		if p.stay[k.ID] {
			p.issues[k.ID] = k
			continue
		}
		k.TeamID, k.Identifier = in.TeamID, fmt.Sprintf("NEW-C%d", n+1)
		var kind string
		for _, st := range p.statuses {
			if st.ID == k.StateID {
				kind = st.Type
			}
		}
		for _, st := range p.statuses {
			if st.TeamID == in.TeamID && st.Type == kind {
				k.StateID = st.ID
			}
		}
		p.issues[k.ID] = k
	}
	p.issues[in.ID] = i
	if p.vanish {
		delete(p.issues, in.ID)
	}
	if p.moveErr != nil {
		return linearprovider.IssueV2{}, p.moveErr
	}
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
	// records is what the server holds per operation, for status.
	records map[string]map[string]any
}

func (a *moveAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "linear_write_charge" {
		return budgetAnswer(r)
	}
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
		if a.records == nil {
			a.records = map[string]map[string]any{}
		}
		a.records[r.Args["operation_id"].(string)] = map[string]any{"state": "reserved"}
		if a.replay != nil {
			return bus.Result{Source: "local-authority", Data: a.replay}, nil
		}
		by := "owner"
		if a.actor != "" {
			by = "coordinator"
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": "reserved", "by": by, "lane": a.lane, "replay": false}}, nil
	case "issue_move_status":
		if rec, ok := a.records[r.Args["operation_id"].(string)]; ok {
			return bus.Result{Source: "local-authority", Data: rec}, nil
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": "none"}}, nil
	case "issue_move_settle":
		a.settles = append(a.settles, r.Args)
		if a.records != nil {
			a.records[r.Args["operation_id"].(string)] = map[string]any{"state": r.Args["outcome"], "new_identifier": r.Args["new_identifier"]}
		}
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
	due := "2026-11-14"
	p.issues[issueID] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueID, Identifier: "OLD-1", TeamID: teamID, StateID: moveTodoA}, ProjectKnown: true, LabelNames: []string{"Bug"}, ParentKnown: true, AssigneeKnown: true, Priority: &two, PriorityKnown: true, DueDate: &due, DueDateKnown: true, CycleKnown: true, CycleSet: true, EstimateKnown: true, UpdatedAt: "2026-10-28T12:00:00.000Z"}
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
		"labels":           {func(i *linearprovider.IssueV2) { i.LabelNames = nil }, "labels"},
		"state":            {func(i *linearprovider.IssueV2) { i.StateID = moveBacklogB }, "state"},
		"cycle":            {func(i *linearprovider.IssueV2) { i.CycleSet = true }, "cycle"},
		"estimate":         {func(i *linearprovider.IssueV2) { three := 3.0; i.Estimate = &three }, "estimate"},
		"priority":         {func(i *linearprovider.IssueV2) { one := 1; i.Priority = &one }, "priority"},
		"due date":         {func(i *linearprovider.IssueV2) { d := "2026-12-01"; i.DueDate = &d }, "due_date"},
		"due date dropped": {func(i *linearprovider.IssueV2) { i.DueDate, i.DueDateKnown = nil, false }, "due_date"},
		"due date cleared": {func(i *linearprovider.IssueV2) { i.DueDate = nil }, "due_date"},
		"team":             {func(i *linearprovider.IssueV2) { i.TeamID = teamID }, "team"},
		"same id":          {func(i *linearprovider.IssueV2) { i.Identifier = "OLD-1" }, "identifier"},
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

// A failed answer to a sent move is read back by UUID before the settle:
// a definite refusal with the issue still in its team is not_sent; an
// answer that failed otherwise (one that didn't parse, a transport error)
// is accepted when the read shows the move as planned, and uncertain when
// it doesn't or the read fails.
func TestMoveFailedAnswerReadsBack(t *testing.T) {
	refused := &linearprovider.Error{Kind: "provider_rejected"}
	unparsed := &linearprovider.Error{Kind: "scope_metadata_missing"}
	for name, c := range map[string]struct {
		err            error
		landed, vanish bool
		outcome        string
		failed         bool
	}{
		"refused":                  {refused, false, false, "not_sent", true},
		"refused, but it moved":    {refused, true, false, "accepted", false},
		"unparsed answer, moved":   {unparsed, true, false, "accepted", false},
		"unparsed answer, unmoved": {unparsed, false, false, "uncertain", true},
		"transport, moved":         {errors.New("transport"), true, false, "accepted", false},
		"read-back fails":          {unparsed, true, true, "uncertain", true},
	} {
		s, a, p, scope := moveTest(t)
		plan := s.previewMove(t, scope).Plan
		p.moveErr, p.landed, p.vanish = c.err, c.landed, c.vanish
		got, e := s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: issueID, TeamID: otherTeamID, OperationID: "mv", Confirm: true, ExpectedPlanSHA256: plan.PlanSHA256})
		if got.Outcome != c.outcome || (e != nil) != c.failed || len(p.moves) != 1 || len(a.settles) != 1 || a.settles[0]["outcome"] != c.outcome {
			t.Fatal(name, got, e, a.settles)
		}
		if c.outcome == "accepted" && (got.NewIdentifier != "NEW-158" || a.settles[0]["new_identifier"] != "NEW-158") {
			t.Fatal(name, "the read's identifier is settled", got, a.settles)
		}
		if name == "read-back fails" && !contains(got.Unverified, "read-back") {
			t.Fatal(name, got.Unverified)
		}
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

const (
	moveKidOne = "c9000000-0000-4000-8000-000000000001"
	moveKidTwo = "c9000000-0000-4000-8000-000000000002"
)

// familyTest is moveTest with two sub-issues of the issue, in its team,
// project and state, each with label Bug.
func familyTest(t *testing.T) (*Service, *moveAuthority, *moveFake, bus.Request) {
	t.Helper()
	s, a, p, scope := moveTest(t)
	for n, id := range []string{moveKidOne, moveKidTwo} {
		p.issues[id] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, Identifier: fmt.Sprintf("OLD-%d", n+2), TeamID: teamID, StateID: moveTodoA}, ProjectKnown: true, LabelNames: []string{"Bug"}, ParentKnown: true, ParentIdentifier: "OLD-1", AssigneeKnown: true, UpdatedAt: "2026-10-28T12:00:00.000Z"}
	}
	return s, a, p, scope
}

func (s *Service) family(t *testing.T, scope bus.Request, in MoveInputV2) (MoveResultV2, error) {
	t.Helper()
	in.IssueID, in.TeamID, in.Family = issueID, otherTeamID, true
	return s.MoveIssueV2(context.Background(), scope, in)
}

// A family's failed answer is read back as a single move's is, every
// child included: a definite refusal is not_sent only while the parent
// and each child are still in the source team; an answer that failed
// otherwise but moved the family is accepted, child by child.
func TestMoveFamilyFailedAnswerReadsBack(t *testing.T) {
	refused := &linearprovider.Error{Kind: "provider_rejected"}
	unparsed := &linearprovider.Error{Kind: "scope_metadata_missing"}
	confirm := func(t *testing.T, err error, landed bool, after func(*moveFake)) (MoveResultV2, error, *moveAuthority) {
		s, a, p, scope := familyTest(t)
		r, _ := s.family(t, scope, MoveInputV2{})
		p.moveErr, p.landed = err, landed
		if after != nil {
			after(p)
		}
		got, e := s.family(t, scope, MoveInputV2{OperationID: "fam", Confirm: true, ExpectedPlanSHA256: r.Plan.PlanSHA256})
		if len(p.moves) != 1 || len(a.settles) != 1 {
			t.Fatal("one send, one settle:", p.moves, a.settles)
		}
		return got, e, a
	}
	if got, e, a := confirm(t, refused, false, nil); got.Outcome != "not_sent" || e == nil || a.settles[0]["outcome"] != "not_sent" || a.settles[0]["children"] != nil {
		t.Fatal("refused, all stayed:", got, e, a.settles)
	}
	// The parent reads as unmoved, but a child moved: not not_sent.
	got, e, a := confirm(t, refused, true, func(p *moveFake) {
		p.afterMove = func(i *linearprovider.IssueV2) { i.TeamID = teamID }
	})
	if got.Outcome != "uncertain" || e == nil || a.settles[0]["outcome"] != "uncertain" || len(got.Children) != 2 {
		t.Fatal("refused, a child moved:", got, e, a.settles)
	}
	got, e, a = confirm(t, unparsed, true, nil)
	if got.Outcome != "accepted" || e != nil || a.settles[0]["outcome"] != "accepted" || len(got.Children) != 2 || got.Children[0].Outcome != "accepted" || got.Children[1].Outcome != "accepted" {
		t.Fatal("an unparsed answer, moved:", got, e, a.settles)
	}
	got, e, _ = confirm(t, unparsed, false, nil)
	if got.Outcome != "uncertain" || e == nil {
		t.Fatal("an unparsed answer, unmoved:", got, e)
	}
}

// Without --family a parent is refused and named for it; with it, every
// child is planned as a single move would be, and the confirm sends one
// write, on the parent only, reserves and settles each child, and reads
// each back under the parent's new identifier.
func TestMoveFamilyPreviewThenConfirm(t *testing.T) {
	s, a, p, scope := familyTest(t)
	if r := s.previewMove(t, scope); !strings.Contains(strings.Join(r.Plan.Refusals, ";"), "--family") {
		t.Fatal(r.Plan.Refusals)
	}
	r, e := s.family(t, scope, MoveInputV2{})
	if e != nil || len(r.Plan.Refusals) != 0 || len(r.Plan.Children) != 2 || r.Plan.Children[0].ToStateID != moveReadyB || r.Plan.Children[1].Labels[0].ToID != moveBugB || p.writes != 0 {
		t.Fatal(r, e)
	}
	got, e := s.family(t, scope, MoveInputV2{OperationID: "fam-1", Confirm: true, ExpectedPlanSHA256: r.Plan.PlanSHA256})
	if e != nil || got.Outcome != "accepted" || len(got.Children) != 2 || got.Children[0].Outcome != "accepted" || got.Children[0].NewIdentifier != "NEW-C1" || got.Children[1].OldIdentifier != "OLD-3" {
		t.Fatal(got, e)
	}
	if len(p.moves) != 1 || p.moves[0].ID != issueID {
		t.Fatal("a family is one write, on the parent:", p.moves)
	}
	if kids, _ := a.reserves[0]["children"].([]any); len(kids) != 2 || kids[0] != moveKidOne {
		t.Fatal(a.reserves)
	}
	if kids, _ := a.settles[0]["children"].([]any); len(kids) != 2 || kids[0].(map[string]any)["outcome"] != "accepted" || kids[0].(map[string]any)["new_identifier"] != "NEW-C1" {
		t.Fatal(a.settles)
	}
	// A child changed since the preview changes the family's digest.
	s, _, p, scope = familyTest(t)
	r, _ = s.family(t, scope, MoveInputV2{})
	k := p.issues[moveKidTwo]
	k.UpdatedAt = "2026-10-28T12:09:00.000Z"
	p.issues[moveKidTwo] = k
	if _, e = s.family(t, scope, MoveInputV2{OperationID: "fam-1", Confirm: true, ExpectedPlanSHA256: r.Plan.PlanSHA256}); e == nil || !strings.Contains(e.Error(), "plan changed") || p.writes != 0 {
		t.Fatal("a changed child:", e)
	}
}

// Every child that can't follow as a single move would is a refusal of
// the whole family, named by the child (r6 O4–O8, P1).
func TestMoveFamilyRefusals(t *testing.T) {
	kid := func(id string, change func(*linearprovider.IssueV2)) func(*moveFake) {
		return func(p *moveFake) { k := p.issues[id]; change(&k); p.issues[id] = k }
	}
	for name, c := range map[string]struct {
		setup func(*moveFake)
		want  string
	}{
		"already moved": {kid(moveKidOne, func(k *linearprovider.IssueV2) { k.TeamID = otherTeamID }), "OLD-2: already in the target team"},
		"another team":  {kid(moveKidOne, func(k *linearprovider.IssueV2) { k.TeamID = "e9000000-0000-4000-8000-000000000009" }), "OLD-2: in team"},
		"other project": {kid(moveKidTwo, func(k *linearprovider.IssueV2) { k.ProjectID = projectA }), "OLD-3: in another project"},
		"grandchild": {func(p *moveFake) {
			p.issues["c9000000-0000-4000-8000-000000000003"] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: "c9000000-0000-4000-8000-000000000003", Identifier: "OLD-9", TeamID: teamID}, ParentKnown: true, ParentID: moveKidOne}
		}, "OLD-2: has sub-issues of its own"},
		"label": {kid(moveKidOne, func(k *linearprovider.IssueV2) { k.LabelNames = []string{"Docs"} }), "OLD-2: the target team has no label for: Docs"},
	} {
		s, a, p, scope := familyTest(t)
		c.setup(p)
		r, e := s.family(t, scope, MoveInputV2{})
		if e != nil || !strings.Contains(strings.Join(r.Plan.Refusals, "; "), c.want) {
			t.Fatal(name, r.Plan.Refusals, e)
		}
		if _, e = s.family(t, scope, MoveInputV2{OperationID: "x", Confirm: true, ExpectedPlanSHA256: r.Plan.PlanSHA256}); e == nil || p.writes != 0 || len(a.reserves) != 0 {
			t.Fatal(name, "confirmed:", e)
		}
	}
	// A listing that can't be read to its end plans nothing.
	s, _, p, scope := familyTest(t)
	p.childrenErr = errors.New("pagination_unavailable")
	if _, e := s.family(t, scope, MoveInputV2{}); e == nil || !strings.Contains(e.Error(), "listed to the end") {
		t.Fatal(e)
	}
	// A family is named by its parent: a child asked for is refused.
	s, _, p, scope = familyTest(t)
	r, e := s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: moveKidOne, TeamID: otherTeamID, Family: true})
	if e != nil || !strings.Contains(strings.Join(r.Plan.Refusals, ";"), "named by its parent") {
		t.Fatal(r.Plan.Refusals, e)
	}
}

// A child that didn't follow makes the child and the family uncertain,
// settled so with every child named; the move-back is listed; nothing is
// retried and no child is written.
func TestMoveFamilyChildDidNotFollow(t *testing.T) {
	s, a, p, scope := familyTest(t)
	r, _ := s.family(t, scope, MoveInputV2{})
	p.stay = map[string]bool{moveKidTwo: true}
	got, e := s.family(t, scope, MoveInputV2{OperationID: "fam-1", Confirm: true, ExpectedPlanSHA256: r.Plan.PlanSHA256})
	if e != nil || got.Outcome != "uncertain" || got.Children[0].Outcome != "accepted" || got.Children[1].Outcome != "uncertain" || !contains(got.Children[1].Unverified, "team") || !contains(got.Unverified, "children") || !strings.Contains(got.Note, "--team "+teamID+" --family") {
		t.Fatal(got, e)
	}
	if len(p.moves) != 1 || a.settles[0]["outcome"] != "uncertain" {
		t.Fatal(p.moves, a.settles)
	}
	if kids, _ := a.settles[0]["children"].([]any); len(kids) != 2 || kids[1].(map[string]any)["outcome"] != "uncertain" {
		t.Fatal(a.settles)
	}
	// A child that follows without its parent link is uncertain on parent.
	s, _, p, scope = familyTest(t)
	r, _ = s.family(t, scope, MoveInputV2{})
	p.unlink = map[string]bool{moveKidOne: true}
	if got, e = s.family(t, scope, MoveInputV2{OperationID: "fam-3", Confirm: true, ExpectedPlanSHA256: r.Plan.PlanSHA256}); e != nil || got.Outcome != "uncertain" || !contains(got.Children[0].Unverified, "parent") {
		t.Fatal(got, e)
	}
	// A failed send settles every child uncertain too.
	s, a, p, scope = familyTest(t)
	r, _ = s.family(t, scope, MoveInputV2{})
	p.moveErr = errors.New("transport")
	if got, e = s.family(t, scope, MoveInputV2{OperationID: "fam-2", Confirm: true, ExpectedPlanSHA256: r.Plan.PlanSHA256}); e == nil || got.Outcome != "uncertain" {
		t.Fatal(got, e)
	}
	if kids, _ := a.settles[0]["children"].([]any); len(kids) != 2 || kids[0].(map[string]any)["outcome"] != "uncertain" {
		t.Fatal(a.settles)
	}
}

const moveSolo = "c9000000-0000-4000-8000-0000000000aa"

// batchTest is familyTest plus a lone issue in teamID.
func batchTest(t *testing.T) (*Service, *moveAuthority, *moveFake, bus.Request) {
	t.Helper()
	s, a, p, scope := familyTest(t)
	p.issues[moveSolo] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: moveSolo, Identifier: "OLD-9", TeamID: teamID, StateID: moveTodoA}, ProjectKnown: true, LabelNames: []string{"Bug"}, ParentKnown: true, AssigneeKnown: true, UpdatedAt: "2026-10-28T12:00:00.000Z"}
	return s, a, p, scope
}

func (s *Service) batch(t *testing.T, scope bus.Request, sha string, issues ...string) (BatchMoveResultV2, error) {
	t.Helper()
	return s.BatchMoveV2(context.Background(), scope, BatchMoveInputV2{TeamID: otherTeamID, Issues: issues, OperationID: "b-1", Confirm: sha != "", ExpectedPlanSHA256: sha})
}

// The preview plans each issue (a parent as its family) and counts the
// children toward the bound; the confirm moves them one by one, each under
// OP:<index>; a child named in the batch refuses it all (P1).
func TestMoveBatchPreviewThenConfirm(t *testing.T) {
	s, a, p, scope := batchTest(t)
	if r, _ := s.batch(t, scope, "", moveSolo, issueID, moveKidOne); len(r.Refusals) == 0 || !strings.Contains(r.Refusals[0], "issue 2:") {
		t.Fatal("a child named too:", r.Refusals)
	}
	r, e := s.batch(t, scope, "", moveSolo, issueID)
	if e != nil || r.Outcome != "preview" || r.Issues != 4 || !r.Items[1].Family || r.Items[0].Status != "plan" || len(r.Refusals) != 0 || p.writes != 0 {
		t.Fatal(r, e)
	}
	got, e := s.batch(t, scope, r.PlanSHA256, moveSolo, issueID)
	if e != nil || got.Outcome != "accepted" || got.Items[0].Outcome != "accepted" || got.Items[1].Outcome != "accepted" || len(got.Items[1].Children) != 2 || len(p.moves) != 2 {
		t.Fatal(got, e)
	}
	if a.reserves[0]["operation_id"] != "b-1:0" || a.reserves[1]["operation_id"] != "b-1:1" {
		t.Fatal(a.reserves)
	}
	// Run again: every item answers from the record; nothing is sent.
	again, _ := s.batch(t, scope, "", moveSolo, issueID)
	if again.Items[0].Status != "done" || again.Items[1].Status != "done" {
		t.Fatal(again.Items)
	}
	if done, e := s.batch(t, scope, again.PlanSHA256, moveSolo, issueID); e != nil || done.Outcome != "accepted" || len(p.moves) != 2 {
		t.Fatal("resume of a done batch:", done, e)
	}
	// Bounds and a changed batch.
	many := make([]string, maxBatchIssues+1)
	for i := range many {
		many[i] = fmt.Sprintf("c8000000-0000-4000-8000-%012d", i)
	}
	if _, e = s.batch(t, scope, "", many...); e == nil {
		t.Fatal("more than the bound")
	}
	s, _, p, scope = batchTest(t)
	r, _ = s.batch(t, scope, "", moveSolo)
	i := p.issues[moveSolo]
	i.UpdatedAt = "2026-10-28T12:30:00.000Z"
	p.issues[moveSolo] = i
	if _, e = s.batch(t, scope, r.PlanSHA256, moveSolo); e == nil || !strings.Contains(e.Error(), "changed since the preview") || p.writes != 0 {
		t.Fatal("a changed batch:", e)
	}
}

// The first issue that isn't accepted stops the batch; nothing after it is
// sent. The resume holds an unacknowledged uncertain item (P2), then, once
// acknowledged, reads the issue back as already moved and goes on.
func TestMoveBatchStopsAndResumes(t *testing.T) {
	s, a, p, scope := batchTest(t)
	r, _ := s.batch(t, scope, "", moveSolo, issueID)
	p.afterMove = func(i *linearprovider.IssueV2) {
		if i.ID == moveSolo {
			i.LabelNames = nil
		}
	}
	got, e := s.batch(t, scope, r.PlanSHA256, moveSolo, issueID)
	if e != nil || got.Outcome != "stopped" || got.StoppedAt == nil || *got.StoppedAt != 0 || got.Items[0].Outcome != "uncertain" || got.Items[1].Outcome != "" || len(p.moves) != 1 || !strings.Contains(got.Note, "linear writes --ack b-1:0") {
		t.Fatal(got, e)
	}
	p.afterMove = nil
	held, _ := s.batch(t, scope, "", moveSolo, issueID)
	if held.Items[0].Status != "refused" || !strings.Contains(held.Refusals[0], "uncertain") {
		t.Fatal("an unacknowledged uncertain item:", held.Items[0])
	}
	if _, e = s.batch(t, scope, held.PlanSHA256, moveSolo, issueID); e == nil || len(p.moves) != 1 {
		t.Fatal("resumed past an uncertain item:", e)
	}
	a.records["b-1:0"]["acknowledged"] = true
	resume, _ := s.batch(t, scope, "", moveSolo, issueID)
	if resume.Items[0].Status != "already_moved" || resume.Items[1].Status != "plan" {
		t.Fatal(resume.Items)
	}
	done, e := s.batch(t, scope, resume.PlanSHA256, moveSolo, issueID)
	if e != nil || done.Outcome != "accepted" || done.Items[1].Outcome != "accepted" || len(p.moves) != 2 {
		t.Fatal(done, e)
	}
}

// J2: an acknowledged uncertain item whose issue isn't in the target team
// is refused, not planned again under its spent operation ID.
func TestMoveBatchAcknowledgedUncertainNotMovedIsRefused(t *testing.T) {
	s, a, p, scope := batchTest(t)
	r, _ := s.batch(t, scope, "", moveSolo, issueID)
	p.afterMove = func(i *linearprovider.IssueV2) {
		if i.ID == moveSolo {
			i.LabelNames = nil
		}
	}
	if got, _ := s.batch(t, scope, r.PlanSHA256, moveSolo, issueID); got.Items[0].Outcome != "uncertain" {
		t.Fatal(got)
	}
	p.afterMove = nil
	a.records["b-1:0"]["acknowledged"] = true
	back := p.issues[moveSolo]
	back.TeamID = teamID
	p.issues[moveSolo] = back
	held, _ := s.batch(t, scope, "", moveSolo, issueID)
	if held.Items[0].Status != "refused" || len(held.Refusals) != 1 || !strings.Contains(held.Refusals[0], "new operation ID") {
		t.Fatal(held.Items[0], held.Refusals)
	}
	if _, e := s.batch(t, scope, held.PlanSHA256, moveSolo, issueID); e == nil || len(p.moves) != 1 || len(a.reserves) != 1 {
		t.Fatal("re-planned under a spent operation ID:", e, len(a.reserves))
	}
}

// O2/P4: the confirm tells the server its estimated calls, a family's
// children included; a budget refusal sends nothing and stops a batch
// there, retryable with the same operation ID.
func TestMoveBudget(t *testing.T) {
	s, a, p, scope := familyTest(t)
	r, _ := s.family(t, scope, MoveInputV2{})
	if _, e := s.family(t, scope, MoveInputV2{OperationID: "fam-1", Confirm: true, ExpectedPlanSHA256: r.Plan.PlanSHA256}); e != nil {
		t.Fatal(e)
	}
	if calls, _ := a.reserves[0]["calls"].(int); calls != r.Plan.Calls+2+len(r.Plan.Children)+1 {
		t.Fatal("estimate", a.reserves[0]["calls"], r.Plan.Calls)
	}
	s, a, p, scope = batchTest(t)
	b, _ := s.batch(t, scope, "", moveSolo, issueID)
	a.reserveErr = bus.PublicError("rate_limited", "the project's hourly Linear budget has 3 of 200 calls left, and this move needs 9; retry after 2026-10-29T05:00:00Z; nothing was sent")
	got, e := s.batch(t, scope, b.PlanSHA256, moveSolo, issueID)
	if e != nil || got.Outcome != "stopped" || *got.StoppedAt != 0 || got.Items[0].Outcome != "not_sent" || !strings.Contains(got.Items[0].Note, "retry after") || !strings.Contains(got.Note, "same --operation-id") || p.writes != 0 {
		t.Fatal(got, e)
	}
	// Once the hour rolls, the same batch goes on from where it stopped.
	a.reserveErr = nil
	again, _ := s.batch(t, scope, "", moveSolo, issueID)
	if done, e := s.batch(t, scope, again.PlanSHA256, moveSolo, issueID); e != nil || done.Outcome != "accepted" || len(p.moves) != 2 {
		t.Fatal(done, e)
	}
}

// A move into a bound team the enabled status policy has no entry for says
// so in its answer's note, appended to a note already set; a team the
// policy has, a disabled policy, or a policy that can't be read adds
// nothing and never changes the move.
func TestMoveNotesATeamMissingFromTheStatusPolicy(t *testing.T) {
	note := otherTeamID + " is not in the status policy, so claims and done-releases there won't move its state: the project owner runs `clawbridge linear status-policy set` with the file {\"enabled\": true}"
	confirm := func(status map[string]any, statusErr error) MoveResultV2 {
		s, a, _, scope := moveTest(t)
		a.status, a.statusErr = status, statusErr
		plan := s.previewMove(t, scope).Plan
		got, e := s.MoveIssueV2(context.Background(), scope, MoveInputV2{IssueID: issueID, TeamID: otherTeamID, OperationID: "mv-1", Confirm: true, ExpectedPlanSHA256: plan.PlanSHA256})
		if e != nil || got.Outcome != "accepted" || got.NewIdentifier != "NEW-158" {
			t.Fatal(got, e)
		}
		return got
	}
	enabled := func(teams map[string]any) map[string]any {
		return map[string]any{"revision": 1, "enabled": true, "teams": teams}
	}
	if got := confirm(enabled(map[string]any{teamID: map[string]any{}}), nil); got.Note != note {
		t.Fatalf("note = %q", got.Note)
	}
	if got := confirm(enabled(map[string]any{teamID: map[string]any{}, otherTeamID: map[string]any{}}), nil); got.Note != "" {
		t.Fatal("a team in the policy was noted:", got.Note)
	}
	if got := confirm(map[string]any{"revision": 1, "enabled": false, "teams": map[string]any{}}, nil); got.Note != "" {
		t.Fatal("a disabled policy was noted:", got.Note)
	}
	if got := confirm(nil, errors.New("down")); got.Note != "" {
		t.Fatal("an unreadable policy was noted:", got.Note)
	}
	// A family that did not wholly follow already has a note: this is added.
	s, a, p, scope := familyTest(t)
	a.status = enabled(map[string]any{teamID: map[string]any{}})
	r, _ := s.family(t, scope, MoveInputV2{})
	p.stay = map[string]bool{moveKidTwo: true}
	got, e := s.family(t, scope, MoveInputV2{OperationID: "fam-1", Confirm: true, ExpectedPlanSHA256: r.Plan.PlanSHA256})
	if e != nil || !strings.Contains(got.Note, "--team "+teamID+" --family") || !strings.HasSuffix(got.Note, "; "+note) {
		t.Fatal(got.Note, e)
	}
}
