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
	before := strings.ToLower(current.StateID)
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
	switch landed := strings.ToLower(after.StateID); {
	case re != nil || landed == "":
		return "uncertain", nil, e
	case landed == target.ID, landed != before:
		return done(landed)
	}
	return "uncertain", nil, e
}
