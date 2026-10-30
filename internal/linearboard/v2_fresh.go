package linearboard

import (
	"context"
	"errors"
	"reflect"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// refreshWriteProofV2 runs under the operation lock. These are observations,
// not a provider CAS: a visible intervening scope/eligibility change is refused.
// refreshWriteProofV2 re-reads, inside the operation lock, every Linear fact
// the write depends on: the issue, its labels, the assignee, the parent and
// the state (a create's team lists excepted, see below). It reuses the same
// command's binding verification v (team and project structure, read
// moments earlier at preflight, or a kept team proof with the pair of the
// target checked at preflight); the ClawBridge binding revision itself is
// re-checked around this call by unchangedV2. The re-read issue must still
// be in the team and project the preflight checked, so a move between the
// two reads stops the write.
func (s *Service) refreshWriteProofV2(ctx context.Context, p Provider, p2 ProviderV2, b bus.TaskboardBindingV2, v verifiedV2, in WriteInputV2, team, project string, original linearprovider.IssueV2, legacy bool, plan labelPlanV2) error {
	var e error
	if in.Operation != "create" {
		var current linearprovider.IssueV2
		if legacy {
			issue, err := p.GetIssue(ctx, in.IssueID)
			if err != nil {
				return err
			}
			current = linearprovider.IssueV2{Issue: issue, ProjectKnown: true}
		} else {
			// The in-lock re-read is never served from a held operation's
			// cache: an issue moved out of the binding meanwhile must stop
			// the write.
			current, e = s.rereadIssue(ctx, p2, in.IssueID)
			if e != nil {
				return e
			}
		}
		if current.ID != in.IssueID || current.TeamID != original.TeamID || current.ProjectID != original.ProjectID {
			return errors.New("source issue context changed before dispatch")
		}
		if e = writeScopeV2(in)(current, b, v); e != nil {
			return e
		}
		if e = textGuardV2(in, current); e != nil {
			return e
		}
		if in.AddLabels != nil || in.RemoveLabels != nil {
			fresh, e := writePlanV2(ctx, s.planProvider(p2), current, in)
			if e != nil {
				return e
			}
			if !reflect.DeepEqual(fresh, plan) {
				return errors.New("issue labels changed before dispatch; re-read and retry with a new operation_id")
			}
		}
		if in.Labels.Present {
			// The fresh in-lock read must equal the set the caller last read,
			// not merely the set this call read moments earlier.
			if current, e = resolveLabelsV2(ctx, p2, current); e != nil {
				return e
			}
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
		if in.Parent.Present && in.Operation != "comment" && !in.Project.Present && !current.ProjectKnown {
			return errors.New("effective destination is unavailable before parent change")
		}
	}
	if e = writeScopeV2(in)(linearprovider.IssueV2{Issue: linearprovider.Issue{ID: "00000000-0000-0000-0000-000000000000", TeamID: team, ProjectID: project}, ProjectKnown: true}, b, v); e != nil {
		return e
	}
	// A create's team lists (its labels and workflow states) were read
	// moments ago, before the ledger write, for the same team the create
	// names, and are not read again here. Linear validates state and label
	// IDs on save, so a list gone stale meanwhile fails the write and can't
	// leak scope. Every other operation's target team comes from the issue
	// re-read above, so its lists are read again.
	reread := in.Operation != "create"
	for i, labels := range [][]string{in.Labels.Value, in.AddLabels} {
		// A marker's add was proven eligible by name in this held
		// operation moments ago (markerLabel).
		if !reread || len(labels) == 0 || i == 1 && in.marker {
			continue
		}
		found, e := findTeamLabelsV2(ctx, p2, labelTeamsV2(v, b, team), labels)
		if e != nil {
			return e
		}
		if len(found) != len(labels) {
			return errors.New("selected label no longer belongs to the target team")
		}
	}
	if in.Assignee.Present && in.Assignee.Value != nil {
		if e = findMemberV2(ctx, p2, team, *in.Assignee.Value); e != nil {
			return e
		}
	}
	// A reply's parent is a comment, proven once before the ledger write: a
	// comment never moves to another issue.
	if in.Parent.Present && in.Parent.Value != nil && in.Operation != "comment" {
		parent, e := verifyParentV2(ctx, p2, b, v, in.IssueID, *in.Parent.Value)
		if e != nil {
			return e
		}
		if e = sameDestinationParentV2(parent, team, project); e != nil {
			return e
		}
	}
	if in.StateID != nil && reread {
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

// commandCapsV2 derives a provider's capabilities at most once per command:
// the connection and its tool list do not change while the command runs, and
// each derivation re-reads every tool schema.
type commandCapsV2 struct {
	p    ProviderV2
	caps *linearprovider.Capabilities
}

func (c *commandCapsV2) get(ctx context.Context) (linearprovider.Capabilities, error) {
	if c.caps == nil {
		caps, e := c.p.Capabilities(ctx)
		if e != nil {
			return caps, e
		}
		c.caps = &caps
	}
	return *c.caps, nil
}

// relationEvidenceV2 reads both ends and their edges. lane, when the
// binding has lanes, checks a team against the session's lane (X2): an add
// needs the caller's own end in it; a remove needs the stored edge's from
// end, as Linear reports it, and both ends of a related edge whose stored
// direction isn't seen.
func relationEvidenceV2(ctx context.Context, p ProviderV2, b bus.TaskboardBindingV2, v verifiedV2, caps *commandCapsV2, in RelationInputV2, lane func(team string) error) (*linearprovider.Relation, bool, error) {
	from, e := p.IssueV2(ctx, in.FromID)
	if e != nil {
		return nil, false, e
	}
	if from.ID != in.FromID {
		return nil, false, errors.New("relation source identity changed")
	}
	// Both ends are the relation's targets, checked on every read: on a
	// kept team proof, each end's own team and project must be the
	// binding's (admitIssueV2).
	v = admitIssueV2(v, b, from)
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
	v = admitIssueV2(v, b, to)
	if e = checkIssueV2(to, b, v); e != nil {
		return nil, false, e
	}
	// Native edges may name an endpoint only by display identifier; resolve it
	// against the two issues read here. An edge to any other issue is
	// irrelevant to this request.
	resolve := func(id, identifier string) (string, bool) {
		switch {
		case id != "":
			return id, linearprovider.ValidID(id)
		case identifier != "" && identifier == from.Identifier:
			return from.ID, true
		case identifier != "" && identifier == to.Identifier:
			return to.ID, true
		}
		return "", true
	}
	var existing *linearprovider.Relation
	storedFrom := ""
	for _, issue := range []linearprovider.IssueV2{from, to} {
		for _, edge := range issue.Relations {
			fromID, okFrom := resolve(edge.FromID, edge.FromIdentifier)
			toID, okTo := resolve(edge.ToID, edge.ToIdentifier)
			if !okFrom || !okTo {
				return nil, false, errors.New("invalid authoritative relation evidence")
			}
			if fromID == "" || toID == "" {
				continue
			}
			edge.FromID, edge.ToID, edge.FromIdentifier, edge.ToIdentifier = fromID, toID, "", ""
			reported := edge.FromID
			if (edge.Type != "blocks" && edge.Type != "related") || edge.FromID == edge.ToID || (edge.ID != "" && !linearprovider.ValidID(edge.ID)) {
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
				storedFrom = reported
			}
		}
	}
	observed := (in.Action == "add" && existing != nil) || (in.Action == "remove" && existing == nil && from.RelationsKnown && to.RelationsKnown)
	if lane != nil && !observed {
		teamOf := map[string]string{from.ID: from.TeamID, to.ID: to.TeamID}
		var ends []string
		switch {
		case in.Action == "add":
			ends = []string{in.source}
		case storedFrom != "":
			ends = []string{storedFrom}
		case in.Type == "related":
			ends = []string{from.ID, to.ID}
		default:
			ends = []string{in.FromID}
		}
		for _, id := range ends {
			if e = lane(teamOf[id]); e != nil {
				return nil, false, e
			}
		}
	}
	if !observed {
		c, e := caps.get(ctx)
		if e != nil {
			return nil, false, e
		}
		if (in.Action == "add" && !c.RelationAdd) || (in.Action == "remove" && !c.RelationRemove) {
			return nil, false, errors.New("provider lacks a genuine one-edge operation; relation-list replacement is forbidden")
		}
	}
	return existing, observed, ctx.Err()
}
