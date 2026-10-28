package linearboard

import (
	"context"
	"errors"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// WorkspaceUsersProvider lists the Linear workspace's users.
type WorkspaceUsersProvider interface {
	WorkspaceUsers(context.Context, linearprovider.PageOptions) (linearprovider.Page[linearprovider.WorkspaceUser], error)
}

// maxUserPages bounds a workspace listing: 50 pages of 100 users.
const maxUserPages = 50

// WorkspaceUsers lists every user of the Linear workspace the project's
// board is in, through this person's own connection, for team roles: a
// role needs workspace membership, never a team's. The binding is
// verified first; nothing is stored.
func (s *Service) WorkspaceUsers(ctx context.Context, scope bus.Request) ([]linearprovider.WorkspaceUser, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	if !b.Configured || len(b.LinearTeamIDs) == 0 {
		return nil, errors.New("this project has no Linear board linked")
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	if _, e = s.verifyReadBindingV2(ctx, p, b); e != nil {
		return nil, e
	}
	wp, ok := p.(WorkspaceUsersProvider)
	if !ok {
		return nil, errors.New("this Linear connection can't list the workspace's users")
	}
	out := []linearprovider.WorkspaceUser{}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		if page == maxUserPages {
			return nil, errors.New("the workspace has more users than ClawBridge reads; give the person's Linear user ID")
		}
		got, e := wp.WorkspaceUsers(ctx, linearprovider.PageOptions{Limit: 100, Cursor: cursor})
		if e != nil {
			return nil, e
		}
		for _, u := range got.Items {
			if !seen[u.ID] {
				seen[u.ID] = true
				out = append(out, u)
			}
		}
		if got.Complete || got.NextCursor == "" {
			return out, nil
		}
		cursor = got.NextCursor
	}
}
