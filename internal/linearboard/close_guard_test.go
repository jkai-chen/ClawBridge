package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	cgCanceled  = "c1000000-0000-4000-8000-0000000000a7"
	cgDuplicate = "c1000000-0000-4000-8000-0000000000a8"
	cgUntyped   = "c1000000-0000-4000-8000-0000000000a9"
)

// cgStates are sfStates plus a canceled state, a state of Linear's
// duplicate type (known to ClawBridge only by its raw type) and one whose
// type nothing reports.
func cgStates() []linearprovider.Status {
	out := append([]linearprovider.Status{}, sfStates...)
	return append(out,
		linearprovider.Status{ID: cgCanceled, Name: "Canceled", TeamID: teamID, Type: "canceled", Raw: "canceled"},
		linearprovider.Status{ID: cgDuplicate, Name: "Duplicate", TeamID: teamID, Raw: "duplicate"},
		linearprovider.Status{ID: cgUntyped, Name: "Mystery", TeamID: teamID})
}

// cgPolicy is sfPolicy with a done state, where agents close projectA;
// projectB, unlisted, is people's by default.
func cgPolicy() map[string]any {
	policy := sfPolicy()
	team := policy.Teams[teamID]
	team.Done = &bus.StatusState{ID: sfDone, Name: "Done", Type: "completed"}
	policy.Teams[teamID] = team
	return map[string]any{"revision": 1, "enabled": true, "teams": policy.Teams, "closes": map[string]any{projectA: "agent"}}
}

// closeTest is a board whose issue sits in project at state, with both
// projects in the issue's team.
func closeTest(t *testing.T, project, state string) (*Service, *authorityV2, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	p.statuses = cgStates()
	p.projects[projectB] = []string{teamID, otherTeamID}
	issue := p.issues[issueID]
	issue.ProjectID, issue.StateID = project, state
	p.issues[issueID] = issue
	a.status = cgPolicy()
	return s, a, p, scope
}

var cgOp = 0

func cgUpdate(s *Service, scope bus.Request, state, project *string) error {
	cgOp++
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "op-cg-" + strings.Repeat("u", cgOp), IssueID: issueID, StateID: state}}
	if project != nil {
		in.Project = linearprovider.Field[*string]{Present: true, Value: project}
	}
	_, err := s.WriteV2(context.Background(), scope, in)
	return err
}

func refusedNaming(t *testing.T, name string, err error, p *providerV2, words ...string) {
	t.Helper()
	if err == nil || len(p.updated)+len(p.created) != 0 {
		t.Fatal(name, "not refused before Linear:", err, p.updated, p.created)
	}
	for _, w := range words {
		if !strings.Contains(err.Error(), w) {
			t.Fatal(name, "refusal does not say", w, ":", err)
		}
	}
}

// Under an enabled policy an agent completes an issue only in a project
// agents close, and never cancels one, refused before any Linear write and
// naming the project; a person's write is never refused.
func TestAgentClosesOnlyWhereThePolicySaysAgent(t *testing.T) {
	s, _, p, scope := closeTest(t, projectA, sfProgress)
	if err := cgUpdate(s, scope, ptrV2(sfDone), nil); err != nil || len(stateMoves(p)) != 1 {
		t.Fatal("agent project", err, stateMoves(p))
	}
	s, _, p, scope = closeTest(t, projectB, sfProgress)
	refusedNaming(t, "human project", cgUpdate(s, scope, ptrV2(sfDone), nil), p, projectB, "people close")
	for name, state := range map[string]string{"canceled": cgCanceled, "duplicate": cgDuplicate} {
		s, _, p, scope = closeTest(t, projectA, sfProgress)
		refusedNaming(t, name, cgUpdate(s, scope, ptrV2(state), nil), p, "never cancel")
	}
	// An issue in no project is a person's to close.
	s, _, p, scope = closeTest(t, "", sfProgress)
	none := linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueID, TeamID: teamID}, ProjectKnown: true}
	err := s.closeGuard(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", StateID: ptrV2(sfDone)}}, cgStates(), teamID, none, "")
	if err == nil || !strings.Contains(err.Error(), "no project") {
		t.Fatal("no project", err)
	}
	// A person (no session) is unaffected.
	s, a, p, scope := closeTest(t, projectB, sfProgress)
	a.actor, a.name = "", ""
	if err := cgUpdate(s, scope, ptrV2(sfDone), nil); err != nil {
		t.Fatal("a person's completion", err)
	}
	if err := cgUpdate(s, scope, ptrV2(cgCanceled), nil); err != nil || len(stateMoves(p)) != 2 {
		t.Fatal("a person's cancel", err, stateMoves(p))
	}
	// A create is judged by its project.
	s, _, p, scope = closeTest(t, projectA, sfProgress)
	create := func(project string) error {
		cgOp++
		_, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: "op-cg-" + strings.Repeat("c", cgOp), Title: ptrV2("T"), StateID: ptrV2(sfDone)}, Project: linearprovider.Field[*string]{Present: true, Value: ptrV2(project)}})
		return err
	}
	if err := create(projectA); err != nil || len(p.created) != 1 {
		t.Fatal("create in an agent project", err)
	}
	if err := create(projectB); err == nil || len(p.created) != 1 || !strings.Contains(err.Error(), projectB) {
		t.Fatal("create in a human project", err, p.created)
	}
}

// A2-M1: an update that completes an issue is judged by both the project
// it is in and the one it moves to.
func TestCompletingAnUpdateNeedsBothProjectsAgent(t *testing.T) {
	s, _, p, scope := closeTest(t, projectB, sfProgress)
	refusedNaming(t, "human → agent and done", cgUpdate(s, scope, ptrV2(sfDone), ptrV2(projectA)), p, projectB)
	s, _, p, scope = closeTest(t, projectA, sfProgress)
	refusedNaming(t, "agent → human and done", cgUpdate(s, scope, ptrV2(sfDone), ptrV2(projectB)), p, projectB)
	// An issue not yet started may move, so only the source project
	// refuses this one.
	s, _, p, scope = closeTest(t, projectB, sfBacklog)
	refusedNaming(t, "unstarted human → agent and done", cgUpdate(s, scope, ptrV2(sfDone), ptrV2(projectA)), p, projectB, "may not complete")
}

// The hop rule: an agent may not move an issue under way out of a project
// people close into one agents close; an unstarted issue, a move the other
// way, or a person's move are unaffected.
func TestAgentMayNotHopAStartedIssueIntoAnAgentProject(t *testing.T) {
	for _, state := range []string{sfProgress, sfDone} {
		s, _, p, scope := closeTest(t, projectB, state)
		refusedNaming(t, "hop", cgUpdate(s, scope, nil, ptrV2(projectA)), p, projectB, "under way")
	}
	s, _, p, scope := closeTest(t, projectB, sfBacklog)
	if err := cgUpdate(s, scope, nil, ptrV2(projectA)); err != nil || len(p.updated) != 1 {
		t.Fatal("an unstarted issue's move", err)
	}
	s, _, p, scope = closeTest(t, projectA, sfProgress)
	if err := cgUpdate(s, scope, nil, ptrV2(projectB)); err != nil || len(p.updated) != 1 {
		t.Fatal("agent → human", err)
	}
	s, a, p, scope := closeTest(t, projectB, sfProgress)
	a.actor, a.name = "", ""
	if err := cgUpdate(s, scope, nil, ptrV2(projectA)); err != nil || len(p.updated) != 1 {
		t.Fatal("a person's move", err)
	}
	// Under the policy, a current state of unknown type refuses.
	s, _, p, scope = closeTest(t, projectB, cgUntyped)
	refusedNaming(t, "unknown current type", cgUpdate(s, scope, nil, ptrV2(projectA)), p, "cannot verify")
}

// With no status policy, or a disabled one, an agent completes and moves
// issues as before; canceled is still refused wherever its type is known,
// and passes only where nothing knows it (the documented gap).
func TestWithoutAPolicyOnlyCanceledIsRefused(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		fresh := func(project, state string) (*Service, *providerV2, bus.Request) {
			s, a, p, scope := closeTest(t, project, state)
			a.status = nil
			if disabled {
				a.status = cgPolicy()
				a.status["enabled"] = false
			}
			return s, p, scope
		}
		s, p, scope := fresh(projectB, sfProgress)
		if err := cgUpdate(s, scope, ptrV2(sfDone), nil); err != nil {
			t.Fatal(disabled, "completion", err)
		}
		if err := cgUpdate(s, scope, nil, ptrV2(projectA)); err != nil || len(p.updated) != 2 {
			t.Fatal(disabled, "hop", err)
		}
		for name, state := range map[string]string{"canceled": cgCanceled, "duplicate": cgDuplicate} {
			s, p, scope = fresh(projectA, sfProgress)
			refusedNaming(t, name, cgUpdate(s, scope, ptrV2(state), nil), p, "never cancel")
		}
		s, p, scope = fresh(projectA, sfProgress)
		if err := cgUpdate(s, scope, ptrV2(cgUntyped), nil); err != nil || len(p.updated) != 1 {
			t.Fatal(disabled, "an unknown type without a policy", err)
		}
	}
}

// Under an enabled policy an unknown type refuses; a hosted server's
// StateType decides it, by Linear's raw type.
func TestStateTypeDecidesAStateNothingElseKnows(t *testing.T) {
	s, _, p, scope := closeTest(t, projectA, sfProgress)
	refusedNaming(t, "unknown", cgUpdate(s, scope, ptrV2(cgUntyped), nil), p, "cannot verify")
	reads := 0
	for raw, sent := range map[string]bool{"completed": true, "Duplicate": false, "canceled": false} {
		s, _, p, scope = closeTest(t, projectA, sfProgress)
		s.cfg.StateType = func(_ context.Context, team, state string) (string, error) {
			reads++
			if team != teamID || state != cgUntyped {
				t.Fatal("read", team, state)
			}
			return raw, nil
		}
		if err := cgUpdate(s, scope, ptrV2(cgUntyped), nil); (err == nil) != sent || len(p.updated) == 0 == sent {
			t.Fatal(raw, err, p.updated)
		}
	}
	s, _, p, scope = closeTest(t, projectA, sfProgress)
	s.cfg.StateType = func(context.Context, string, string) (string, error) { return "", errors.New("raw GraphQL detail") }
	err := cgUpdate(s, scope, ptrV2(cgUntyped), nil)
	refusedNaming(t, "a failed read", err, p, "cannot read")
	if strings.Contains(err.Error(), "GraphQL") || reads != 3 {
		t.Fatal(err, reads)
	}
}

// The older V1 write route holds the same rule.
func TestV1WriteRefusesAnAgentsCancel(t *testing.T) {
	s, a, _, scope := testService(t)
	a.statusPolicy = map[string]any{"revision": 0, "enabled": false, "teams": map[string]any{}}
	s.cfg.StateType = func(context.Context, string, string) (string, error) { return "canceled", nil }
	_, err := s.Write(context.Background(), scope, WriteInput{Operation: "update", OperationID: "op-v1-cancel", IssueID: issueID, StateID: ptrV2(issueID)})
	if err == nil || !strings.Contains(err.Error(), "never cancel") {
		t.Fatal(err)
	}
}

// The state type cache reads a team once per TTL, reads again for a state
// it has not seen, and forgets everything on Drop.
func TestStateTypeCacheKeepsATeamForItsTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	reads := 0
	c := NewStateTypeCache(func(_ context.Context, team string) (map[string]string, error) {
		reads++
		return map[string]string{sfDone: "completed"}, nil
	}, time.Minute, func() time.Time { return now })
	get := func(state string) string {
		kind, err := c.Type(context.Background(), strings.ToUpper(teamID), state)
		if err != nil {
			t.Fatal(err)
		}
		return kind
	}
	if get(sfDone) != "completed" || get(strings.ToUpper(sfDone)) != "completed" || reads != 1 {
		t.Fatal("not kept", reads)
	}
	if get(cgUntyped) != "" || reads != 2 {
		t.Fatal("an unseen state was not read again", reads)
	}
	now = now.Add(time.Minute)
	if get(sfDone); reads != 3 {
		t.Fatal("kept past its TTL", reads)
	}
	c.Drop()
	if get(sfDone); reads != 4 {
		t.Fatal("kept after Drop", reads)
	}
}

func TestGraphQLStateTypesChecksItsAnswer(t *testing.T) {
	answer := func(nodes []map[string]any, next bool) func(context.Context, string, map[string]any, any) error {
		return func(_ context.Context, q string, vars map[string]any, out any) error {
			if !strings.Contains(q, "workflowStates") || vars["team"] != teamID {
				t.Fatal(q, vars)
			}
			raw := map[string]any{"workflowStates": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next}}}
			return remarshal(raw, out)
		}
	}
	ok := []map[string]any{{"id": strings.ToUpper(cgDuplicate), "type": "Duplicate", "team": map[string]any{"id": teamID}}}
	types, err := GraphQLStateTypes(answer(ok, false))(context.Background(), teamID)
	if err != nil || types[cgDuplicate] != "duplicate" {
		t.Fatal(types, err)
	}
	if _, err = GraphQLStateTypes(answer(ok, true))(context.Background(), teamID); err == nil {
		t.Fatal("an incomplete list was kept")
	}
	foreign := []map[string]any{{"id": cgDuplicate, "type": "canceled", "team": map[string]any{"id": otherTeamID}}}
	if _, err = GraphQLStateTypes(answer(foreign, false))(context.Background(), teamID); err == nil {
		t.Fatal("another team's state was kept")
	}
}

func remarshal(in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// A done-release goes on to the team's done state in a project agents
// close, from the claim state or, when its claim was overtaken under the
// same policy, from the claim's from-set; elsewhere it stops at
// release_done, and it never moves from release_done.
func TestDoneReleaseReachesDoneOnlyInAnAgentProject(t *testing.T) {
	policy := func(done bool) bus.StatusPolicy {
		p, _ := decodeStatusPolicy(cgPolicy())
		if !done {
			team := p.Teams[teamID]
			team.Done = nil
			p.Teams[teamID] = team
		}
		return p
	}
	for _, c := range []struct {
		name, project, current string
		done                   bool
		claimRevision          any
		moved                  string
	}{
		{"agent project", projectA, sfProgress, true, nil, sfDone},
		{"human project", projectB, sfProgress, true, nil, sfReview},
		{"agent project without done", projectA, sfProgress, false, nil, sfReview},
		{"overtaken claim, agent project", projectA, sfBacklog, true, 1, sfDone},
		{"overtaken claim, policy changed", projectA, sfBacklog, true, 2, ""},
		{"from release_done", projectA, sfReview, true, nil, ""},
	} {
		s, a, p, scope := stateTest(t, "release_done", c.current, policy(c.done), false)
		p.projects[projectB] = []string{teamID, otherTeamID}
		issue := p.issues[issueID]
		issue.ProjectID = c.project
		p.issues[issueID] = issue
		if c.claimRevision != nil {
			a.comments.gos["state"]["claim_state"] = "superseded"
			a.comments.gos["state"]["claim_policy_revision"] = c.claimRevision
		}
		s.ApplyMarker(context.Background(), scope, "asg_1", "close", nil)
		m := stateMoves(p)
		if c.moved == "" {
			if len(m) != 0 || settled(a, "state")["skip_reason"] != "from_state" {
				t.Fatal(c.name, m, settled(a, "state"))
			}
			continue
		}
		if len(m) != 1 || m[0] != c.moved || settled(a, "state")["state_landed"] != c.moved {
			t.Fatal(c.name, m, settled(a, "state"))
		}
	}
}

// The owner's file: done resolves to a completed state per team, closes
// names bound projects only, and the answer names each team whose
// done-releases will stop at release_done.
func TestSetStatusPolicyResolvesDoneAndCloses(t *testing.T) {
	s, a, p, scope := testV2(t)
	// The other team has the same workflow but no Done.
	p.statuses = append([]linearprovider.Status{}, sfStates...)
	for _, st := range sfStates {
		if st.Type != "completed" {
			st.TeamID, st.ID = otherTeamID, strings.Replace(st.ID, "c1", "c2", 1)
			p.statuses = append(p.statuses, st)
		}
	}
	set := func(file string) (any, error) {
		a.statusSet = nil
		return s.SetStatusPolicyV2(context.Background(), scope, []byte(file), 0, func(map[string]string) error { return nil })
	}
	out, err := set(`{"enabled": true, "claim": "In Progress", "release_done": "In Review", "done": "Done", "closes": {"` + strings.ToUpper(projectA) + `": "agent"}}`)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := bus.ParseStatusPolicy(a.statusSet["status_policy"])
	if err != nil || policy.Teams[teamID].Done == nil || policy.Teams[teamID].Done.ID != sfDone || policy.Closes[projectA] != "agent" {
		t.Fatal(err, a.statusSet)
	}
	// otherTeamID has no done: named.
	notes, _ := out.(map[string]any)["notes"].([]string)
	if len(notes) != 1 || !strings.Contains(notes[0], otherTeamID) || strings.Contains(notes[0], teamID) {
		t.Fatal(out)
	}
	for name, file := range map[string]string{
		"unbound project": `{"enabled": true, "claim": "In Progress", "closes": {"` + parentA + `": "agent"}}`,
		"bad value":       `{"enabled": true, "claim": "In Progress", "closes": {"` + projectA + `": "robot"}}`,
		"started done":    `{"enabled": true, "claim": "In Progress", "done": "In Review"}`,
	} {
		if _, err := set(file); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	if out, err = set(`{"enabled": true, "claim": "In Progress", "closes": {"` + projectB + `": "human"}}`); err != nil || out.(map[string]any)["notes"] != nil {
		t.Fatal("notes without an agent project", out, err)
	}
}
