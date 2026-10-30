package linearboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// StatusPolicyFile is what the owner writes: state names, resolved per
// bound team when the policy is set.
//
// A top-level key the file leaves out keeps its current value; one it
// includes replaces it (an empty "closes" clears every rule, "done": ""
// clears every done state). Presence is read from the raw file.
type StatusPolicyFile struct {
	Enabled         *bool  `json:"enabled"`
	ClaimFromTriage *bool  `json:"claim_from_triage"`
	Create          string `json:"create"`
	Claim           string `json:"claim"`
	ReleaseDone     string `json:"release_done"`
	// Done is the completed state a done-release reaches in a project
	// agents close; a team without it stops at release_done.
	Done string `json:"done"`
	// Closes says who closes each bound Linear project's issues: agent or
	// human, the default.
	Closes map[string]string `json:"closes"`
	// ProjectFlow lets each bound Linear project's status follow its
	// issues (A4); ProjectTargets overrides the statuses it moves to, by
	// project status ID.
	ProjectFlow    *bool               `json:"project_flow"`
	ProjectTargets *bus.ProjectTargets `json:"project_targets"`
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
// workflow (state ID → type), so a state added later moves nothing. done
// may be missing from a team, which then stops at release_done: the answer
// names each such team when any project is closed by agents.
//
// A file that lets agents close a project they didn't close before
// (R89-M1) is confirmed by confirm with each such project's Linear name,
// before anything is sent, and the write says via cli-tty. Without confirm
// it is refused.
func (s *Service) SetStatusPolicyV2(ctx context.Context, scope bus.Request, raw []byte, expected int64, confirm ConfirmAgentCloses) (any, error) {
	if e := uniqueKeys(raw); e != nil {
		return nil, e
	}
	var file StatusPolicyFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&file) != nil || dec.Decode(new(any)) != io.EOF || file.Enabled == nil {
		return nil, errors.New(`the status policy file must be {"enabled": true|false, "claim_from_triage", "create", "claim", "release_done", "done"} with state names, "closes": {"<Linear project UUID>": "agent"|"human"}, "project_flow": true|false, and "project_targets": {"started": "<project status UUID>", "completed": "<project status UUID>"}`)
	}
	// Keys match as the decoder matches them, ignoring case.
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys)
	present := map[string]bool{}
	for k := range keys {
		present[strings.ToLower(k)] = true
	}
	has := func(key string) bool { return present[key] }
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if !b.Configured || len(b.LinearTeamIDs) == 0 {
		return nil, errors.New("link the task board before setting a status policy")
	}
	// What the file leaves out is kept from the policy now set, so the
	// current policy is read first and never replaced blindly.
	cur, e := s.statusPolicy(ctx, scope)
	if e != nil {
		return nil, errors.New("the current status policy could not be read; nothing was set")
	}
	kept := map[string]bool{}
	notes := []string{}
	closes := map[string]any{}
	agents := false
	source := file.Closes
	if !has("closes") {
		source = cur.Closes
	}
	for project, who := range source {
		project = strings.ToLower(project)
		bound := false
		for _, p := range b.LinearProjectIDs {
			bound = bound || strings.EqualFold(p, project)
		}
		if !bound && !has("closes") {
			notes = append(notes, fmt.Sprintf("closes kept from the current policy names Linear project %s, which is no longer in this board's binding: dropped", project))
			continue
		}
		if !bound {
			return nil, fmt.Errorf("closes names Linear project %q, which is not in this board's binding; nothing was set", project)
		}
		if who != "agent" && who != "human" {
			return nil, fmt.Errorf("closes for Linear project %s must be agent or human; nothing was set", project)
		}
		closes[project] = who
		agents = agents || who == "agent"
		kept["closes"] = kept["closes"] || !has("closes")
	}
	via, e := s.confirmLoosened(ctx, scope, closes, confirm)
	if e != nil {
		return nil, e
	}
	fileNames := map[string]string{"create": file.Create, "claim": file.Claim, "release_done": file.ReleaseDone, "done": file.Done}
	fields := []string{"create", "claim", "release_done", "done"}
	// For a newly bound team, the name every current team that has the
	// field agrees on.
	agreed := map[string]map[string]bool{}
	for _, field := range fields {
		agreed[field] = map[string]bool{}
		for _, t := range cur.Teams {
			if st := stateOf(t, field); st != nil {
				agreed[field][st.Name] = true
			}
		}
	}
	stops := []string{}
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
		byID := map[string]linearprovider.Status{}
		for _, st := range states {
			if st.Type == "" {
				return nil, errors.New("the Linear connection did not report workflow state types; nothing was set")
			}
			workflow[strings.ToLower(st.ID)] = st.Type
			byName[st.Name] = append(byName[st.Name], st)
			byID[strings.ToLower(st.ID)] = st
		}
		entry := map[string]any{"workflow": workflow}
		set := func(field string, st linearprovider.Status) {
			entry[field] = map[string]any{"id": strings.ToLower(st.ID), "name": st.Name, "type": st.Type}
		}
		curTeam, known := cur.Teams[strings.ToLower(team)]
		for _, field := range fields {
			if has(field) {
				name := fileNames[field]
				if name == "" {
					if field == "done" {
						stops = append(stops, team)
					}
					continue
				}
				found := byName[name]
				if field == "done" && len(found) == 0 {
					stops = append(stops, team)
					continue
				}
				if len(found) != 1 {
					return nil, fmt.Errorf("%s state %q is missing or not unique in team %s; nothing was set", field, name, team)
				}
				set(field, found[0])
				continue
			}
			// Left out of the file: keep what the policy has.
			var resolved *linearprovider.Status
			switch old := stateOf(curTeam, field); {
			case known && old != nil:
				if st, ok := byID[strings.ToLower(old.ID)]; ok {
					resolved = &st
				} else if found := byName[old.Name]; len(found) == 1 {
					resolved = &found[0]
				} else if field != "done" {
					return nil, fmt.Errorf("%s state %s kept from the current policy is no longer in team %s; nothing was set", field, old.Name, team)
				} else {
					notes = append(notes, fmt.Sprintf("done state %s kept from the current policy is no longer in team %s: its done-releases stop at release_done", old.Name, team))
				}
			case !known && len(agreed[field]) == 1:
				var name string
				for name = range agreed[field] {
				}
				if found := byName[name]; len(found) == 1 {
					resolved = &found[0]
				} else {
					notes = append(notes, fmt.Sprintf("team %s has no unique %s state named %s, which the current policy uses: left unset", team, field, name))
				}
			case !known && len(agreed[field]) > 1:
				notes = append(notes, fmt.Sprintf("team %s is new and the current teams disagree on the %s state name: left unset", team, field))
			}
			if resolved != nil {
				set(field, *resolved)
				kept[field] = true
			} else if field == "done" {
				stops = append(stops, team)
			}
		}
		teams[strings.ToLower(team)] = entry
	}
	claimFromTriage := cur.ClaimFromTriage
	if file.ClaimFromTriage != nil {
		claimFromTriage = *file.ClaimFromTriage
	} else if claimFromTriage {
		kept["claim_from_triage"] = true
	}
	policy := map[string]any{"enabled": *file.Enabled, "claim_from_triage": claimFromTriage, "teams": teams}
	if len(closes) > 0 {
		policy["closes"] = closes
	}
	projectFlow := cur.ProjectFlow
	if file.ProjectFlow != nil {
		projectFlow = *file.ProjectFlow
	} else if projectFlow {
		kept["project_flow"] = true
	}
	if projectFlow {
		policy["project_flow"] = true
	}
	targets := cur.ProjectTargets
	if has("project_targets") {
		targets = file.ProjectTargets
	} else if targets != nil {
		kept["project_targets"] = true
	}
	if t := targets; t != nil {
		policy["project_targets"] = map[string]any{"started": strings.ToLower(t.Started), "completed": strings.ToLower(t.Completed)}
	}
	if _, e = bus.ParseStatusPolicy(policy); e != nil {
		return nil, e
	}
	scope.Operation = "status_policy_set"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["expected_revision"] = expected
	scope.Args["status_policy"] = policy
	scope.Args["via"] = via
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil && strings.Contains(e.Error(), `unsupported argument "via"`) {
		// A server older than A3 takes no via, and checks nothing of it.
		delete(scope.Args, "via")
		r, e = s.cfg.Executor.Execute(ctx, scope)
	}
	if e != nil {
		return r.Data, e
	}
	if agents && len(stops) > 0 {
		// A2-L3: say where a project agents close will stop short of done.
		sort.Strings(stops)
		notes = append(notes, fmt.Sprintf("closes names projects agents close, but team(s) %s have no done state: their done-releases stop at release_done", strings.Join(stops, ", ")))
	}
	if len(notes) == 0 && len(kept) == 0 {
		return r.Data, nil
	}
	out := map[string]any{}
	if m, ok := r.Data.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	if len(notes) > 0 {
		out["notes"] = notes
	}
	if len(kept) > 0 {
		keys := []string{}
		for k := range kept {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out["kept"] = keys
	}
	return out, nil
}

// stateOf is a policy team's state for a file field, or nil.
func stateOf(t bus.StatusTeam, field string) *bus.StatusState {
	switch field {
	case "create":
		return t.Create
	case "claim":
		return t.Claim
	case "release_done":
		return t.ReleaseDone
	}
	return t.Done
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
		Closes          map[string]string         `json:"closes"`
		ProjectFlow     bool                      `json:"project_flow"`
		ProjectTargets  *bus.ProjectTargets       `json:"project_targets"`
	}
	if json.Unmarshal(raw, &view) != nil {
		return bus.StatusPolicy{}, errors.New("the status policy is unreadable")
	}
	return bus.StatusPolicy{Enabled: view.Enabled, ClaimFromTriage: view.ClaimFromTriage, Teams: view.Teams, Closes: view.Closes, ProjectFlow: view.ProjectFlow, ProjectTargets: view.ProjectTargets}, nil
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

// claimOvertaken reports a release move whose claim move never ran, so the
// issue may still sit where the claim would have moved it from. Two cases:
// the bus superseded the claim because the release came first (A1), and the
// claim froze the same policy revision as this move (A1-L1); or the claim
// was skipped because the team's flow was off then (team_off,
// team_not_in_policy or policy_off), where the revisions differ by nature:
// the policy was turned on after the claim was skipped. Any other claim
// outcome, and a bus that predates both (no claim_skip_reason), keeps the
// exact rule.
func claimOvertaken(g map[string]any) bool {
	state, _ := g["claim_state"].(string)
	if state == "skipped" {
		reason, _ := g["claim_skip_reason"].(string)
		return reason == "team_off" || reason == "team_not_in_policy" || reason == "policy_off"
	}
	claim, ok := policyRevision(g["claim_policy_revision"])
	move, same := policyRevision(g["policy_revision"])
	return state == "superseded" && ok && same && claim == move
}

// inClaimFromSet is a state a claim moves from: backlog or unstarted, or
// triage when the policy allows it.
func inClaimFromSet(kindOf string, policy bus.StatusPolicy) bool {
	return kindOf == "backlog" || kindOf == "unstarted" || (kindOf == "triage" && policy.ClaimFromTriage)
}

// stateMove decides and makes one state move for a marker row: the issue is
// read again, its team must be bound and in the policy (a bound team the policy lacks is skipped as team_not_in_policy), the target in that
// team's workflow, and the current state in the move's "from" set (a claim
// from the claim's from-set; a release only from exactly the claim state,
// or from the claim's from-set too when its claim was overtaken or skipped
// because the team's flow was off). A move
// already in place is done (SF-L5). After a failed send the issue is read
// once more: the target is done, an unchanged state is retried, and any
// other state is what landed, never sent again (SF-L3).
func (s *Service) stateMove(ctx context.Context, scope bus.Request, issue, kind string, policy bus.StatusPolicy, overtaken, retry bool) (string, map[string]any, error) {
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
		// In a project agents close (A2), a done-release goes on to the
		// team's done state; elsewhere it stops at release_done.
		target = t.ReleaseDone
		if t.Done != nil && current.ProjectKnown && policy.AgentCloses(strings.ToLower(current.ProjectID)) {
			target = t.Done
		}
	}
	if !ok {
		// Bound, but the policy was set before the team was bound.
		return skip("team_not_in_policy")
	}
	if target == nil {
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
		allowed = inClaimFromSet(t.Workflow[before], policy)
	case "release_done":
		// A release that overtook its claim finds the issue where the claim
		// would have moved it from.
		allowed = t.Claim != nil && (before == t.Claim.ID || (overtaken && inClaimFromSet(t.Workflow[before], policy)))
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
	op := markerOperation()
	_, e = s.WriteV2(ctx, scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: op, IssueID: issue, StateID: &state}, marker: true})
	if e == nil {
		return done(target.ID)
	}
	// A failed send settles with its operation ID, so the bus can close
	// its ledger record once a reread proves the move's outcome: this
	// attempt's own reread below, or a later attempt's.
	sent := map[string]any{"operation_id": op}
	outcome := markerFailure(e)
	if outcome != "uncertain" {
		return outcome, sent, e
	}
	after, re := s.IssueV2(ctx, scope, issue)
	landed := ""
	if re == nil {
		landed, re = s.issueStateID(ctx, scope, after)
	}
	switch {
	case re != nil || landed == "":
		return "uncertain", sent, e
	case landed == target.ID, landed != before:
		outcome, extra, err := done(landed)
		extra["operation_id"] = op
		return outcome, extra, err
	}
	return "uncertain", sent, e
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
