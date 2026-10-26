package linearboard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	sfTriage   = "c1000000-0000-4000-8000-0000000000a0"
	sfBacklog  = "c1000000-0000-4000-8000-0000000000a1"
	sfTodo     = "c1000000-0000-4000-8000-0000000000a2"
	sfProgress = "c1000000-0000-4000-8000-0000000000a3"
	sfReview   = "c1000000-0000-4000-8000-0000000000a4"
	sfQA       = "c1000000-0000-4000-8000-0000000000a5"
	sfDone     = "c1000000-0000-4000-8000-0000000000a6"
)

var sfStates = []linearprovider.Status{
	{ID: sfTriage, Name: "Triage", TeamID: teamID, Type: "triage"},
	{ID: sfBacklog, Name: "Backlog", TeamID: teamID, Type: "backlog"},
	{ID: sfTodo, Name: "Todo", TeamID: teamID, Type: "unstarted"},
	{ID: sfProgress, Name: "In Progress", TeamID: teamID, Type: "started"},
	{ID: sfReview, Name: "In Review", TeamID: teamID, Type: "started"},
	{ID: sfQA, Name: "QA", TeamID: teamID, Type: "started"},
	{ID: sfDone, Name: "Done", TeamID: teamID, Type: "completed"},
}

func sfPolicy() bus.StatusPolicy {
	workflow := map[string]string{}
	for _, s := range sfStates {
		workflow[s.ID] = s.Type
	}
	return bus.StatusPolicy{Enabled: true, Teams: map[string]bus.StatusTeam{teamID: {
		Create:      &bus.StatusState{ID: sfTodo, Name: "Todo", Type: "unstarted"},
		Claim:       &bus.StatusState{ID: sfProgress, Name: "In Progress", Type: "started"},
		ReleaseDone: &bus.StatusState{ID: sfReview, Name: "In Review", Type: "started"},
		Workflow:    workflow,
	}}}
}

// stateTest is a marker test whose state part hands out a move of kind at
// the issue's current state.
func stateTest(t *testing.T, kind, current string, policy bus.StatusPolicy, retry bool) (*Service, *authorityV2, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope := markerTest(t)
	p.statuses = sfStates
	issue := p.issues[issueID]
	issue.StateID = current
	p.issues[issueID] = issue
	state := "pending"
	if retry {
		state = "in_flight"
	}
	a.comments.gos = map[string]map[string]any{"state": {"action": "move", "issue_uuid": issueID, "kind": kind, "policy": policy, "policy_revision": 1, "state": state}}
	return s, a, p, scope
}

func stateMoves(p *providerV2) []string {
	out := []string{}
	for _, u := range p.updated {
		if u.StateID != nil {
			out = append(out, *u.StateID)
		}
	}
	return out
}

func TestClaimMovesOnlyFromBacklogOrUnstarted(t *testing.T) {
	for _, c := range []struct {
		name, current string
		triage        bool
		moved         bool
		outcome       string
	}{
		{"backlog", sfBacklog, false, true, "done"},
		{"todo", sfTodo, false, true, "done"},
		{"triage is a human gate", sfTriage, false, false, "skipped"},
		{"triage when allowed", sfTriage, true, true, "done"},
		{"already further along", sfReview, false, false, "skipped"},
		{"done", sfDone, false, false, "skipped"},
		{"an unknown state", "c1000000-0000-4000-8000-0000000000ff", false, false, "skipped"},
	} {
		policy := sfPolicy()
		policy.ClaimFromTriage = c.triage
		s, a, p, scope := stateTest(t, "claim", c.current, policy, false)
		out := s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
		got := settled(a, "state")
		if out.State != c.outcome || got["outcome"] != c.outcome || (len(stateMoves(p)) == 1) != c.moved {
			t.Fatal(c.name, out, got, stateMoves(p))
		}
		if c.moved && (stateMoves(p)[0] != sfProgress || got["state_target"] != sfProgress || got["state_landed"] != sfProgress) {
			t.Fatal(c.name, "moved wrongly", stateMoves(p), got)
		}
		if !c.moved && got["skip_reason"] != "from_state" {
			t.Fatal(c.name, got)
		}
	}
}

// SF-L1: a release moves only from exactly the claim state, never back
// from a later started state a person chose.
func TestReleaseDoneMovesOnlyFromTheClaimState(t *testing.T) {
	s, a, p, scope := stateTest(t, "release_done", sfProgress, sfPolicy(), false)
	s.ApplyMarker(context.Background(), scope, "asg_1", "close", nil)
	if m := stateMoves(p); len(m) != 1 || m[0] != sfReview || settled(a, "state")["outcome"] != "done" {
		t.Fatal(m, settled(a, "state"))
	}
	s, a, p, scope = stateTest(t, "release_done", sfQA, sfPolicy(), false)
	s.ApplyMarker(context.Background(), scope, "asg_1", "close", nil)
	if len(stateMoves(p)) != 0 || settled(a, "state")["skip_reason"] != "from_state" {
		t.Fatal("moved back from QA", stateMoves(p), settled(a, "state"))
	}
}

// SF-L5: a move already in place is done without a write.
func TestStateAlreadyInPlaceIsDone(t *testing.T) {
	s, a, p, scope := stateTest(t, "claim", sfProgress, sfPolicy(), false)
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if got := settled(a, "state"); len(stateMoves(p)) != 0 || got["outcome"] != "done" || got["state_landed"] != sfProgress {
		t.Fatal(stateMoves(p), got)
	}
}

// A retried move whose issue a person has meanwhile moved elsewhere settles
// done with what stands, and never sends again (SF-L3).
func TestRetryAfterAPersonMovedTheIssueSendsNothing(t *testing.T) {
	s, a, p, scope := stateTest(t, "claim", sfQA, sfPolicy(), true)
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if got := settled(a, "state"); len(stateMoves(p)) != 0 || got["outcome"] != "done" || got["state_landed"] != sfQA || got["state_target"] != sfProgress {
		t.Fatal(stateMoves(p), got)
	}
}

func TestStateMoveSkipsTeamsAndTargetsOutsideThePolicy(t *testing.T) {
	policy := sfPolicy()
	off := bus.StatusPolicy{Enabled: true, Teams: map[string]bus.StatusTeam{}}
	unknown := sfPolicy()
	team := unknown.Teams[teamID]
	team.Workflow = map[string]string{sfBacklog: "backlog"}
	unknown.Teams[teamID] = team
	for name, c := range map[string]struct {
		policy bus.StatusPolicy
		team   string
		reason string
	}{
		"team not in the policy": {off, teamID, "team_off"},
		"target not in workflow": {unknown, teamID, "target_unknown"},
	} {
		s, a, p, scope := stateTest(t, "claim", sfBacklog, c.policy, false)
		issue := p.issues[issueID]
		issue.TeamID = c.team
		p.issues[issueID] = issue
		s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
		if got := settled(a, "state"); len(stateMoves(p)) != 0 || got["outcome"] != "skipped" || got["skip_reason"] != c.reason {
			t.Fatal(name, stateMoves(p), got)
		}
	}
	// An issue moved out of the binding is refused by the read itself.
	s, a, p, scope := stateTest(t, "claim", sfBacklog, policy, false)
	issue := p.issues[issueID]
	issue.TeamID = "dddddddd-0000-4000-8000-000000000001"
	p.issues[issueID] = issue
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if got := settled(a, "state"); len(stateMoves(p)) != 0 || got["outcome"] != "out_of_binding" {
		t.Fatal(stateMoves(p), got)
	}
}

// After a send that may have failed, the issue is read again: the target
// is done, an unchanged state stays uncertain for a retry.
func TestUncertainMoveIsDecidedByAReadBack(t *testing.T) {
	s, a, p, scope := stateTest(t, "claim", sfBacklog, sfPolicy(), false)
	p.writeErr = errors.New("connection reset")
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if got := settled(a, "state"); got["outcome"] != "uncertain" {
		t.Fatal(got)
	}
	s, a, p, scope = stateTest(t, "claim", sfBacklog, sfPolicy(), false)
	p.writeErr = errors.New("connection reset")
	p.after = func() {
		issue := p.issues[issueID]
		issue.StateID = sfProgress
		p.issues[issueID] = issue
	}
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if got := settled(a, "state"); got["outcome"] != "done" || got["state_landed"] != sfProgress {
		t.Fatal(got)
	}
	// A person moved it during the send: what landed stands (SF-L3).
	s, a, p, scope = stateTest(t, "claim", sfBacklog, sfPolicy(), false)
	p.writeErr = errors.New("connection reset")
	p.after = func() {
		issue := p.issues[issueID]
		issue.StateID = sfQA
		p.issues[issueID] = issue
	}
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if got := settled(a, "state"); got["outcome"] != "done" || got["state_landed"] != sfQA || len(stateMoves(p)) != 1 {
		t.Fatal(got, stateMoves(p))
	}
}

// An authority older than the status policy refuses the state part; that
// is no state part, not an error.
func TestOlderAuthorityHasNoStatePart(t *testing.T) {
	s, a, _, scope := markerTest(t)
	a.comments.goErr = map[string]error{"state": errors.New(`invalid: part must be comment or label`)}
	if out := s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil); out.Error != "" || out.State != "off" {
		t.Fatal(out)
	}
}

// SF-L4: a create without a state takes the policy's create state; an
// explicit state is kept; an older authority means no policy; any other
// failure to read the policy sends nothing.
func TestCreateTakesThePolicyState(t *testing.T) {
	create := func(a *authorityV2, p *providerV2, s *Service, scope bus.Request, state *string) error {
		p.statuses = sfStates
		_, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: "op-" + strings.Repeat("c", len(p.created)+1), Title: ptrV2("T"), StateID: state}, Project: linearprovider.Field[*string]{Present: true, Value: ptrV2(projectA)}})
		return err
	}
	s, a, p, scope := testV2(t)
	a.status = map[string]any{"revision": 1, "enabled": true, "teams": sfPolicy().Teams}
	if err := create(a, p, s, scope, nil); err != nil || len(p.created) != 1 || p.created[0].StateID != sfTodo {
		t.Fatal(err, p.created)
	}
	if err := create(a, p, s, scope, ptrV2(sfBacklog)); err != nil || p.created[1].StateID != sfBacklog {
		t.Fatal("an explicit state was replaced", err, p.created)
	}
	a.status["enabled"] = false
	if err := create(a, p, s, scope, nil); err != nil || p.created[2].StateID != "" {
		t.Fatal("a disabled policy set a state", err, p.created)
	}
	a.statusErr = errors.New(`unsupported operation "status_policy_get"`)
	if err := create(a, p, s, scope, nil); err != nil || p.created[3].StateID != "" {
		t.Fatal("an older authority", err, p.created)
	}
	a.statusErr = errors.New("authority unavailable")
	if err := create(a, p, s, scope, nil); err == nil || len(p.created) != 4 {
		t.Fatal("a create was sent without the policy", err)
	}
}

func TestSetStatusPolicyResolvesNamesPerTeam(t *testing.T) {
	s, a, p, scope := testV2(t)
	other := append([]linearprovider.Status{}, sfStates...)
	for i := range other {
		other[i].TeamID, other[i].ID = otherTeamID, strings.Replace(other[i].ID, "c1", "c2", 1)
	}
	p.statuses = append(append([]linearprovider.Status{}, sfStates...), other...)
	if _, err := s.SetStatusPolicyV2(context.Background(), scope, []byte(`{"enabled": true, "create": "Todo", "claim": "In Progress", "release_done": "In Review"}`), 0); err != nil {
		t.Fatal(err)
	}
	sent := a.statusSet
	policy, err := bus.ParseStatusPolicy(sent["status_policy"])
	if err != nil || sent["expected_revision"] != int64(0) || len(policy.Teams) != 2 || policy.Teams[otherTeamID].Claim.ID != strings.Replace(sfProgress, "c1", "c2", 1) || len(policy.Teams[teamID].Workflow) != len(sfStates) {
		t.Fatal(err, sent)
	}
	for name, file := range map[string]string{
		"missing name":    `{"enabled": true, "claim": "Doing"}`,
		"no enabled":      `{"claim": "In Progress"}`,
		"unknown key":     `{"enabled": true, "claim": "In Progress", "extra": 1}`,
		"completed claim": `{"enabled": true, "claim": "Done"}`,
	} {
		if _, err := s.SetStatusPolicyV2(context.Background(), scope, []byte(file), 0); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	// Without state types nothing is set.
	p.statuses[0].Type = ""
	a.statusSet = nil
	if _, err := s.SetStatusPolicyV2(context.Background(), scope, []byte(`{"enabled": true, "claim": "In Progress"}`), 0); err == nil || !strings.Contains(err.Error(), "state types") || a.statusSet != nil {
		t.Fatal("a policy was set without state types", err)
	}
}
