package linearboard

import (
	"context"
	"errors"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// viewsProvider lists a team's shared custom views (LINEAR-VIEWS-PLAN §12.2).
type viewsProvider interface {
	Views(context.Context, string, string, linearprovider.PageOptions) (linearprovider.ViewPage, error)
}

// ViewsV2 lists one page of a bound team's shared views, of one kind
// ("issue" or "project") or both. Workspace views and other kinds are left
// out by the provider and only counted; nothing of them is returned.
func (s *Service) ViewsV2(ctx context.Context, scope bus.Request, team, kind string, limit int, opaque string) (linearprovider.ViewPage, error) {
	empty := linearprovider.ViewPage{}
	if e := metadataOptionsV2(team, limit, opaque); e != nil {
		return empty, e
	}
	if kind != "" && linearprovider.ViewKinds[kind] == "" {
		return empty, errors.New("view kind must be issue or project")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return empty, e
	}
	if !contains(b.LinearTeamIDs, team) {
		return empty, outOfScopeError("view team is outside the selected task-board scope")
	}
	c, e := readCursorV2(opaque, digest([]any{"views", s.cfg.ProfileID, b, team, kind}), 1)
	if e != nil {
		return empty, e
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return empty, e
	}
	defer done()
	if _, e = s.verifyReadBindingV2(ctx, p, b); e != nil {
		return empty, e
	}
	vp, ok := p.(viewsProvider)
	if !ok {
		return empty, &linearprovider.Error{Kind: "capability_unavailable", NotSent: true}
	}
	page, e := vp.Views(ctx, team, kind, linearprovider.PageOptions{Limit: limit, Cursor: c.Provider})
	if e != nil {
		return empty, e
	}
	if e = validProviderPageV2(page.NextCursor, page.Complete, c.Provider, len(page.Items), limit); e != nil {
		return empty, e
	}
	for _, v := range page.Items {
		if v.TeamID != team || (kind != "" && v.Kind != kind) {
			return empty, errors.New("provider page escaped the selected team or kind")
		}
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return empty, e
	}
	if !page.Complete {
		c.Provider = page.NextCursor
		if page.NextCursor, e = encodeCursorV2(c); e != nil {
			return empty, e
		}
	}
	return page, nil
}
