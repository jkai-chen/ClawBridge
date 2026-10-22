package linearboard

import (
	"context"
	"errors"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// refreshWriteProofV2 runs under the operation lock. These are observations,
// not a provider CAS: a visible intervening scope/eligibility change is refused.
func refreshWriteProofV2(ctx context.Context, p Provider, p2 ProviderV2, b bus.TaskboardBindingV2, in WriteInputV2, team, project string, original linearprovider.IssueV2, legacy bool) error {
	v, e := verifyBindingV2(ctx, p, b)
	if e != nil {
		return e
	}
	if in.Operation != "create" {
		var current linearprovider.IssueV2
		if legacy {
			issue, err := p.GetIssue(ctx, in.IssueID)
			if err != nil {
				return err
			}
			current = linearprovider.IssueV2{Issue: issue, ProjectKnown: true}
		} else {
			current, e = p2.IssueV2(ctx, in.IssueID)
			if e != nil {
				return e
			}
		}
		if current.ID != in.IssueID || current.TeamID != original.TeamID || current.ProjectID != original.ProjectID {
			return errors.New("source issue context changed before dispatch")
		}
		if e = checkIssueV2(current, b, v); e != nil {
			return e
		}
		if in.Labels.Present {
			// The fresh in-lock read must equal the set the caller last read,
			// not merely the set this call read moments earlier.
			if !current.LabelsKnown {
				return errors.New("canonical labels became unavailable before replacement")
			}
			newIDs := current.LabelIDs
			if newIDs == nil {
				newIDs = []string{}
			}
			newIDs, e = sortedUniqueIDs(newIDs, 100)
			if e != nil {
				return e
			}
			expected, e := sortedUniqueIDs(in.ExpectedLabels.Value, 100)
			if e != nil {
				return e
			}
			if digest(expected) != digest(newIDs) {
				return errors.New("issue labels differ from expected_label_ids before replacement; inspect current labels")
			}
		}
		if in.Parent.Present && !in.Project.Present && !current.ProjectKnown {
			return errors.New("effective destination is unavailable before parent change")
		}
	}
	if e = checkIssueV2(linearprovider.IssueV2{Issue: linearprovider.Issue{ID: "00000000-0000-0000-0000-000000000000", TeamID: team, ProjectID: project}, ProjectKnown: true}, b, v); e != nil {
		return e
	}
	if in.Labels.Present {
		found, e := findLabelsV2(ctx, p2, team, in.Labels.Value)
		if e != nil {
			return e
		}
		if len(found) != len(in.Labels.Value) {
			return errors.New("selected label no longer belongs to the target team")
		}
	}
	if in.Assignee.Present && in.Assignee.Value != nil {
		if e = findMemberV2(ctx, p2, team, *in.Assignee.Value); e != nil {
			return e
		}
	}
	if in.Parent.Present && in.Parent.Value != nil {
		parent, e := verifyParentV2(ctx, p2, b, v, in.IssueID, *in.Parent.Value)
		if e != nil {
			return e
		}
		if e = sameDestinationParentV2(parent, team, project); e != nil {
			return e
		}
	}
	if in.StateID != nil {
		states, e := p.Statuses(ctx, team)
		if e != nil {
			return e
		}
		found := false
		for _, state := range states {
			if state.ID == *in.StateID && state.TeamID == team {
				found = true
			}
		}
		if !found {
			return errors.New("selected state no longer belongs to the target team")
		}
	}
	return ctx.Err()
}

func relationEvidenceV2(ctx context.Context, p ProviderV2, b bus.TaskboardBindingV2, v verifiedV2, in RelationInputV2) (*linearprovider.Relation, bool, error) {
	from, e := p.IssueV2(ctx, in.FromID)
	if e != nil {
		return nil, false, e
	}
	if from.ID != in.FromID {
		return nil, false, errors.New("relation source identity changed")
	}
	if e = checkIssueV2(from, b, v); e != nil {
		return nil, false, e
	}
	to, e := p.IssueV2(ctx, in.ToID)
	if e != nil {
		return nil, false, e
	}
	if to.ID != in.ToID {
		return nil, false, errors.New("relation target identity changed")
	}
	if e = checkIssueV2(to, b, v); e != nil {
		return nil, false, e
	}
	var existing *linearprovider.Relation
	for _, issue := range []linearprovider.IssueV2{from, to} {
		for _, edge := range issue.Relations {
			if (edge.Type != "blocks" && edge.Type != "related") || !linearprovider.ValidID(edge.FromID) || !linearprovider.ValidID(edge.ToID) || edge.FromID == edge.ToID || (edge.ID != "" && !linearprovider.ValidID(edge.ID)) {
				return nil, false, errors.New("invalid authoritative relation evidence")
			}
			if edge.Type == "related" && edge.FromID > edge.ToID {
				edge.FromID, edge.ToID = edge.ToID, edge.FromID
			}
			if edge.Type == in.Type && edge.FromID == in.FromID && edge.ToID == in.ToID {
				if existing != nil && existing.ID != "" && edge.ID != "" && existing.ID != edge.ID {
					return nil, false, errors.New("ambiguous existing relation identity")
				}
				copy := edge
				existing = &copy
			}
		}
	}
	observed := (in.Action == "add" && existing != nil) || (in.Action == "remove" && existing == nil && from.RelationsKnown && to.RelationsKnown)
	if !observed {
		caps, e := p.Capabilities(ctx)
		if e != nil {
			return nil, false, e
		}
		if (in.Action == "add" && !caps.RelationAdd) || (in.Action == "remove" && !caps.RelationRemove) {
			return nil, false, errors.New("provider lacks a genuine one-edge operation; relation-list replacement is forbidden")
		}
	}
	return existing, observed, ctx.Err()
}
