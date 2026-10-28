package linearprovider

import "context"

// WorkspaceUser is one user of the Linear workspace, for team roles
// (live/TEAM-ROLE-MAPPING-DESIGN.md): a role needs workspace membership,
// never a team's. Each flag says whether the listing stated it.
type WorkspaceUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Email is for finding a person by email; never stored or shown.
	Email       string `json:"-"`
	Active      bool   `json:"active"`
	ActiveKnown bool   `json:"active_known"`
	Guest       bool   `json:"guest"`
	GuestKnown  bool   `json:"guest_known"`
}

// Eligible is an active, non-guest user, both stated by Linear (T2'):
// a flag the listing leaves out fails closed.
func (u WorkspaceUser) Eligible() bool {
	return u.ActiveKnown && u.Active && u.GuestKnown && !u.Guest
}

// WorkspaceUsers lists the workspace's users with list_users and no team
// filter. It reads only.
func (p *Provider) WorkspaceUsers(ctx context.Context, o PageOptions) (Page[WorkspaceUser], error) {
	args, e := pageArgs(o)
	if e != nil {
		return Page[WorkspaceUser]{}, e
	}
	m, e := p.v2Call(ctx, []string{"list_users"}, args...)
	if e != nil {
		return Page[WorkspaceUser]{}, e
	}
	rows, cursor, complete, e := list(m, "users")
	if e != nil {
		return Page[WorkspaceUser]{}, e
	}
	out := Page[WorkspaceUser]{Items: []WorkspaceUser{}, NextCursor: cursor, Complete: complete}
	for _, row := range rows {
		id, e := entityID(row, "userId")
		if e != nil {
			return Page[WorkspaceUser]{}, e
		}
		active, activeKnown, e := explicitBool(row, "active", "isActive")
		if e != nil {
			return Page[WorkspaceUser]{}, e
		}
		guest, guestKnown, e := explicitBool(row, "guest", "isGuest")
		if e != nil {
			return Page[WorkspaceUser]{}, e
		}
		out.Items = append(out.Items, WorkspaceUser{ID: id, Name: str(row, "name"), Email: str(row, "email"), Active: active, ActiveKnown: activeKnown, Guest: guest, GuestKnown: guestKnown})
	}
	return out, nil
}
