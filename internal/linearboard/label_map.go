package linearboard

import (
	"context"
	"errors"
	"sort"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// labelMapPages bounds one team's listing: up to 1,000 labels, read 100 at a
// time. A team with more says so with complete false.
const labelMapPages = 10

// TeamLabelsV2 is one bound team's labels by name. Each team has its own: a
// label with the same name in two teams has two IDs, so teams are never
// merged.
type TeamLabelsV2 struct {
	TeamID  string `json:"team_id"`
	Default bool   `json:"default,omitempty"`
	// Labels maps each name to its label's ID, for the labels an issue can
	// carry: group and archived labels are left out.
	Labels map[string]string `json:"labels"`
	// Duplicates are names used by more than one of the team's labels (in
	// different groups, say), with every ID; they aren't in Labels.
	Duplicates map[string][]string `json:"duplicates,omitempty"`
	Complete   bool                `json:"complete"`
}

// LabelMapV2 lists every bound team's labels by name in one call, in the
// binding's team order.
func (s *Service) LabelMapV2(ctx context.Context, scope bus.Request) ([]TeamLabelsV2, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	if _, e = s.verifyReadBindingV2(ctx, p, b); e != nil {
		return nil, e
	}
	p2, ok := p.(ProviderV2)
	if !ok {
		return nil, errors.New("native provider lacks V2 labels capability")
	}
	out := []TeamLabelsV2{}
	for _, team := range b.LinearTeamIDs {
		t, e := teamLabelsV2(ctx, p2, team)
		if e != nil {
			return nil, e
		}
		t.Default = team == b.LinearDefaultTeamID
		out = append(out, t)
	}
	if e = s.unchangedV2(ctx, scope, b); e != nil {
		return nil, e
	}
	return out, nil
}

// teamLabelsV2 pages through one team's labels with the same identity and
// scope checks as LabelsV2.
func teamLabelsV2(ctx context.Context, p ProviderV2, team string) (TeamLabelsV2, error) {
	t := TeamLabelsV2{TeamID: team, Labels: map[string]string{}}
	ids := map[string][]string{}
	seen := map[string]string{}
	cursor := ""
	for page := 0; page < labelMapPages; page++ {
		got, e := p.Labels(ctx, team, linearprovider.PageOptions{Limit: 100, Cursor: cursor})
		if e != nil {
			return t, e
		}
		if e = validProviderPageV2(got.NextCursor, got.Complete, cursor, len(got.Items), 100); e != nil {
			return t, e
		}
		for _, label := range got.Items {
			if !linearprovider.ValidID(label.ID) {
				return t, errors.New("label identity is invalid")
			}
			h := digest(label)
			if old, ok := seen[label.ID]; ok {
				if old != h {
					return t, errors.New("contradictory label identity")
				}
				continue
			}
			seen[label.ID] = h
			if checkLabelV2(label, team) != nil || label.Group || label.Archived {
				continue
			}
			ids[label.Name] = append(ids[label.Name], label.ID)
		}
		if got.Complete {
			t.Complete = true
			break
		}
		cursor = got.NextCursor
	}
	for name, list := range ids {
		if len(list) == 1 {
			t.Labels[name] = list[0]
			continue
		}
		if t.Duplicates == nil {
			t.Duplicates = map[string][]string{}
		}
		sort.Strings(list)
		t.Duplicates[name] = list
	}
	return t, nil
}
