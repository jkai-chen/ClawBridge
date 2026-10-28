package linearboard

import (
	"context"
	"errors"
	"slices"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// Labels across a parent team and its sub-teams (CROSS-TEAM-BINDING-DESIGN
// §3.2). Linear lets a sub-team's issues use labels of the sub-team, its
// parent or the workspace, and a sub-team made from its parent's settings
// holds copies of the parent's labels under new IDs. So the paths that find
// a label by name (needs-agent, agent-working) and the checks that a label
// may go on an issue's team use a label set: the team's own label of that
// name, and its parent's when the binding binds the parent too. What
// authorizes by label ID (label policy, kinds, views) stays by ID.

// labelSetPages bounds each team's label walk for a label set.
const labelSetPages = 10

var (
	// ErrLabelAmbiguous: a team lists more than one label of the name, so
	// none is guessed.
	ErrLabelAmbiguous = errors.New("the team has more than one label of that name; none is guessed")
	// ErrLabelWalk: the walk hit its bound before the listing completed, so
	// a second label of the name may be hidden.
	ErrLabelWalk = errors.New("the team has too many labels to prove a label name unique; nothing was sent")
	// ErrLabelNotVerified: the label isn't one the team's issues can carry.
	ErrLabelNotVerified = errors.New("label is not verified for the team")
)

// labelTeamsV2 are the teams whose labels an issue of team may carry: the
// team, then its parent when Linear reports one and the binding binds it.
func labelTeamsV2(v verifiedV2, b bus.TaskboardBindingV2, team string) []string {
	out := []string{team}
	for _, t := range v.Teams {
		if t.ID == team && t.ParentID != "" && t.ParentID != team && contains(b.LinearTeamIDs, t.ParentID) {
			out = append(out, t.ParentID)
		}
	}
	return out
}

// LabelSetV2 is every label of one name an issue of a team may carry.
type LabelSetV2 struct {
	// IDs are the set, the team's own first; a workspace label, listed for
	// both teams, appears once.
	IDs []string `json:"ids"`
	// Apply is the label to put on an issue: the team's own when it is
	// proven eligible, else the parent's; "" when neither is.
	Apply string `json:"apply,omitempty"`
	// Scopes says where each of IDs is listed: "team", "parent", or
	// "workspace" when both teams list it.
	Scopes []string `json:"scopes"`
	// Calls counts the label pages read.
	Calls int `json:"calls"`
}

// LabelSetV2 resolves the label set of name for a bound team, by exact
// name, archived labels and groups left out. One team listing two labels of
// the name is ErrLabelAmbiguous; a walk cut off is ErrLabelWalk.
func (s *Service) LabelSetV2(ctx context.Context, scope bus.Request, team, name string) (LabelSetV2, error) {
	var set LabelSetV2
	if !linearprovider.ValidID(team) || name == "" {
		return set, errors.New("a label set needs a team UUID and a name")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return set, e
	}
	if !contains(b.LinearTeamIDs, team) {
		return set, errors.New("metadata team is outside allowed scope")
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return set, e
	}
	defer done()
	v, e := s.verifyReadBindingV2(ctx, p, b)
	if e != nil {
		return set, e
	}
	p2, ok := p.(ProviderV2)
	if !ok {
		return set, errors.New("native provider lacks V2 labels capability")
	}
	for i, t := range labelTeamsV2(v, b, team) {
		found, calls, e := labelsNamedV2(ctx, p2, t, name)
		set.Calls += calls
		if e != nil {
			return LabelSetV2{Calls: set.Calls}, e
		}
		if len(found) > 1 {
			return LabelSetV2{Calls: set.Calls}, ErrLabelAmbiguous
		}
		for _, l := range found {
			if at := slices.Index(set.IDs, l.ID); at >= 0 {
				set.Scopes[at] = "workspace"
			} else {
				set.IDs = append(set.IDs, l.ID)
				set.Scopes = append(set.Scopes, map[bool]string{true: "team", false: "parent"}[i == 0])
			}
			if set.Apply == "" && eligibleLabelV2(l) == nil {
				set.Apply = l.ID
			}
		}
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return LabelSetV2{Calls: set.Calls}, e
	}
	return set, nil
}

// labelsNamedV2 walks a team's complete label listing for the labels named
// name that are the team's own or the workspace's, not archived, not groups.
func labelsNamedV2(ctx context.Context, p ProviderV2, team, name string) ([]linearprovider.Label, int, error) {
	var found []linearprovider.Label
	cursor, calls := "", 0
	for page := 0; page < labelSetPages; page++ {
		calls++
		rows, e := p.Labels(ctx, team, linearprovider.PageOptions{Limit: 100, Cursor: cursor})
		if e != nil {
			return nil, calls, e
		}
		if e = validProviderPageV2(rows.NextCursor, rows.Complete, cursor, len(rows.Items), 100); e != nil {
			return nil, calls, e
		}
		for _, l := range rows.Items {
			if l.Name == name && !l.Archived && !l.Group && checkLabelV2(l, team) == nil {
				found = append(found, l)
			}
		}
		if rows.Complete {
			return found, calls, nil
		}
		cursor = rows.NextCursor
	}
	return nil, calls, ErrLabelWalk
}

// findTeamLabelsV2 resolves label IDs for an issue of team: each from the
// team's own listing, else from its parent's (labelTeamsV2). A label found
// in neither is missing from the result.
func findTeamLabelsV2(ctx context.Context, p ProviderV2, teams []string, ids []string) (map[string]linearprovider.Label, error) {
	found := map[string]linearprovider.Label{}
	missing := append([]string(nil), ids...)
	for _, t := range teams {
		if len(missing) == 0 {
			break
		}
		got, e := findLabelsV2(ctx, p, t, missing)
		if e != nil {
			return nil, e
		}
		var still []string
		for _, id := range missing {
			if l, ok := got[id]; ok {
				found[id] = l
			} else {
				still = append(still, id)
			}
		}
		missing = still
	}
	return found, nil
}
