package linearboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// StatusPolicyFile is what the owner writes: state names, resolved per
// bound team when the policy is set.
type StatusPolicyFile struct {
	Enabled         *bool  `json:"enabled"`
	ClaimFromTriage bool   `json:"claim_from_triage"`
	Create          string `json:"create"`
	Claim           string `json:"claim"`
	ReleaseDone     string `json:"release_done"`
}

// StatusPolicyV2 reads the project's status policy.
func (s *Service) StatusPolicyV2(ctx context.Context, scope bus.Request) (any, error) {
	scope.Operation = "status_policy_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}

// SetStatusPolicyV2 resolves the owner's state names in every bound team's
// workflow, exactly and uniquely, and sets the policy at expected, the
// revision the owner last read. Each team keeps a snapshot of its whole
// workflow (state ID → type), so a state added later moves nothing.
func (s *Service) SetStatusPolicyV2(ctx context.Context, scope bus.Request, raw []byte, expected int64) (any, error) {
	if e := uniqueKeys(raw); e != nil {
		return nil, e
	}
	var file StatusPolicyFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&file) != nil || dec.Decode(new(any)) != io.EOF || file.Enabled == nil {
		return nil, errors.New(`the status policy file must be {"enabled": true|false, "claim_from_triage", "create", "claim", "release_done"} with state names`)
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if !b.Configured || len(b.LinearTeamIDs) == 0 {
		return nil, errors.New("link the task board before setting a status policy")
	}
	teams := map[string]any{}
	for _, team := range b.LinearTeamIDs {
		states, e := s.StatesV2(ctx, scope, team)
		if e != nil {
			return nil, e
		}
		if !typed(states) {
			// Linear's MCP status list may leave the type out: a hosted
			// server's Linear app reads it.
			if states, e = s.appStates(ctx, scope, team); e != nil {
				return nil, e
			}
		}
		workflow := map[string]any{}
		byName := map[string][]linearprovider.Status{}
		for _, st := range states {
			if st.Type == "" {
				return nil, errors.New("the Linear connection did not report workflow state types; nothing was set")
			}
			workflow[strings.ToLower(st.ID)] = st.Type
			byName[st.Name] = append(byName[st.Name], st)
		}
		entry := map[string]any{"workflow": workflow}
		for field, name := range map[string]string{"create": file.Create, "claim": file.Claim, "release_done": file.ReleaseDone} {
			if name == "" {
				continue
			}
			found := byName[name]
			if len(found) != 1 {
				return nil, fmt.Errorf("%s state %q is missing or not unique in team %s; nothing was set", field, name, team)
			}
			entry[field] = map[string]any{"id": strings.ToLower(found[0].ID), "name": found[0].Name, "type": found[0].Type}
		}
		teams[strings.ToLower(team)] = entry
	}
	policy := map[string]any{"enabled": *file.Enabled, "claim_from_triage": file.ClaimFromTriage, "teams": teams}
	if _, e = bus.ParseStatusPolicy(policy); e != nil {
		return nil, e
	}
	scope.Operation = "status_policy_set"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = expected
	scope.Args["status_policy"] = policy
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}

func typed(states []linearprovider.Status) bool {
	for _, st := range states {
		if st.Type == "" {
			return false
		}
	}
	return true
}

// appStates reads a bound team's workflow states with their types through
// a hosted server's Linear app (linear_app_states).
func (s *Service) appStates(ctx context.Context, scope bus.Request, team string) ([]linearprovider.Status, error) {
	scope.Operation = "linear_app_states"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["team"] = team
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, errors.New("the Linear connection did not report workflow state types, and the server's Linear app could not read them; nothing was set")
	}
	raw, _ := json.Marshal(r.Data)
	var out struct {
		States []linearprovider.Status `json:"states"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return nil, errors.New("the server's Linear app answered no workflow states; nothing was set")
	}
	known := []linearprovider.Status{}
	for _, st := range out.States {
		if !linearprovider.ValidID(st.ID) || !strings.EqualFold(st.TeamID, team) {
			return nil, errors.New("the server's Linear app answered an unverified workflow state; nothing was set")
		}
		// A v0.3.1 server before this fix passes Linear's type through:
		// a state of a type ClawBridge does not know is left out here too.
		if st.Type = linearprovider.StatusType(st.Type); st.Type != "" {
			known = append(known, st)
		}
	}
	if len(known) == 0 {
		return nil, errors.New("the server's Linear app answered no workflow states; nothing was set")
	}
	return known, nil
}

// statusPolicy reads the policy for a create. An authority older than the
// status policy has none; any other failure refuses the write, so a create
// never lands in a state the policy would not have chosen.
func (s *Service) statusPolicy(ctx context.Context, scope bus.Request) (bus.StatusPolicy, error) {
	scope.Operation = "status_policy_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		if strings.Contains(e.Error(), `unsupported operation "status_policy_get"`) {
			return bus.StatusPolicy{}, nil
		}
		return bus.StatusPolicy{}, errors.New("cannot read the status policy; nothing was sent")
	}
	return decodeStatusPolicy(r.Data)
}

// decodeStatusPolicy reads a policy as a local authority returns it or as
// it arrives over the wire.
func decodeStatusPolicy(v any) (bus.StatusPolicy, error) {
	raw, e := json.Marshal(v)
	if e != nil {
		return bus.StatusPolicy{}, errors.New("the status policy is unreadable")
	}
	var view struct {
		Enabled         bool                      `json:"enabled"`
		ClaimFromTriage bool                      `json:"claim_from_triage"`
		Teams           map[string]bus.StatusTeam `json:"teams"`
	}
	if json.Unmarshal(raw, &view) != nil {
		return bus.StatusPolicy{}, errors.New("the status policy is unreadable")
	}
	return bus.StatusPolicy{Enabled: view.Enabled, ClaimFromTriage: view.ClaimFromTriage, Teams: view.Teams}, nil
}

// createState is the policy's create state for team, or "" when the policy
// sets none.
func (s *Service) createState(ctx context.Context, scope bus.Request, team string) (string, error) {
	policy, e := s.statusPolicy(ctx, scope)
	if e != nil {
		return "", e
	}
	if t, ok := policy.Teams[strings.ToLower(team)]; ok && policy.Enabled && t.Create != nil {
		return t.Create.ID, nil
	}
	return "", nil
}

// errStateUnresolved is an issue whose current state could not be told
// by its ID: never guessed.
var errStateUnresolved = errors.New("the issue's workflow state could not be resolved")

// issueStateID is the issue's current workflow state ID, lower-cased.
// Linear's MCP issue read may name the state without its ID, so in order:
// the ID as read; the authoritative IssueState (a hosted server's app);
// the name, when exactly one state of the issue's team has it. Otherwise
// errStateUnresolved; a read that failed is returned as itself.
func (s *Service) issueStateID(ctx context.Context, scope bus.Request, issue linearprovider.IssueV2) (string, error) {
	if issue.StateID != "" {
		return strings.ToLower(issue.StateID), nil
	}
	if s.cfg.IssueState != nil {
		if s.stateReads != nil {
			s.stateReads.Add(1)
		}
		id, team, e := s.cfg.IssueState(ctx, issue.ID)
		if e != nil {
			return "", e
		}
		if !linearprovider.ValidID(strings.ToLower(id)) || !strings.EqualFold(team, issue.TeamID) {
			return "", errStateUnresolved
		}
		return strings.ToLower(id), nil
	}
	if issue.State == "" {
		return "", errStateUnresolved
	}
	states, e := s.StatesV2(ctx, scope, issue.TeamID)
	if e != nil {
		return "", e
	}
	found := ""
	for _, st := range states {
		if st.Name != issue.State {
			continue
		}
		if found != "" {
			return "", errStateUnresolved
		}
		found = strings.ToLower(st.ID)
	}
	if !linearprovider.ValidID(found) {
		return "", errStateUnresolved
	}
	return found, nil
}

// stateMove decides and makes one state move for a marker row: the issue is
// read again, its team must be bound and in the policy, the target in that
// team's workflow, and the current state in the move's "from" set (a claim
// from backlog or unstarted, or triage when the policy allows it; a release
// only from exactly the claim state). A move already in place is done
// (SF-L5). After a failed send the issue is read once more: the target is
// done, an unchanged state is retried, and any other state is what landed,
// never sent again (SF-L3).
func (s *Service) stateMove(ctx context.Context, scope bus.Request, issue, kind string, policy bus.StatusPolicy, retry bool) (string, map[string]any, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return markerFailure(e), nil, e
	}
	current, e := s.IssueV2(ctx, scope, issue)
	if e != nil {
		return markerFailure(e), nil, e
	}
	team := strings.ToLower(current.TeamID)
	skip := func(reason string) (string, map[string]any, error) {
		return "skipped", map[string]any{"skip_reason": reason}, nil
	}
	if !contains(b.LinearTeamIDs, current.TeamID) && !contains(b.LinearTeamIDs, team) {
		return skip("team_unbound")
	}
	t, ok := policy.Teams[team]
	target := t.Claim
	if kind == "release_done" {
		target = t.ReleaseDone
	}
	if !ok || target == nil {
		return skip("team_off")
	}
	if t.Workflow[target.ID] == "" {
		return skip("target_unknown")
	}
	before, e := s.issueStateID(ctx, scope, current)
	if errors.Is(e, errStateUnresolved) {
		return skip("state_unresolved")
	}
	if e != nil {
		return markerFailure(e), nil, e
	}
	done := func(landed string) (string, map[string]any, error) {
		return "done", map[string]any{"state_target": target.ID, "state_landed": landed}, nil
	}
	if before == target.ID {
		return done(before)
	}
	allowed := false
	switch kind {
	case "claim":
		kindOf := t.Workflow[before]
		allowed = kindOf == "backlog" || kindOf == "unstarted" || (kindOf == "triage" && policy.ClaimFromTriage)
	case "release_done":
		allowed = t.Claim != nil && before == t.Claim.ID
	}
	if !allowed {
		if retry && before != "" {
			// An earlier attempt may have been overtaken by a person: what
			// stands now is what landed.
			return done(before)
		}
		return skip("from_state")
	}
	state := target.ID
	_, e = s.WriteV2(ctx, scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: markerOperation(), IssueID: issue, StateID: &state}, marker: true})
	if e == nil {
		return done(target.ID)
	}
	outcome := markerFailure(e)
	if outcome != "uncertain" {
		return outcome, nil, e
	}
	after, re := s.IssueV2(ctx, scope, issue)
	landed := ""
	if re == nil {
		landed, re = s.issueStateID(ctx, scope, after)
	}
	switch {
	case re != nil || landed == "":
		return "uncertain", nil, e
	case landed == target.ID, landed != before:
		return done(landed)
	}
	return "uncertain", nil, e
}

// issueStateQuery reads an issue's state ID and team, read-only.
const issueStateQuery = `query IssueState($id: String!) { issue(id: $id) { id team { id } state { id team { id } } } }`

// GraphQLIssueState is Config.IssueState over a GraphQL read (a hosted
// server's Linear app): the issue's state ID and its team, where the
// state's own team must be the issue's.
func GraphQLIssueState(query func(ctx context.Context, q string, vars map[string]any, out any) error) func(context.Context, string) (string, string, error) {
	return func(ctx context.Context, issue string) (string, string, error) {
		type ref struct {
			ID string `json:"id"`
		}
		var out struct {
			Issue *struct {
				ID    string `json:"id"`
				Team  *ref   `json:"team"`
				State *struct {
					ID   string `json:"id"`
					Team *ref   `json:"team"`
				} `json:"state"`
			} `json:"issue"`
		}
		if e := query(ctx, issueStateQuery, map[string]any{"id": issue}, &out); e != nil {
			return "", "", e
		}
		i := out.Issue
		if i == nil || !strings.EqualFold(i.ID, issue) || i.Team == nil || i.State == nil || i.State.Team == nil || !strings.EqualFold(i.State.Team.ID, i.Team.ID) {
			return "", "", errStateUnresolved
		}
		return i.State.ID, i.Team.ID, nil
	}
}
