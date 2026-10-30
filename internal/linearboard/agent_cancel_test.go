package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	acGroup       = "d1000000-0000-4000-8000-000000000001"
	acEngineering = "d1000000-0000-4000-8000-000000000002"
	acResearch    = "d1000000-0000-4000-8000-000000000003"
	acOtherGroup  = "d1000000-0000-4000-8000-000000000004"
	acOriginal    = "d1000000-0000-4000-8000-000000000005"
)

// cancelTest is a hosted server's board (ProjectKindLabels set) whose
// issue sits in projectA at state, projectA of kind Engineering, and a
// status policy that lets agents cancel in projects of kind Engineering.
// The returned labels are the project's as Linear answers them.
func cancelTest(t *testing.T, state string) (*Service, *authorityV2, *providerV2, bus.Request, *[]ProjectLabelRef) {
	t.Helper()
	s, a, p, scope := closeTest(t, projectA, state)
	a.status["agent_may_cancel_kinds"] = []string{"Engineering"}
	a.kind = map[string]any{"revision": 2, "group_label_id": acGroup, "values": []map[string]any{{"name": "Engineering", "label_id": acEngineering}, {"name": "Research", "label_id": acResearch}}}
	labels := &[]ProjectLabelRef{{ID: acEngineering, ParentID: acGroup}, {ID: labelA}}
	s.cfg.ProjectKindLabels = func(_ context.Context, project string) ([]ProjectLabelRef, error) {
		if project != projectA {
			return nil, errors.New("unexpected project")
		}
		return *labels, nil
	}
	return s, a, p, scope, labels
}

var acOp = 0

func acCancel(s *Service, scope bus.Request, reason string) (any, error) {
	acOp++
	return s.CancelV2(context.Background(), scope, CancelInputV2{Operation: "cancel", OperationID: "op-ac-" + strings.Repeat("c", acOp), IssueID: issueID, Reason: reason})
}

// nothingSent fails unless err refuses with code before any Linear write.
func nothingSent(t *testing.T, name string, err error, p *providerV2, code string) {
	t.Helper()
	if err == nil || p.writes != 0 || len(p.bodies) != 0 || len(p.updated) != 0 || len(p.edges) != 0 {
		t.Fatal(name, "not refused before Linear:", err, p.writes, p.bodies, p.updated, p.edges)
	}
	if !strings.HasPrefix(err.Error(), code+": ") || !strings.Contains(err.Error(), "Nothing was sent") {
		t.Fatal(name, "refusal is not", code, ":", err)
	}
}

// An agent cancels an issue of an allowed kind: the reason is a marked
// comment naming its session, then the team's canceled state; the bus
// records it, and the answer says what was done and what comes next.
func TestAgentCancelsWhereThePersonAllowedTheKind(t *testing.T) {
	s, a, p, scope, _ := cancelTest(t, sfProgress)
	out, err := acCancel(s, scope, "superseded by the new design")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.bodies) != 1 || !strings.HasPrefix(p.bodies[0], "Canceled: superseded by the new design\n\n") || !strings.Contains(p.bodies[0], "session ses") {
		t.Fatal("comment", p.bodies)
	}
	if moves := stateMoves(p); len(moves) != 1 || moves[0] != cgCanceled {
		t.Fatal("state", moves)
	}
	r := out.(CancelResultV2)
	if r.Kind != "cancel" || r.StateLanded != cgCanceled || r.StateSetBy != "clawbridge" || r.CommentID == "" || r.ProjectKind != "Engineering" || r.Recorded == nil || !*r.Recorded || !strings.Contains(r.Next, "without an outcome") {
		t.Fatalf("%+v", r)
	}
	if len(a.records) != 1 {
		t.Fatal("records", a.records)
	}
	rec := a.records[0]
	if rec["action"] != "canceled" || rec["issue"] != issueID || rec["reason"] != "superseded by the new design" || rec["kind"] != "Engineering" || rec["linear_project"] != projectA || rec["instance"] != "secret-instance" {
		t.Fatal(rec)
	}
}

// A record that fails never fails the cancel: it is answered as
// recorded: false.
func TestAFailedRecordLeavesTheCancel(t *testing.T) {
	s, a, p, scope, _ := cancelTest(t, sfProgress)
	a.recordErr = errors.New("bus down")
	out, err := acCancel(s, scope, "dup work")
	if err != nil || len(stateMoves(p)) != 1 {
		t.Fatal(err, stateMoves(p))
	}
	if r := out.(CancelResultV2); r.Recorded == nil || *r.Recorded {
		t.Fatalf("%+v", r)
	}
}

// The kind is read from Linear: another kind is not allowed, and no kind
// label, two of them, a read that fails, or an issue in no project is
// unknown; nothing is sent.
func TestAgentCancelRefusesAKindNotAllowedOrUnknown(t *testing.T) {
	s, _, p, scope, labels := cancelTest(t, sfProgress)
	*labels = []ProjectLabelRef{{ID: acResearch, ParentID: acGroup}}
	_, err := acCancel(s, scope, "r")
	nothingSent(t, "research", err, p, "kind_not_allowed")
	for name, set := range map[string][]ProjectLabelRef{
		"no kind label":                      {{ID: labelA}},
		"two kind labels":                    {{ID: acEngineering, ParentID: acGroup}, {ID: acResearch, ParentID: acGroup}},
		"a kind label's ID in another group": {{ID: acEngineering, ParentID: acOtherGroup}},
	} {
		s, _, p, scope, labels = cancelTest(t, sfProgress)
		*labels = set
		_, err = acCancel(s, scope, "r")
		nothingSent(t, name, err, p, "kind_unknown")
	}
	s, _, p, scope, _ = cancelTest(t, sfProgress)
	s.cfg.ProjectKindLabels = func(context.Context, string) ([]ProjectLabelRef, error) { return nil, errors.New("graphql") }
	_, err = acCancel(s, scope, "r")
	nothingSent(t, "read error", err, p, "kind_unknown")
	s, a, p, scope, _ := cancelTest(t, sfProgress)
	a.kind["group_label_id"] = nil
	_, err = acCancel(s, scope, "r")
	nothingSent(t, "kinds not seeded", err, p, "kind_unknown")
	// An issue in no project, on a team board, and one whose project the
	// binding doesn't list.
	s, a, p, scope, _ = cancelTest(t, sfProgress)
	a.b.Mode, a.b.LinearProjectIDs = "team_board", nil
	issue := p.issues[issueID]
	issue.ProjectID = ""
	p.issues[issueID] = issue
	_, err = acCancel(s, scope, "r")
	nothingSent(t, "no project", err, p, "kind_unknown")
	s, _, p, scope, _ = cancelTest(t, sfProgress)
	b := bus.TaskboardBindingV2{LinearProjectIDs: []string{projectB}}
	if _, err = s.issueKind(context.Background(), scope, b, p.issues[issueID]); err == nil || !strings.HasPrefix(err.Error(), "kind_unknown") {
		t.Fatal("unbound project", err)
	}
}

// On a team board the binding lists no Linear project, so an issue in a
// project that carries an allowed kind label is still of no kind the
// person allowed: the kind counts only in the binding's own projects.
func TestAgentCancelOnATeamBoardIsKindUnknown(t *testing.T) {
	s, a, p, scope, _ := cancelTest(t, sfProgress)
	a.b.Mode, a.b.LinearProjectIDs = "team_board", nil
	if p.issues[issueID].ProjectID == "" {
		t.Fatal("the issue must sit in a project carrying the allowed kind label")
	}
	_, err := acCancel(s, scope, "r")
	nothingSent(t, "team board", err, p, "kind_unknown")
}

// No kind listed, or the policy off: agents never cancel.
func TestAgentCancelOffWithoutAllowedKinds(t *testing.T) {
	s, a, p, scope, _ := cancelTest(t, sfProgress)
	a.status["agent_may_cancel_kinds"] = []string{}
	_, err := acCancel(s, scope, "r")
	nothingSent(t, "empty", err, p, "cancel_off")
	s, a, p, scope, _ = cancelTest(t, sfProgress)
	a.status["enabled"] = false
	_, err = acCancel(s, scope, "r")
	nothingSent(t, "disabled", err, p, "cancel_off")
	s, a, p, scope, _ = cancelTest(t, sfProgress)
	a.status = nil
	_, err = acCancel(s, scope, "r")
	nothingSent(t, "no policy", err, p, "cancel_off")
}

// On a member's own profile (no ProjectKindLabels) a session's cancel is
// refused even with the kind allowed; a person's goes anywhere the binding
// lets them write, with no kind or setting.
func TestPersonalPathRefusesAnAgentButNotAPerson(t *testing.T) {
	s, _, p, scope, _ := cancelTest(t, sfProgress)
	s.cfg.ProjectKindLabels = nil
	_, err := acCancel(s, scope, "r")
	if err == nil || !strings.Contains(err.Error(), "only through the ClawBridge Linear app on a hosted server") || p.writes != 0 {
		t.Fatal(err, p.writes)
	}
	s, a, p, scope, _ := cancelTest(t, sfProgress)
	s.cfg.ProjectKindLabels = nil
	a.actor, a.name = "", ""
	a.status["agent_may_cancel_kinds"] = []string{}
	a.kind = nil
	scope.Session = ""
	out, err := acCancel(s, scope, "not needed")
	if err != nil || len(stateMoves(p)) != 1 || len(p.bodies) != 1 {
		t.Fatal("a person's cancel", err, stateMoves(p), p.bodies)
	}
	if r := out.(CancelResultV2); r.Recorded != nil || len(a.records) != 0 || r.ProjectKind != "" {
		t.Fatalf("a person's cancel was recorded as an agent's: %+v %v", r, a.records)
	}
}

// Never re-opened, never a completed or canceled issue; open states pass.
func TestAgentCancelOnlyFromAnOpenState(t *testing.T) {
	for _, state := range []string{sfDone, cgCanceled, cgDuplicate} {
		s, _, p, scope, _ := cancelTest(t, state)
		_, err := acCancel(s, scope, "r")
		nothingSent(t, state, err, p, "from_state_closed")
	}
	// A person is held to it too.
	s, a, p, scope, _ := cancelTest(t, sfDone)
	a.actor, a.name, scope.Session = "", "", ""
	_, err := acCancel(s, scope, "r")
	nothingSent(t, "a person's", err, p, "from_state_closed")
	for _, state := range []string{sfTriage, sfBacklog, sfTodo, sfProgress} {
		s, _, p, scope, _ := cancelTest(t, state)
		if _, err := acCancel(s, scope, "r"); err != nil || len(stateMoves(p)) != 1 {
			t.Fatal(state, err)
		}
	}
	s, _, p, scope, _ = cancelTest(t, cgUntyped)
	_, err = acCancel(s, scope, "r")
	nothingSent(t, "unknown type", err, p, "state_unknown")
}

// A session outside the issue's team's lane is refused.
func TestAgentCancelHoldsTheLane(t *testing.T) {
	s, a, p, scope, _ := cancelTest(t, sfProgress)
	a.b.Lanes = []bus.Lane{{Name: "core", WriteTeamIDs: []string{teamID}, DefaultTeamID: teamID}, {Name: "web", WriteTeamIDs: []string{otherTeamID}, DefaultTeamID: otherTeamID}}
	a.lane, a.laneTeams = "web", []string{otherTeamID}
	_, err := acCancel(s, scope, "r")
	if err == nil || !strings.Contains(err.Error(), "outside_lane") || p.writes != 0 {
		t.Fatal(err, p.writes)
	}
}

// A retry with the same operation_id posts one comment and sets the state
// once; a state that failed after the comment is completed by the retry.
func TestAgentCancelRetryIsIdempotent(t *testing.T) {
	s, a, p, scope, _ := cancelTest(t, sfProgress)
	in := CancelInputV2{Operation: "cancel", OperationID: "op-ac-retry", IssueID: issueID, Reason: "r"}
	if _, err := s.CancelV2(context.Background(), scope, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelV2(context.Background(), scope, in); err != nil {
		t.Fatal("retry", err)
	}
	if len(p.bodies) != 1 || len(stateMoves(p)) != 1 {
		t.Fatal("sent twice", p.bodies, stateMoves(p))
	}
	if len(a.records) != 2 {
		t.Fatal("the bus dedupes a record by operation_id; each completion asks", a.records)
	}
	s, _, p, scope, _ = cancelTest(t, sfProgress)
	p.preflightErr = errors.New("state write refused")
	in.OperationID = "op-ac-half"
	if _, err := s.CancelV2(context.Background(), scope, in); err == nil || len(p.bodies) != 1 || len(stateMoves(p)) != 0 {
		t.Fatal("half", err, p.bodies, stateMoves(p))
	}
	p.preflightErr = nil
	if _, err := s.CancelV2(context.Background(), scope, in); err != nil || len(p.bodies) != 1 || len(stateMoves(p)) != 1 {
		t.Fatal("completed", err, p.bodies, stateMoves(p))
	}
}

// Validation refuses before anything is read.
func TestCancelAndDuplicateArguments(t *testing.T) {
	for name, in := range map[string]CancelInputV2{
		"no reason":        {Operation: "cancel", OperationID: "op", IssueID: issueID},
		"blank reason":     {Operation: "cancel", OperationID: "op", IssueID: issueID, Reason: "  "},
		"long reason":      {Operation: "cancel", OperationID: "op", IssueID: issueID, Reason: strings.Repeat("x", 2001)},
		"cancel names dup": {Operation: "cancel", OperationID: "op", IssueID: issueID, Reason: "r", DuplicateOf: acOriginal},
		"long op":          {Operation: "cancel", OperationID: strings.Repeat("o", 120), IssueID: issueID, Reason: "r"},
		"step op":          {Operation: "cancel", OperationID: "op:state", IssueID: issueID, Reason: "r"},
		"no duplicate_of":  {Operation: "duplicate", OperationID: "op", IssueID: issueID},
		"self":             {Operation: "duplicate", OperationID: "op", IssueID: issueID, DuplicateOf: issueID},
		"identifier":       {Operation: "duplicate", OperationID: "op", IssueID: issueID, DuplicateOf: "ENG-1"},
	} {
		if validateCancelV2(in) == nil {
			t.Fatal(name, "accepted")
		}
	}
	err := validateCancelV2(CancelInputV2{Operation: "duplicate", OperationID: "op", IssueID: issueID})
	if err == nil || !strings.HasPrefix(err.Error(), "duplicate_of_required: ") {
		t.Fatal(err)
	}
	if validateCancelV2(CancelInputV2{Operation: "cancel", OperationID: strings.Repeat("o", 119), IssueID: issueID, Reason: strings.Repeat("é", 2000)}) != nil {
		t.Fatal("the bounds refused")
	}
	// A caller never writes a step's ID itself.
	s, _, _, scope := testV2(t)
	if _, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "op:comment", IssueID: issueID, Body: "b"}}); err == nil {
		t.Fatal("a step's ID accepted")
	}
}

// duplicateTest is cancelTest with an open original issue in the same
// team.
func duplicateTest(t *testing.T) (*Service, *authorityV2, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope, _ := cancelTest(t, sfProgress)
	p.issues[acOriginal] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: acOriginal, Identifier: "ENG-7", TeamID: teamID, ProjectID: projectA, StateID: sfTodo}, ProjectKnown: true}
	return s, a, p, scope
}

func acDuplicate(s *Service, scope bus.Request, of, reason string) (any, error) {
	acOp++
	return s.CancelV2(context.Background(), scope, CancelInputV2{Operation: "duplicate", OperationID: "op-ac-" + strings.Repeat("d", acOp), IssueID: issueID, DuplicateOf: of, Reason: reason})
}

// A duplicate makes the duplicate edge (from the duplicate to the
// original), posts the optional reason, and sets the team's duplicate
// state when Linear did not move the issue itself.
func TestAgentMarksDuplicate(t *testing.T) {
	s, a, p, scope := duplicateTest(t)
	out, err := acDuplicate(s, scope, acOriginal, "same crash")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.edges) != 1 || p.edges[0] != (linearprovider.RelationInput{Action: "add", Type: "duplicate", FromID: issueID, ToID: acOriginal}) {
		t.Fatal("edge", p.edges)
	}
	if len(p.bodies) != 1 || !strings.HasPrefix(p.bodies[0], "Duplicate of ENG-7: same crash") {
		t.Fatal("comment", p.bodies)
	}
	if moves := stateMoves(p); len(moves) != 1 || moves[0] != cgDuplicate {
		t.Fatal("state", moves)
	}
	r := out.(CancelResultV2)
	if r.Kind != "duplicate" || r.DuplicateOf != acOriginal || r.Relation != "accepted" || r.StateSetBy != "clawbridge" || len(a.records) != 1 || a.records[0]["action"] != "duplicated" || a.records[0]["duplicate_of"] != acOriginal {
		t.Fatalf("%+v %v", r, a.records)
	}
	// Linear moved it itself: no state write. No reason: no comment.
	s, _, p, scope = duplicateTest(t)
	p.after = func() {
		issue := p.issues[issueID]
		issue.StateID = cgDuplicate
		p.issues[issueID] = issue
	}
	out, err = acDuplicate(s, scope, acOriginal, "")
	if err != nil || len(p.bodies) != 0 || len(stateMoves(p)) != 0 || out.(CancelResultV2).StateSetBy != "linear" || out.(CancelResultV2).StateLanded != cgDuplicate {
		t.Fatal(err, p.bodies, stateMoves(p), out)
	}
	// A team with no duplicate state gets its canceled state.
	s, _, p, scope = duplicateTest(t)
	p.statuses = append(append([]linearprovider.Status{}, sfStates...), linearprovider.Status{ID: cgCanceled, Name: "Canceled", TeamID: teamID, Type: "canceled", Raw: "canceled"})
	if _, err = acDuplicate(s, scope, acOriginal, ""); err != nil || stateMoves(p)[0] != cgCanceled {
		t.Fatal(err, stateMoves(p))
	}
}

// The original must be in the binding and open; the agent-facing relation
// tool never makes a duplicate edge.
func TestDuplicateRefusals(t *testing.T) {
	s, _, p, scope := duplicateTest(t)
	_, err := acDuplicate(s, scope, "d1000000-0000-4000-8000-0000000000ff", "")
	if err == nil || p.writes != 0 {
		t.Fatal("unreadable original", err)
	}
	s, _, p, scope = duplicateTest(t)
	original := p.issues[acOriginal]
	original.TeamID, original.ProjectID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", ""
	p.issues[acOriginal] = original
	_, err = acDuplicate(s, scope, acOriginal, "")
	nothingSent(t, "out of scope", err, p, "duplicate_target_out_of_scope")
	for _, state := range []string{sfDone, cgCanceled, cgDuplicate} {
		s, _, p, scope = duplicateTest(t)
		original := p.issues[acOriginal]
		original.StateID = state
		p.issues[acOriginal] = original
		_, err = acDuplicate(s, scope, acOriginal, "")
		nothingSent(t, state, err, p, "duplicate_target_closed")
	}
	s, _, p, scope = duplicateTest(t)
	if _, err = s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "op-ac-rel", Action: "add", Type: "duplicate", FromID: issueID, ToID: acOriginal}); err == nil || len(p.edges) != 0 {
		t.Fatal("a plain duplicate edge", err)
	}
}

// After a cancel the agent releases without an outcome, and the issue
// stays canceled; a done release's state move finds it canceled and
// leaves it (from_state).
func TestADoneReleaseAfterACancelLeavesTheState(t *testing.T) {
	s, _, p, scope, _ := cancelTest(t, sfProgress)
	if _, err := acCancel(s, scope, "r"); err != nil {
		t.Fatal(err)
	}
	issue := p.issues[issueID]
	issue.StateID = cgCanceled
	p.issues[issueID] = issue
	policy := sfPolicy()
	outcome, extra, err := s.stateMove(context.Background(), scope, issueID, "release_done", policy, false, false)
	if err != nil || outcome != "skipped" || extra["skip_reason"] != "from_state" || len(stateMoves(p)) != 1 {
		t.Fatal(outcome, extra, err, stateMoves(p))
	}
}

// The project label read takes the one GraphQL shape and refuses an
// answer it can't verify.
func TestGraphQLProjectKindLabels(t *testing.T) {
	read := GraphQLProjectKindLabels(func(_ context.Context, q string, vars map[string]any, out any) error {
		if !strings.Contains(q, "project(id:$id){ labels { nodes { id parent { id } } } }") || vars["id"] != projectA {
			t.Fatal(q, vars)
		}
		raw := `{"project":{"labels":{"nodes":[{"id":"` + strings.ToUpper(acEngineering) + `","parent":{"id":"` + acGroup + `"}},{"id":"` + labelA + `","parent":null}]}}}`
		return jsonInto(raw, out)
	})
	refs, err := read(context.Background(), projectA)
	if err != nil || len(refs) != 2 || refs[0] != (ProjectLabelRef{ID: acEngineering, ParentID: acGroup}) || refs[1] != (ProjectLabelRef{ID: labelA}) {
		t.Fatal(refs, err)
	}
	bad := GraphQLProjectKindLabels(func(_ context.Context, _ string, _ map[string]any, out any) error {
		return jsonInto(`{"project":{"labels":{"nodes":[{"id":"not-a-uuid"}]}}}`, out)
	})
	if _, err = bad(context.Background(), projectA); err == nil {
		t.Fatal("an unverified label accepted")
	}
	none := GraphQLProjectKindLabels(func(_ context.Context, _ string, _ map[string]any, out any) error {
		return jsonInto(`{"project":null}`, out)
	})
	if _, err = none(context.Background(), projectA); err == nil {
		t.Fatal("a missing project accepted")
	}
}

func jsonInto(raw string, out any) error { return json.Unmarshal([]byte(raw), out) }

// Through a hosted server with the app, a cancel or duplicate goes to the
// server as linear_write with its own arguments, which the server's
// ExecuteV2 reads back; nothing is sent with the member's profile.
func TestHostedCancelGoesToTheServer(t *testing.T) {
	receipt := map[string]any{"kind": "cancel", "outcome": "accepted", "state_landed": cgCanceled}
	server := &appServer{status: appOn, write: func(bus.Request) (bus.Result, error) {
		return bus.Result{Source: "server", Data: map[string]any{"receipt": receipt}}, nil
	}}
	s, p, scope := routedV2(t, server)
	got, err := s.ExecuteV2(context.Background(), scope, "duplicate", map[string]any{"id": issueID, "duplicate_of": acOriginal, "reason": "same", "operation_id": "op-ac-hosted"})
	if err != nil || !reflect.DeepEqual(got, receipt) || p.writes != 0 || len(server.writes) != 1 {
		t.Fatal(got, err, p.writes, server.writes)
	}
	args := server.writes[0].Args
	if args["operation"] != "duplicate" || !reflect.DeepEqual(args["args"], map[string]any{"id": issueID, "duplicate_of": acOriginal, "reason": "same", "operation_id": "op-ac-hosted"}) {
		t.Fatal(args)
	}
	if _, err = s.ExecuteV2(context.Background(), scope, "cancel", map[string]any{"id": issueID, "reason": "r", "operation_id": "op-ac-x", "state_id": cgCanceled}); err == nil {
		t.Fatal("a cancel took a state")
	}
}

// A duplicate retried after it finished sends nothing again and still says
// ClawBridge set the state.
func TestDuplicateRetryIsIdempotent(t *testing.T) {
	s, _, p, scope := duplicateTest(t)
	in := CancelInputV2{Operation: "duplicate", OperationID: "op-ac-dup-retry", IssueID: issueID, DuplicateOf: acOriginal, Reason: "same"}
	if _, err := s.CancelV2(context.Background(), scope, in); err != nil {
		t.Fatal(err)
	}
	out, err := s.CancelV2(context.Background(), scope, in)
	if err != nil || len(p.edges) != 1 || len(p.bodies) != 1 || len(stateMoves(p)) != 1 || out.(CancelResultV2).StateSetBy != "clawbridge" {
		t.Fatal(err, p.edges, p.bodies, stateMoves(p), out)
	}
}

// A duplicate edge's receipt, unverified or not, replays from the ledger;
// only an added one, and only a duplicate's names the relation unverified.
func TestDuplicateEdgeReceiptsReplay(t *testing.T) {
	edge := func(r ReceiptV2) []byte { raw, _ := json.Marshal(r); return raw }
	ok := ReceiptV2{Kind: "edge", Outcome: "accepted", Action: "add", Type: "duplicate", FromID: issueID, ToID: acOriginal}
	if !validReceiptV2(edge(ok)) {
		t.Fatal("a duplicate edge")
	}
	ok.Unverified = []string{"relation"}
	if !validReceiptV2(edge(ok)) {
		t.Fatal("an unverified duplicate edge")
	}
	for name, r := range map[string]ReceiptV2{
		"removed":           {Kind: "edge", Outcome: "accepted", Action: "remove", Type: "duplicate", FromID: issueID, ToID: acOriginal},
		"blocks unverified": {Kind: "edge", Outcome: "accepted", Action: "add", Type: "blocks", FromID: issueID, ToID: acOriginal, Unverified: []string{"relation"}},
		"other field":       {Kind: "edge", Outcome: "accepted", Action: "add", Type: "duplicate", FromID: issueID, ToID: acOriginal, Unverified: []string{"labels"}},
	} {
		if validReceiptV2(edge(r)) {
			t.Fatal(name, "accepted")
		}
	}
}

// An edge Linear's answer doesn't name (save_issue's duplicateOf) is
// answered unverified: no read shows a duplicate edge. A retry replays it.
func TestUnconfirmedDuplicateEdgeIsUnverified(t *testing.T) {
	s, _, p, scope := duplicateTest(t)
	p.unconfirmedEdges = true
	in := CancelInputV2{Operation: "duplicate", OperationID: "op-ac-dup-unconfirmed", IssueID: issueID, DuplicateOf: acOriginal}
	for range 2 {
		out, err := s.CancelV2(context.Background(), scope, in)
		if err != nil || out.(CancelResultV2).Relation != "unverified" || len(p.edges) != 1 || len(stateMoves(p)) != 1 {
			t.Fatal(err, out, p.edges, stateMoves(p))
		}
	}
}

// On a hosted server an agent's writes reuse their team proof for a minute
// (write_proof.go). A cancel runs held, so it verifies the whole binding
// itself: a kept team proof from an earlier write never leaves it without
// the issue's project pair.
func TestAgentCancelWithAKeptTeamProof(t *testing.T) {
	s, _, p, scope, _ := cancelTest(t, sfProgress)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s.cfg.Now = func() time.Time { return now }
	s.cfg.WriteIdentity = func() string { return "app/org/token-1" }
	s.writeProofs = NewWriteTeamProofs()
	if _, err := s.WriteV2(context.Background(), scope, titleUpdateV2()); err != nil {
		t.Fatal("the write that keeps the team proof:", err)
	}
	if _, err := acCancel(s, scope, "no longer needed"); err != nil {
		t.Fatal("a cancel with the kept team proof:", err)
	}
	if len(p.updated) == 0 {
		t.Fatal("the cancel set no state")
	}
}
