package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/tui"
)

// The TUI's team roles view (TEAM-ROLES-TUI-PLAN): it reads and changes
// roles as linear roles does, with the same helpers, so a change made in
// the TUI is the one the CLI would make.

// tuiRolesUsers lists the Linear workspace's users through the viewer's
// connection; tests replace it.
var tuiRolesUsers = func(ctx context.Context, service *linearboard.Service, scope bus.Request) ([]linearprovider.WorkspaceUser, error) {
	return service.WorkspaceUsers(ctx, scope)
}

// rolesView reads the mapping with the TUI's scope, the viewer's project
// role, and the workspace's users through the viewer's own connection
// (best effort: without them people show by ID).
func (s *linearTUISource) rolesView(ctx context.Context, service *linearboard.Service, data *tui.LinearData) error {
	current, youLead, err := s.rolesRead(ctx)
	if err != nil {
		return err
	}
	v := tui.TeamRolesView{Revision: current.Revision, People: current.People, YouLead: youLead}
	for _, t := range current.Teams {
		v.Teams = append(v.Teams, tui.RolesTeam{TeamID: t.TeamID, Leads: t.Leads, Bound: t.TeamBound})
	}
	person := s.scope
	person.Session, person.Operation, person.Args = "", "taskboard_access", map[string]any{"ledger": 1}
	if access, e := s.executor.Execute(ctx, person); e == nil {
		if m, ok := access.Data.(map[string]any); ok {
			v.Owner = m["project_role"] == "owner"
		}
	}
	if service == nil {
		v.NamesErr = "choose your Linear connection on the Linear screen first"
	} else if users, e := tuiRolesUsers(ctx, service, s.scope); e != nil {
		v.NamesErr = "your Linear connection couldn't list the workspace's users: " + e.Error()
	} else {
		v.Users = users
	}
	data.Roles = &v
	return nil
}

// rolesRead is team_roles_get with the TUI's scope, and the teams the
// reader leads.
func (s *linearTUISource) rolesRead(ctx context.Context) (rolesView, []string, error) {
	r := s.scope
	r.Operation, r.Args = "team_roles_get", instanceArgs(s.scope)
	result, err := s.executor.Execute(ctx, r)
	if err != nil {
		return rolesView{}, nil, err
	}
	v, err := decodeRolesView(result.Data)
	if err != nil {
		return v, nil, err
	}
	youLead := []string{}
	if m, ok := result.Data.(map[string]any); ok {
		if list, ok := m["you_lead"].([]any); ok {
			for _, x := range list {
				if id, ok := x.(string); ok {
					youLead = append(youLead, id)
				}
			}
		} else if list, ok := m["you_lead"].([]string); ok {
			youLead = append(youLead, list...)
		}
	}
	return v, youLead, nil
}

// rolesSet previews or stores one change, as the person with no session.
// The next mapping is built from the stored one at the change's revision,
// with the teams and their leads sent back as read unless the change is
// a lead's: a team's lead may only change engineer and reviewer rows.
func (s *linearTUISource) rolesSet(ctx context.Context, service *linearboard.Service, c *tui.RolesChange, data *tui.LinearData) error {
	if c == nil {
		return errors.New("no change to make")
	}
	if s.scope.Session != "" {
		return errors.New("team roles are changed by a person with no session: open the TUI without --session. Nothing was changed")
	}
	if marker := agentMarker(closesEnv()); marker != "" {
		return fmt.Errorf("team roles are a person's to change, and %s says an agent runs this TUI; nothing was changed", marker)
	}
	if service == nil {
		return errors.New("changing team roles looks people up with your Linear connection: choose it on the Linear screen first")
	}
	current, _, err := s.rolesRead(ctx)
	if err != nil {
		return err
	}
	if current.Revision != c.Revision {
		return fmt.Errorf("team roles changed: current revision is %d; read them again", current.Revision)
	}
	users, err := tuiRolesUsers(ctx, service, s.scope)
	if err != nil {
		return err
	}
	next := bus.TeamRoles{Teams: current.mappingTeams(), People: current.People}
	switch c.Kind {
	case "lead":
		next = withLead(next, c.Team, c.User, c.Remove)
	case "person":
		next = withPersonRole(next, c.User, c.Role, c.Teams)
	default:
		return errors.New("a team roles change is a lead or a person")
	}
	if err = checkRoleUsers(next, users); err != nil {
		return err
	}
	// The session is refused above: this is the person's own request.
	person := s.scope
	person.Operation = "team_roles_set"
	person.Args = map[string]any{"expected_revision": c.Revision, "confirm": c.Confirm, "mapping": rolesArg(next)}
	result, err := s.executor.Execute(ctx, person)
	if err != nil {
		return err
	}
	out := tui.RolesSetResult{Revision: c.Revision}
	if m, ok := result.Data.(map[string]any); ok {
		out.Preview = m["preview"] == true
		if change, ok := m["change"].(map[string]any); ok {
			out.Change = change
		}
		if n, ok := m["revision"].(float64); ok {
			out.Revision = int64(n)
		} else if n, ok := m["revision"].(int64); ok {
			out.Revision = n
		} else if n, ok := m["revision"].(int); ok {
			out.Revision = int64(n)
		}
	}
	data.RolesSet = &out
	return nil
}

// withPersonRole gives id role in teams, keeping their account link, or
// removes them when teams is empty. Teams and their leads are untouched:
// a person who leads a team changes through withLead.
func withPersonRole(m bus.TeamRoles, id, role string, teams []string) bus.TeamRoles {
	people := []bus.TeamRolePerson{}
	link, found := "", false
	for _, p := range m.People {
		if p.LinearUserID == id {
			link, found = p.Principal, true
			continue
		}
		people = append(people, p)
	}
	if len(teams) > 0 {
		kept := []string{}
		for _, t := range teams {
			if !slices.Contains(kept, t) {
				kept = append(kept, t)
			}
		}
		p := bus.TeamRolePerson{LinearUserID: id, Role: role, Teams: kept, Principal: link}
		if found {
			// Keep the person where they were, so a preview's order is stable.
			for i, q := range m.People {
				if q.LinearUserID == id {
					people = slices.Insert(people, min(i, len(people)), p)
					return bus.TeamRoles{Teams: m.Teams, People: people}
				}
			}
		}
		people = append(people, p)
	}
	return bus.TeamRoles{Teams: m.Teams, People: people}
}
