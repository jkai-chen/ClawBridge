package linearboard

import (
	"context"
	"errors"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

func checkLabelV2(label linearprovider.Label, team string) error {
	if !linearprovider.ValidID(label.ID) || label.TeamID != team || label.Global {
		return errors.New("label team scope is not verified")
	}
	return nil
}
func eligibleLabelV2(label linearprovider.Label) error {
	if !label.EligibilityKnown || label.Archived || label.Group || label.Global {
		return errors.New("label is not proven eligible for application")
	}
	return nil
}
func checkMemberV2(member linearprovider.Member, team string) error {
	if !linearprovider.ValidID(member.ID) || member.TeamID != team {
		return errors.New("user team membership is not verified")
	}
	return nil
}
func validProviderPageV2(next string, complete bool, previous string, count, limit int) error {
	if count > limit || len(next) > 4096 || (next == "" && !complete) || (next != "" && complete) || (next != "" && next == previous) {
		return errors.New("provider continuation is incomplete, contradictory or non-advancing")
	}
	return nil
}
func findLabelV2(ctx context.Context, p ProviderV2, team, id string) (*linearprovider.Label, error) {
	cursor := ""
	seen := map[string]bool{}
	for pageNumber := 0; pageNumber < 64; pageNumber++ {
		page, e := p.Labels(ctx, team, linearprovider.PageOptions{Limit: 100, Cursor: cursor})
		if e != nil {
			return nil, e
		}
		if e = validProviderPageV2(page.NextCursor, page.Complete, cursor, len(page.Items), 100); e != nil {
			return nil, e
		}
		seenIDs := map[string]string{}
		var found *linearprovider.Label
		// Other rows may be workspace, unscoped or ineligible labels; they are
		// still checked for contradictions but only the requested row's scope
		// and eligibility decide the lookup.
		for _, label := range page.Items {
			if !linearprovider.ValidID(label.ID) {
				return nil, errors.New("label identity is invalid")
			}
			h := digest(label)
			if old, ok := seenIDs[label.ID]; ok && old != h {
				return nil, errors.New("contradictory label identity")
			}
			seenIDs[label.ID] = h
			if label.ID == id {
				copy := label
				found = &copy
			}
		}
		if found != nil {
			if e = checkLabelV2(*found, team); e != nil {
				return nil, e
			}
			if e = eligibleLabelV2(*found); e != nil {
				return nil, e
			}
			return found, nil
		}
		if page.Complete {
			return nil, nil
		}
		if seen[page.NextCursor] {
			return nil, errors.New("label pagination cycle")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	return nil, errors.New("label discovery exceeded bounded verification pages")
}
func findMemberV2(ctx context.Context, p ProviderV2, team, id string) error {
	cursor := ""
	seen := map[string]bool{}
	for pageNumber := 0; pageNumber < 64; pageNumber++ {
		page, e := p.Members(ctx, team, linearprovider.PageOptions{Limit: 100, Cursor: cursor})
		if e != nil {
			return e
		}
		if e = validProviderPageV2(page.NextCursor, page.Complete, cursor, len(page.Items), 100); e != nil {
			return e
		}
		seenIDs := map[string]string{}
		var found *linearprovider.Member
		for _, member := range page.Items {
			if e = checkMemberV2(member, team); e != nil {
				return e
			}
			h := digest(member)
			if old, ok := seenIDs[member.ID]; ok && old != h {
				return errors.New("contradictory member identity")
			}
			seenIDs[member.ID] = h
			if member.ID == id {
				copy := member
				found = &copy
			}
		}
		if found != nil {
			if !found.EligibilityKnown || !found.Active {
				return errors.New("assignee is not proven active and eligible for this team")
			}
			return nil
		}
		if page.Complete {
			return errors.New("assignee does not belong to the issue team")
		}
		if seen[page.NextCursor] {
			return errors.New("membership pagination cycle")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	return errors.New("membership discovery exceeded bounded verification pages")
}
func metadataOptionsV2(team string, limit int, cursor string) error {
	if !linearprovider.ValidID(team) || limit < 1 || limit > 100 || len(cursor) > 8192 {
		return errors.New("metadata requires team UUID, limit1–100 and bounded cursor")
	}
	return nil
}
func (s *Service) LabelsV2(ctx context.Context, scope bus.Request, team string, limit int, opaque string) (linearprovider.Page[linearprovider.Label], error) {
	empty := linearprovider.Page[linearprovider.Label]{}
	if e := metadataOptionsV2(team, limit, opaque); e != nil {
		return empty, e
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return empty, e
	}
	if !contains(b.LinearTeamIDs, team) {
		return empty, errors.New("metadata team is outside allowed scope")
	}
	key := digest([]any{"labels", s.cfg.ProfileID, b, team})
	c, e := readCursorV2(opaque, key, 1)
	if e != nil {
		return empty, e
	}
	_, p, e := s.open(ctx)
	if e != nil {
		return empty, e
	}
	defer p.Close()
	if _, e = verifyBindingV2(ctx, p, b); e != nil {
		return empty, e
	}
	p2, ok := p.(ProviderV2)
	if !ok {
		return empty, errors.New("native provider lacks V2 labels capability")
	}
	page, e := p2.Labels(ctx, team, linearprovider.PageOptions{Limit: limit, Cursor: c.Provider})
	if e != nil {
		return empty, e
	}
	if e = validProviderPageV2(page.NextCursor, page.Complete, c.Provider, len(page.Items), limit); e != nil {
		return empty, e
	}
	// The listing returns only rows verified to be in this team's scope;
	// workspace and unscoped rows are omitted rather than failing the page.
	seen := map[string]string{}
	items := []linearprovider.Label{}
	for _, label := range page.Items {
		if !linearprovider.ValidID(label.ID) {
			return empty, errors.New("label identity is invalid")
		}
		h := digest(label)
		if old, ok := seen[label.ID]; ok && old != h {
			return empty, errors.New("contradictory label identity")
		}
		seen[label.ID] = h
		if checkLabelV2(label, team) == nil {
			items = append(items, label)
		}
	}
	page.Items = items
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return empty, e
	}
	if !page.Complete {
		c.Provider = page.NextCursor
		page.NextCursor, e = encodeCursorV2(c)
	}
	return page, e
}
func (s *Service) MembersV2(ctx context.Context, scope bus.Request, team string, limit int, opaque string) (linearprovider.Page[linearprovider.Member], error) {
	empty := linearprovider.Page[linearprovider.Member]{}
	if e := metadataOptionsV2(team, limit, opaque); e != nil {
		return empty, e
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return empty, e
	}
	if !contains(b.LinearTeamIDs, team) {
		return empty, errors.New("metadata team is outside allowed scope")
	}
	key := digest([]any{"members", s.cfg.ProfileID, b, team})
	c, e := readCursorV2(opaque, key, 1)
	if e != nil {
		return empty, e
	}
	_, p, e := s.open(ctx)
	if e != nil {
		return empty, e
	}
	defer p.Close()
	if _, e = verifyBindingV2(ctx, p, b); e != nil {
		return empty, e
	}
	p2, ok := p.(ProviderV2)
	if !ok {
		return empty, errors.New("native provider lacks V2 membership capability")
	}
	page, e := p2.Members(ctx, team, linearprovider.PageOptions{Limit: limit, Cursor: c.Provider})
	if e != nil {
		return empty, e
	}
	if e = validProviderPageV2(page.NextCursor, page.Complete, c.Provider, len(page.Items), limit); e != nil {
		return empty, e
	}
	seen := map[string]string{}
	for _, member := range page.Items {
		if e = checkMemberV2(member, team); e != nil {
			return empty, e
		}
		h := digest(member)
		if old, ok := seen[member.ID]; ok && old != h {
			return empty, errors.New("contradictory member identity")
		}
		seen[member.ID] = h
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return empty, e
	}
	if !page.Complete {
		c.Provider = page.NextCursor
		page.NextCursor, e = encodeCursorV2(c)
	}
	return page, e
}
