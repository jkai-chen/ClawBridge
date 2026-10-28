package linearboard

import (
	"context"
	"errors"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// TeamLabelledInputV2 is one page of a bound team's issues carrying a label,
// updated after a time, across every project of the team.
type TeamLabelledInputV2 struct {
	TeamID, LabelID, UpdatedAfter string
	// Cursor is the previous page's NextCursor, as returned.
	Cursor string
	Limit  int
}

// TeamIssueRefV2 names one row of that page. OutOfBinding is set only when
// the provider reported the issue's project and no bound pair has it; a row
// without it still needs IssueV2 to prove it is in the binding.
type TeamIssueRefV2 struct {
	ID           string
	OutOfBinding bool
}

type TeamLabelledPageV2 struct {
	Items      []TeamIssueRefV2
	NextCursor string
	Complete   bool
	// FilteredOtherTeam counts rows of an unlisted team the provider
	// returned (a sub-team under its parent's filter), dropped unshown.
	FilteredOtherTeam int
}

// TeamLabelledV2 reads one page of a bound team's issues carrying a label
// verified for that team, with no project filter: one query per team
// however many projects the binding pairs it with (intake S1). It returns
// issue IDs only; every row is proven to be the team's, to carry the label
// and to be updated after the time, but not to be in the binding.
func (s *Service) TeamLabelledV2(ctx context.Context, scope bus.Request, in TeamLabelledInputV2) (TeamLabelledPageV2, error) {
	empty := TeamLabelledPageV2{}
	if in.Limit < 1 || in.Limit > 100 || len(in.Cursor) > 4096 || !linearprovider.ValidID(in.LabelID) {
		return empty, errors.New("invalid team label query")
	}
	after, ok := linearprovider.UpdatedAfterTime(in.UpdatedAfter)
	if !ok {
		return empty, errors.New("updated_after requires an absolute RFC 3339 time")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return empty, e
	}
	if !contains(b.LinearTeamIDs, in.TeamID) {
		return empty, outOfScopeError("team is outside the selected task-board scope")
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return empty, e
	}
	defer done()
	v, e := s.verifyReadBindingV2(ctx, p, b)
	if e != nil {
		return empty, e
	}
	p2, ok := p.(ProviderV2)
	if !ok {
		return empty, errors.New("native provider lacks V2 issue capability")
	}
	// The label may be the team's own or, for a sub-team, its bound parent's
	// (CROSS-TEAM-BINDING §3.2); a names-only row is then proven against the
	// listing of the team that holds it. A row carrying the team's own copy
	// of the name also passes: this read serves intake, for which every
	// label of the name's set means the same.
	owner := ""
	for _, t := range labelTeamsV2(v, b, in.TeamID) {
		label, e := findLabelV2(ctx, p2, t, in.LabelID)
		if e != nil {
			return empty, e
		}
		if label != nil {
			owner = t
			break
		}
	}
	if owner == "" {
		return empty, ErrLabelNotVerified
	}
	page, e := p2.IssuesV2(ctx, linearprovider.IssueQueryV2{IssueQuery: linearprovider.IssueQuery{TeamID: in.TeamID, Limit: in.Limit, Cursor: in.Cursor}, LabelID: in.LabelID, UpdatedAfter: in.UpdatedAfter})
	if e != nil {
		return empty, e
	}
	if len(page.Items) > in.Limit || (page.NextCursor == "" && !page.Complete) || (page.NextCursor != "" && page.Complete) || len(page.NextCursor) > 4096 || (page.NextCursor != "" && page.NextCursor == in.Cursor) {
		return empty, errors.New("incomplete or contradictory issue continuation")
	}
	out := TeamLabelledPageV2{Items: []TeamIssueRefV2{}, NextCursor: page.NextCursor, Complete: page.Complete}
	filteredName := ""
	for _, issue := range page.Items {
		if !linearprovider.ValidID(issue.ID) {
			return empty, errors.New("provider page escaped the selected team")
		}
		if issue.TeamID != in.TeamID {
			// A sub-team's row under its parent's filter: a listed team's
			// own read returns it; any other is dropped and counted.
			if !contains(b.LinearTeamIDs, issue.TeamID) {
				out.FilteredOtherTeam++
			}
			continue
		}
		if updated, ok := linearprovider.UpdatedAfterTime(issue.UpdatedAt); !ok || updated.Before(after) {
			return empty, errors.New("provider did not prove the updated_after filter")
		}
		if !issue.LabelsKnown {
			if filteredName == "" {
				if filteredName, e = filteredLabelNameV2(ctx, p2, owner, in.LabelID); e != nil {
					return empty, e
				}
			}
			if !contains(issue.LabelNames, filteredName) {
				return empty, errors.New("provider did not prove the selected label filter")
			}
		} else if !contains(issue.LabelIDs, in.LabelID) {
			return empty, errors.New("provider did not prove the selected label filter")
		}
		out.Items = append(out.Items, TeamIssueRefV2{ID: issue.ID, OutOfBinding: outsidePairs(issue, b, v)})
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return empty, e
	}
	return out, nil
}

// outsidePairs: a project_set binding whose pairs don't hold the project
// the provider reported. It can only rule an issue out; an unreported
// project is left to IssueV2's proof.
func outsidePairs(issue linearprovider.IssueV2, b bus.TaskboardBindingV2, v verifiedV2) bool {
	if b.Mode != "project_set" || !issue.ProjectKnown {
		return false
	}
	for _, pair := range v.Pairs {
		if pair.TeamID == issue.TeamID && pair.ProjectID == issue.ProjectID {
			return false
		}
	}
	return true
}
