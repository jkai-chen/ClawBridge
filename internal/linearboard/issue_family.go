package linearboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// A family move (live/ISSUE-TEAM-MOVE-DESIGN.md r5, r6): Linear moves a
// parent's sub-issues with it, so a family is one write on the parent and
// never a write on a child. The preview proves every child can follow as a
// single move would (the same team and project as the parent, no children
// of its own, its state and labels mapped exactly); the confirm reads each
// child back. A child that didn't follow, or followed otherwise, makes the
// family uncertain: nothing is retried and no child is written.

// maxFamilyChildren bounds a family (r6 O8).
const maxFamilyChildren = 50

// MoveChildV2 is one child's plan in a family move.
type MoveChildV2 struct {
	IssueID    string        `json:"issue_id"`
	Identifier string        `json:"identifier"`
	FromState  string        `json:"from_state"`
	ToState    string        `json:"to_state"`
	ToStateID  string        `json:"to_state_id"`
	Labels     []MoveLabelV2 `json:"labels"`
	labelIDs   []string
	names      []string
	current    linearprovider.IssueV2
}

// MoveChildResultV2 is one child's outcome in a confirmed family move.
type MoveChildResultV2 struct {
	IssueID       string   `json:"issue_id"`
	OldIdentifier string   `json:"old_identifier"`
	NewIdentifier string   `json:"new_identifier,omitempty"`
	Outcome       string   `json:"outcome"`
	Unverified    []string `json:"unverified,omitempty"`
}

// statusCache reads each team's states once in a family's plan.
type statusCache struct {
	Provider
	got map[string][]linearprovider.Status
}

func (c *statusCache) Statuses(ctx context.Context, team string) ([]linearprovider.Status, error) {
	if s, ok := c.got[team]; ok {
		return s, nil
	}
	s, e := c.Provider.Statuses(ctx, team)
	if e == nil {
		c.got[team] = s
	}
	return s, e
}

// labelCache reads each team's label pages once in a family's plan.
type labelCache struct {
	ProviderV2
	got map[string]linearprovider.Page[linearprovider.Label]
}

func (c *labelCache) Labels(ctx context.Context, team string, o linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	key := team + "\x00" + o.Cursor
	if page, ok := c.got[key]; ok {
		return page, nil
	}
	page, e := c.ProviderV2.Labels(ctx, team, o)
	if e == nil {
		c.got[key] = page
	}
	return page, e
}

// planFamilyV2 lists the parent's children to the end and plans each one.
// Everything that stops a child is a refusal of the whole family, named by
// the child's identifier (r6 O4–O8).
func (s *Service) planFamilyV2(ctx context.Context, p Provider, p2 ProviderV2, mover moverV2, b bus.TaskboardBindingV2, v verifiedV2, plan *MovePlanV2) error {
	parent := plan.current
	refuse := func(child linearprovider.IssueV2, format string, a ...any) {
		plan.Refusals = append(plan.Refusals, child.Identifier+": "+fmt.Sprintf(format, a...))
	}
	children, e := mover.Children(ctx, parent.ID, maxFamilyChildren)
	plan.Calls++
	if e != nil {
		return fmt.Errorf("the parent's sub-issues couldn't be listed to the end (a family has at most %d); nothing was planned: %w", maxFamilyChildren, e)
	}
	cachedP := &statusCache{Provider: p, got: map[string][]linearprovider.Status{}}
	cachedP2 := &labelCache{ProviderV2: p2, got: map[string]linearprovider.Page[linearprovider.Label]{}}
	slices.SortFunc(children, func(x, y linearprovider.IssueV2) int { return strings.Compare(x.ID, y.ID) })
	for _, child := range children {
		if !linearprovider.ValidID(child.ID) || child.ID == parent.ID {
			return fmt.Errorf("the parent's sub-issue listing named an invalid issue; nothing was planned")
		}
		// G2: Linear names the parent by its identifier, or its UUID.
		if !(child.ParentID == parent.ID || (child.ParentID == "" && child.ParentIdentifier != "" && child.ParentIdentifier == parent.Identifier)) {
			return fmt.Errorf("the parent's sub-issue listing named %s, whose parent isn't this issue; nothing was planned", child.Identifier)
		}
		switch {
		case child.TeamID == plan.ToTeam:
			refuse(child, "already in the target team; the family is split already")
			continue
		case child.TeamID != plan.FromTeam:
			refuse(child, "in team %s, not the parent's; Linear would move it into a team this binding may not show", child.TeamID)
			continue
		}
		if !child.ProjectKnown || child.ProjectID != parent.ProjectID {
			refuse(child, "in another project than its parent")
			continue
		}
		grand, e := mover.SubIssues(ctx, child.ID)
		plan.Calls++
		if e != nil {
			return e
		}
		if grand > 0 {
			refuse(child, "has sub-issues of its own; a family of more than two levels isn't moved")
			continue
		}
		cp := MovePlanV2{IssueID: child.ID, FromTeam: plan.FromTeam, ToTeam: plan.ToTeam, Labels: []MoveLabelV2{}}
		if e = planMoveStateV2(ctx, cachedP, &cp, child); e != nil {
			return e
		}
		if e = planMoveLabelsV2(ctx, cachedP2, v, b, &cp, child); e != nil {
			return e
		}
		for _, r := range cp.Refusals {
			refuse(child, "%s", r)
		}
		plan.Children = append(plan.Children, MoveChildV2{IssueID: child.ID, Identifier: child.Identifier, FromState: cp.FromState, ToState: cp.ToState, ToStateID: cp.ToStateID, Labels: cp.Labels, labelIDs: cp.labelIDs, names: cp.names, current: child})
	}
	plan.Calls += len(cachedP.got) + len(cachedP2.got)
	if len(children) == 0 {
		plan.Refusals = append(plan.Refusals, "Linear lists no sub-issues under the issue now; move it without --family")
	}
	return nil
}

// childDigest is one child's part of its family's plan digest (P3): IDs
// and its updatedAt, never names.
func childDigest(c MoveChildV2) string {
	x := c.current
	raw, _ := json.Marshal([]any{c.IssueID, x.UpdatedAt, x.StateID, c.ToStateID, c.labelIDs, len(c.names), x.ProjectID, x.AssigneeID, x.CycleSet})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// familyReadBackV2 reads every child after the parent moved: each must be
// in the target team under a new identifier, still the parent's sub-issue
// (by its new identifier or its UUID), with its planned state and labels
// and its assignee, project and priority unchanged; and the parent must
// still list exactly these children.
func familyReadBackV2(ctx context.Context, p2 ProviderV2, mover moverV2, plan MovePlanV2, parent linearprovider.IssueV2) ([]MoveChildResultV2, bool) {
	var out []MoveChildResultV2
	whole := true
	listed, e := mover.Children(ctx, plan.IssueID, maxFamilyChildren)
	if e != nil || len(listed) != len(plan.Children) {
		whole = false
	}
	for _, c := range plan.Children {
		r := MoveChildResultV2{IssueID: c.IssueID, OldIdentifier: c.Identifier, Outcome: "accepted"}
		back, e := p2.IssueV2(ctx, c.IssueID)
		if e != nil {
			r.Outcome, r.Unverified = "uncertain", []string{"read-back"}
			out = append(out, r)
			continue
		}
		r.NewIdentifier = back.Identifier
		cp := MovePlanV2{IssueID: c.IssueID, Identifier: c.Identifier, ToTeam: plan.ToTeam, ToState: c.ToState, ToStateID: c.ToStateID, labelIDs: c.labelIDs, names: c.names, current: c.current}
		r.Unverified = moveReadBackV2(cp, linearprovider.IssueV2{}, back)
		// The parent link changes form: the parent's new identifier, or
		// its UUID; never compared across forms.
		r.Unverified = slices.DeleteFunc(r.Unverified, func(f string) bool { return f == "parent" })
		if !(back.ParentID == plan.IssueID || (back.ParentID == "" && back.ParentIdentifier != "" && back.ParentIdentifier == parent.Identifier)) {
			r.Unverified = append(r.Unverified, "parent")
		}
		if !whole {
			r.Unverified = append(r.Unverified, "family")
		}
		if len(r.Unverified) > 0 {
			r.Outcome = "uncertain"
		}
		out = append(out, r)
	}
	return out, whole
}
