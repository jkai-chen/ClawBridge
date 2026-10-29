package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// otherState is sfStates' ID in team B (otherTeamID).
func otherState(id string) string { return strings.Replace(id, "c1", "c2", 1) }

// keepTest is a board on teams A and B, both with the sfStates workflow,
// and a current status policy for team A only: claim, release_done and
// done, with both bound projects closed by agents, project flow on.
func keepTest(t *testing.T) (*Service, *authorityV2, *providerV2, bus.Request, bus.StatusPolicy) {
	t.Helper()
	s, a, p, scope := testV2(t)
	p.statuses = append([]linearprovider.Status{}, sfStates...)
	for _, st := range sfStates {
		st.TeamID, st.ID = otherTeamID, otherState(st.ID)
		p.statuses = append(p.statuses, st)
	}
	cur := sfPolicy()
	team := cur.Teams[teamID]
	team.Create = nil
	team.Done = &bus.StatusState{ID: sfDone, Name: "Done", Type: "completed"}
	cur.Teams[teamID] = team
	cur.Closes = map[string]string{projectA: "agent", projectB: "agent"}
	cur.ProjectFlow = true
	cur.ProjectTargets = &bus.ProjectTargets{Started: projectA, Completed: projectB}
	setCurrent(t, a, cur)
	return s, a, p, scope, cur
}

func setCurrent(t *testing.T, a *authorityV2, cur bus.StatusPolicy) {
	t.Helper()
	raw, _ := json.Marshal(cur)
	a.status = map[string]any{}
	if e := json.Unmarshal(raw, &a.status); e != nil {
		t.Fatal(e)
	}
	a.status["revision"] = 2
}

func setFile(s *Service, a *authorityV2, scope bus.Request, file string) (map[string]any, bus.StatusPolicy, error) {
	a.statusSet = nil
	out, e := s.SetStatusPolicyV2(context.Background(), scope, []byte(file), 2, func(map[string]string) error { return nil })
	if e != nil {
		return nil, bus.StatusPolicy{}, e
	}
	got, _ := out.(map[string]any)
	policy, e := bus.ParseStatusPolicy(a.statusSet["status_policy"])
	return got, policy, e
}

// The production case: a file from before closes and done existed, set
// again to take in a newly bound team, keeps both.
func TestSetStatusPolicyKeepsWhatTheFileLeavesOut(t *testing.T) {
	s, a, _, scope, _ := keepTest(t)
	out, policy, e := setFile(s, a, scope, `{"enabled": true, "claim": "In Progress", "release_done": "In Review"}`)
	if e != nil {
		t.Fatal(e)
	}
	if len(policy.Closes) != 2 || policy.Closes[projectA] != "agent" || policy.Closes[projectB] != "agent" {
		t.Fatal("closes lost:", policy.Closes)
	}
	if d := policy.Teams[teamID].Done; d == nil || d.ID != sfDone {
		t.Fatal("team A lost its done:", policy.Teams[teamID])
	}
	b := policy.Teams[otherTeamID]
	if b.Done == nil || b.Done.ID != otherState(sfDone) || b.Done.Name != "Done" || b.Claim == nil || b.Claim.ID != otherState(sfProgress) || b.ReleaseDone == nil || b.ReleaseDone.ID != otherState(sfReview) || b.Create != nil {
		t.Fatal("team B:", b)
	}
	if !policy.ProjectFlow || policy.ProjectTargets == nil || policy.ProjectTargets.Started != projectA {
		t.Fatal("project flow or targets lost:", policy)
	}
	if !reflect.DeepEqual(out["kept"], []string{"closes", "done", "project_flow", "project_targets"}) {
		t.Fatalf("kept = %v", out["kept"])
	}
}

// A key the file includes replaces the current value: closes {} clears
// every rule, done "" clears every done state, project_flow false turns
// it off, project_targets null clears them.
func TestSetStatusPolicyReplacesWhatTheFileSays(t *testing.T) {
	s, a, _, scope, _ := keepTest(t)
	out, policy, e := setFile(s, a, scope, `{"enabled": true, "claim": "In Progress", "release_done": "In Review", "done": "", "closes": {}, "project_flow": false, "project_targets": null}`)
	if e != nil {
		t.Fatal(e)
	}
	if len(policy.Closes) != 0 || policy.ProjectFlow || policy.ProjectTargets != nil {
		t.Fatal(policy)
	}
	for team, st := range policy.Teams {
		if st.Done != nil {
			t.Fatal("done kept in", team)
		}
	}
	if out != nil && out["kept"] != nil {
		t.Fatal("kept named for a file that said everything:", out)
	}
	// Absent project_flow keeps true; present false turns it off.
	if _, policy, e = setFile(s, a, scope, `{"enabled": true, "claim": "In Progress", "project_flow": false}`); e != nil || policy.ProjectFlow {
		t.Fatal(policy, e)
	}
	if _, policy, e = setFile(s, a, scope, `{"enabled": true, "claim": "In Progress"}`); e != nil || !policy.ProjectFlow {
		t.Fatal(policy, e)
	}
	// claim_from_triage is kept too, unless the file says.
	cur := sfPolicy()
	cur.ClaimFromTriage = true
	setCurrent(t, a, cur)
	if _, policy, e = setFile(s, a, scope, `{"enabled": true, "claim": "In Progress"}`); e != nil || !policy.ClaimFromTriage {
		t.Fatal(policy, e)
	}
	if _, policy, e = setFile(s, a, scope, `{"enabled": true, "claim": "In Progress", "claim_from_triage": false}`); e != nil || policy.ClaimFromTriage {
		t.Fatal(policy, e)
	}
}

// A kept state whose ID left the workflow is found again by its stored name
// once, and only if that is unique; otherwise the set is refused and
// nothing is sent (a kept done instead stops the team at release_done).
func TestSetStatusPolicyKeptStateThatVanished(t *testing.T) {
	s, a, p, scope, _ := keepTest(t)
	// Team A's Done is re-created with a new ID: found by its name.
	const renewed = "c1000000-0000-4000-8000-0000000000e6"
	for i, st := range p.statuses {
		if st.ID == sfDone {
			p.statuses[i].ID = renewed
		}
	}
	_, policy, e := setFile(s, a, scope, `{"enabled": true, "claim": "In Progress", "release_done": "In Review"}`)
	if e != nil || policy.Teams[teamID].Done == nil || policy.Teams[teamID].Done.ID != renewed {
		t.Fatal(policy.Teams[teamID], e)
	}
	// A kept claim that is gone by ID and name: refused, nothing sent.
	s, a, p, scope, _ = keepTest(t)
	for i, st := range p.statuses {
		if st.ID == sfProgress {
			p.statuses[i].ID, p.statuses[i].Name = "c1000000-0000-4000-8000-0000000000e3", "Doing"
		}
	}
	_, _, e = setFile(s, a, scope, `{"enabled": true, "release_done": "In Review"}`)
	if e == nil || !strings.Contains(e.Error(), "claim state In Progress kept from the current policy is no longer in team "+teamID+"; nothing was set") || a.statusSet != nil {
		t.Fatal(e, a.statusSet)
	}
	// A kept done that is gone by ID and name: the team stops, named.
	s, a, p, scope, _ = keepTest(t)
	for i, st := range p.statuses {
		if st.ID == sfDone {
			p.statuses[i].ID, p.statuses[i].Name = renewed, "Finished"
		}
	}
	out, policy, e := setFile(s, a, scope, `{"enabled": true, "claim": "In Progress", "release_done": "In Review"}`)
	notes, _ := out["notes"].([]string)
	if e != nil || policy.Teams[teamID].Done != nil || !strings.Contains(strings.Join(notes, "|"), teamID) {
		t.Fatal(policy.Teams[teamID], notes, e)
	}
}

// Current teams that disagree on the done name leave a newly bound team
// without one, named in the notes.
func TestSetStatusPolicyDisagreeingDoneLeavesNewTeamUnset(t *testing.T) {
	s, a, _, scope, cur := keepTest(t)
	extra := "c3000000-0000-4000-8000-000000000001"
	cur.Teams[extra] = bus.StatusTeam{Done: &bus.StatusState{ID: "c3000000-0000-4000-8000-0000000000a6", Name: "Shipped", Type: "completed"}, Workflow: map[string]string{"c3000000-0000-4000-8000-0000000000a6": "completed"}}
	setCurrent(t, a, cur)
	out, policy, e := setFile(s, a, scope, `{"enabled": true, "claim": "In Progress", "release_done": "In Review"}`)
	notes, _ := out["notes"].([]string)
	if e != nil || policy.Teams[otherTeamID].Done != nil || policy.Teams[teamID].Done == nil {
		t.Fatal(policy.Teams, e)
	}
	if joined := strings.Join(notes, "|"); !strings.Contains(joined, otherTeamID) || strings.Contains(joined, "team(s) "+teamID) {
		t.Fatal(notes)
	}
}

// A current policy that can't be read is never replaced blindly.
func TestSetStatusPolicyRefusesWhenTheCurrentPolicyIsUnreadable(t *testing.T) {
	s, a, _, scope, _ := keepTest(t)
	a.statusErr = errors.New("down")
	_, _, e := setFile(s, a, scope, `{"enabled": true, "claim": "In Progress"}`)
	if e == nil || e.Error() != "the current status policy could not be read; nothing was set" || a.statusSet != nil {
		t.Fatal(e, a.statusSet)
	}
}

// A kept close for a project no longer bound is dropped and named.
func TestSetStatusPolicyDropsAKeptCloseOfAnUnboundProject(t *testing.T) {
	s, a, _, scope, cur := keepTest(t)
	cur.Closes[parentA] = "agent"
	setCurrent(t, a, cur)
	out, policy, e := setFile(s, a, scope, `{"enabled": true, "claim": "In Progress", "release_done": "In Review"}`)
	notes, _ := out["notes"].([]string)
	if e != nil || len(policy.Closes) != 2 || policy.Closes[parentA] != "" || !strings.Contains(strings.Join(notes, "|"), parentA) {
		t.Fatal(policy.Closes, notes, e)
	}
}

// The documented fix for a newly bound team, the file {"enabled": true},
// keeps every key and gives the new team the states the others agree on.
func TestSetStatusPolicyEnabledOnlyTakesInANewTeam(t *testing.T) {
	s, a, _, scope, _ := keepTest(t)
	out, policy, e := setFile(s, a, scope, `{"enabled": true}`)
	if e != nil {
		t.Fatal(e)
	}
	if len(policy.Closes) != 2 || !policy.ProjectFlow || policy.ProjectTargets == nil {
		t.Fatal("a key was reset:", policy)
	}
	a2, b := policy.Teams[teamID], policy.Teams[otherTeamID]
	if a2.Claim == nil || a2.Claim.ID != sfProgress || a2.Done == nil || a2.Done.ID != sfDone {
		t.Fatal("team A:", a2)
	}
	if b.Claim == nil || b.Claim.ID != otherState(sfProgress) || b.ReleaseDone == nil || b.ReleaseDone.ID != otherState(sfReview) || b.Done == nil || b.Done.ID != otherState(sfDone) {
		t.Fatal("team B:", b)
	}
	if !reflect.DeepEqual(out["kept"], []string{"claim", "closes", "done", "project_flow", "project_targets", "release_done"}) {
		t.Fatalf("kept = %v", out["kept"])
	}
}

// A key counts as present however it is cased, as the decoder reads it:
// "Closes": {} clears every rule rather than keeping them.
func TestSetStatusPolicyKeyPresenceIgnoresCase(t *testing.T) {
	s, a, _, scope, _ := keepTest(t)
	_, policy, e := setFile(s, a, scope, `{"enabled": true, "Closes": {}}`)
	if e != nil {
		t.Fatal(e)
	}
	if len(policy.Closes) != 0 {
		t.Fatal("closes kept for a file that cleared them:", policy.Closes)
	}
}
