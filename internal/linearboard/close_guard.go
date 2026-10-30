package linearboard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// Who closes an issue (A2, live/STATUS-FLOW-DESIGN.md §9). An agent's write
// (a session's, or automation's) never sets a canceled state where its type
// is known, policy or not, except through the cancel and duplicate
// operations (agent_cancel.go). Under an enabled status policy it also sets a
// completed state only in a project the policy says agents close, and
// never moves a started issue from a project people close into one agents
// close. A person's write without a session is never refused here. It
// governs cooperative agents, not a hostile local process.

// errStateTypeUnknown refuses an agent's state write under an enabled
// policy when no source knows the state's type.
var errStateTypeUnknown = errors.New("cannot verify the workflow state's type, so the status policy cannot be applied; nothing was sent. The project owner sets the status policy again")

// errAgentCancelUpdate refuses an agent's plain update to a canceled or
// duplicate state: an agent cancels only with the cancel and duplicate
// operations, where a person allowed the project's kind.
var errAgentCancelUpdate = errors.New("agents cancel or mark Duplicate only with taskboard_cancel_v2 / taskboard_duplicate_v2 (clawbridge linear cancel|duplicate), where a person allowed the project's kind. Nothing was sent")

// errStateTypeUnread is a failed type read, told without the reader's own
// text.
var errStateTypeUnread = errors.New("cannot read the workflow state's type; nothing was sent")

// stateKind is a team's state's type for an agent's write, in order: the
// status list's raw type (Linear's duplicate counts as canceled); the
// policy's workflow snapshot; the list's type; then StateType (a hosted
// server's app). "" is unknown.
func (s *Service) stateKind(ctx context.Context, policy bus.StatusPolicy, states []linearprovider.Status, team, state string) (string, error) {
	state = strings.ToLower(state)
	var listed *linearprovider.Status
	for i := range states {
		if strings.EqualFold(states[i].ID, state) && strings.EqualFold(states[i].TeamID, team) {
			listed = &states[i]
		}
	}
	if listed != nil && listed.Raw != "" {
		if kind := linearprovider.GuardType(listed.Raw); kind != "" {
			return kind, nil
		}
	}
	if kind := policy.Teams[strings.ToLower(team)].Workflow[state]; kind != "" {
		return kind, nil
	}
	if listed != nil && listed.Type != "" {
		return linearprovider.GuardType(listed.Type), nil
	}
	if s.cfg.StateType == nil {
		return "", nil
	}
	raw, e := s.cfg.StateType(ctx, team, state)
	if e != nil {
		return "", e
	}
	return linearprovider.GuardType(raw), nil
}

// closeGuard decides an agent's write before anything is sent. states is
// the target team's status list, read for the write's state; current is
// the issue as read (an update), and project the issue's project after
// the write.
func (s *Service) closeGuard(ctx context.Context, scope bus.Request, in WriteInputV2, states []linearprovider.Status, team string, current linearprovider.IssueV2, project string) error {
	moves := in.Operation == "update" && project != current.ProjectID
	if in.StateID == nil && !moves {
		return nil
	}
	policy, e := s.statusPolicy(ctx, scope)
	if e != nil {
		return e
	}
	if in.StateID != nil {
		kind, e := s.stateKind(ctx, policy, states, team, *in.StateID)
		if e != nil {
			return errStateTypeUnread
		}
		// A cancel or duplicate made its own checks (agent_cancel.go);
		// any other write by an agent to such a state is refused.
		if kind == "canceled" && !in.cancelStep {
			return errAgentCancelUpdate
		}
		if !policy.Enabled {
			return nil
		}
		if kind == "" {
			return errStateTypeUnknown
		}
		if kind == "completed" {
			// A2-M1: an update is judged by where the issue is and where it
			// goes, so a move into a project agents close cannot complete
			// an issue of one people close.
			judged := []string{project}
			if in.Operation == "update" {
				judged = []string{current.ProjectID, project}
			}
			for _, p := range judged {
				if !policy.AgentCloses(p) {
					return humanCloses(p)
				}
			}
		}
	}
	if !policy.Enabled || !moves || !policy.AgentCloses(project) || policy.AgentCloses(current.ProjectID) {
		return nil
	}
	// The hop rule: a started issue may not leave a project people close
	// for one agents close, then be completed there.
	before, e := s.issueStateID(ctx, scope, current)
	if e != nil && !errors.Is(e, errStateUnresolved) {
		return e
	}
	kind := ""
	if before != "" {
		if kind, e = s.stateKind(ctx, policy, nil, current.TeamID, before); e != nil {
			return errStateTypeUnread
		}
	}
	switch kind {
	case "triage", "backlog", "unstarted":
		return nil
	case "":
		return errStateTypeUnknown
	}
	return fmt.Errorf("people close the issues of Linear project %s, and this issue is under way: an agent may not move it into a project agents close. Nothing was sent", projectName(current.ProjectID))
}

func humanCloses(project string) error {
	return fmt.Errorf("people close the issues of Linear project %s under the status policy: an agent may not complete them. Nothing was sent", projectName(project))
}

func projectName(project string) string {
	if project == "" {
		return "(none: an issue in no project)"
	}
	return project
}

// StateTypeCache reads teams' workflow state types (Linear's own, raw)
// through read, keeping each team's answer for ttl. A state missing from a
// kept answer is read once more before it is called unknown, so a state
// added since is never refused for being new. Drop forgets every answer,
// as after a failed write.
type StateTypeCache struct {
	read    func(ctx context.Context, team string) (map[string]string, error)
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]stateTypes
}

type stateTypes struct {
	types map[string]string
	at    time.Time
}

func NewStateTypeCache(read func(ctx context.Context, team string) (map[string]string, error), ttl time.Duration, now func() time.Time) *StateTypeCache {
	if now == nil {
		now = time.Now
	}
	return &StateTypeCache{read: read, ttl: ttl, now: now, entries: map[string]stateTypes{}}
}

// Type is Config.StateType over the cache.
func (c *StateTypeCache) Type(ctx context.Context, team, state string) (string, error) {
	team, state = strings.ToLower(team), strings.ToLower(state)
	c.mu.Lock()
	kept, ok := c.entries[team]
	c.mu.Unlock()
	if ok && c.now().Sub(kept.at) < c.ttl {
		if kind, found := kept.types[state]; found {
			return kind, nil
		}
	}
	types, e := c.read(ctx, team)
	if e != nil {
		return "", e
	}
	c.mu.Lock()
	c.entries[team] = stateTypes{types: types, at: c.now()}
	c.mu.Unlock()
	return types[state], nil
}

// Drop forgets every kept answer.
func (c *StateTypeCache) Drop() {
	c.mu.Lock()
	c.entries = map[string]stateTypes{}
	c.mu.Unlock()
}

// stateTypesQuery reads one team's workflow states with Linear's own types.
const stateTypesQuery = `query StateTypes($team: ID!) { workflowStates(first: 100, filter: { team: { id: { eq: $team } } }) { nodes { id type team { id } } pageInfo { hasNextPage } } }`

// GraphQLStateTypes reads a team's workflow state types over GraphQL (a
// hosted server's Linear app): state ID → Linear's type, lower-cased.
// Every state must be the team's, and the list complete.
func GraphQLStateTypes(query func(ctx context.Context, q string, vars map[string]any, out any) error) func(context.Context, string) (map[string]string, error) {
	return func(ctx context.Context, team string) (map[string]string, error) {
		var out struct {
			WorkflowStates struct {
				Nodes []struct {
					ID, Type string
					Team     struct{ ID string }
				}
				PageInfo struct{ HasNextPage bool }
			}
		}
		if e := query(ctx, stateTypesQuery, map[string]any{"team": team}, &out); e != nil {
			return nil, e
		}
		if out.WorkflowStates.PageInfo.HasNextPage {
			return nil, errors.New("the team has more workflow states than one read returns")
		}
		types := map[string]string{}
		for _, n := range out.WorkflowStates.Nodes {
			id := strings.ToLower(n.ID)
			if !linearprovider.ValidID(id) || !strings.EqualFold(n.Team.ID, team) {
				return nil, errors.New("the workflow state read answered an unverified state")
			}
			types[id] = strings.ToLower(n.Type)
		}
		return types, nil
	}
}
