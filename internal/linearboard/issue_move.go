package linearboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// An issue's move to another bound team (FR7, live/ISSUE-TEAM-MOVE-DESIGN.md
// r3). The preview plans it from Linear's reads and sends nothing. The
// confirm re-plans, refuses a plan that changed since the preview, has the
// server reserve it (the server decides who may confirm, and refuses until
// its operator proves moves), sends one save_issue through the person's own
// Linear profile, reads the issue back by UUID and settles the reservation.
// Nothing is retried.

// errAlreadyInTeam: the issue is in the target team already.
var errAlreadyInTeam = errors.New("the issue is already in that team")

// moverV2 is a provider that can express and prove a team move.
type moverV2 interface {
	ValidateMoveV2(linearprovider.MoveInputV2) error
	MoveV2(context.Context, linearprovider.MoveInputV2) (linearprovider.IssueV2, error)
	SubIssues(context.Context, string) (int, error)
	Children(context.Context, string, int) ([]linearprovider.IssueV2, error)
}

// MoveInputV2 is a move request: the issue and target team by UUID, and on
// a confirm the operation ID and the preview's plan digest.
type MoveInputV2 struct {
	IssueID, TeamID, OperationID string
	Confirm                      bool
	ExpectedPlanSHA256           string
	// Family moves a parent with all its sub-issues: one write on the
	// parent, whose children Linear moves with it (r5).
	Family bool
}

// MoveLabelV2 is one label of the issue and the target team's label of the
// same name; ToID is empty when the target set has none.
type MoveLabelV2 struct {
	Name  string `json:"name"`
	ToID  string `json:"to_id,omitempty"`
	Scope string `json:"scope,omitempty"`
}

// MovePlanV2 is what a move would do. Refusals list everything that stops
// it; a plan with any is never confirmed.
type MovePlanV2 struct {
	IssueID    string        `json:"issue_id"`
	Identifier string        `json:"identifier"`
	FromTeam   string        `json:"from_team_id"`
	ToTeam     string        `json:"to_team_id"`
	FromState  string        `json:"from_state"`
	ToState    string        `json:"to_state"`
	ToStateID  string        `json:"to_state_id"`
	StateBy    string        `json:"state_by,omitempty"`
	Labels     []MoveLabelV2 `json:"labels"`
	Cycle      string        `json:"cycle"`
	Estimate   string        `json:"estimate"`
	ProjectID  string        `json:"project_id,omitempty"`
	Refusals   []string      `json:"refusals,omitempty"`
	PlanSHA256 string        `json:"plan_sha256"`
	// Children is a family move's sub-issues, each planned as a single
	// move would be.
	Children []MoveChildV2 `json:"children,omitempty"`
	// SubIssues is how many sub-issues Linear listed (at least: one
	// page). A family's answer leaves it out: Children lists them all.
	SubIssues int `json:"sub_issues,omitempty"`
	Calls     int `json:"calls"`
	// what the digest and the read-back compare; never shown.
	updatedAt string
	current   linearprovider.IssueV2
	labelIDs  []string
	names     []string
}

// MoveResultV2 is a preview or a confirmed move's outcome: preview,
// accepted, or uncertain with the fields the read-back couldn't prove.
type MoveResultV2 struct {
	Plan          MovePlanV2 `json:"plan"`
	Outcome       string     `json:"outcome"`
	NewIdentifier string     `json:"new_identifier,omitempty"`
	Unverified    []string   `json:"unverified,omitempty"`
	By            string     `json:"by,omitempty"`
	Lane          string     `json:"lane,omitempty"`
	Note          string     `json:"note,omitempty"`
	// Children is a family's outcome per child.
	Children []MoveChildResultV2 `json:"children,omitempty"`
}

// MoveIssueV2 previews a move, or confirms the one previewed.
func (s *Service) MoveIssueV2(ctx context.Context, scope bus.Request, in MoveInputV2) (MoveResultV2, error) {
	var out MoveResultV2
	if !linearprovider.ValidID(in.IssueID) || !linearprovider.ValidID(in.TeamID) {
		return out, errors.New("a move takes the issue's UUID and the target team's UUID")
	}
	in.IssueID, in.TeamID = strings.ToLower(in.IssueID), strings.ToLower(in.TeamID)
	if in.Confirm && (in.OperationID == "" || in.ExpectedPlanSHA256 == "") {
		return out, errors.New("a confirm takes --operation-id and the preview's --expected-plan-sha256; nothing was sent")
	}
	if !in.Confirm && in.ExpectedPlanSHA256 != "" {
		return out, errors.New("--expected-plan-sha256 goes with --confirm")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return out, e
	}
	if !b.Configured {
		return out, errors.New("task-board is not linked")
	}
	access, e := s.writeAccessV2(ctx, scope, b)
	if e != nil {
		return out, e
	}
	open := s.open
	if in.Confirm {
		open = s.openForWrite
	}
	profile, p, done, e := open(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	p2, ok := p.(ProviderV2)
	mover, canMove := p.(moverV2)
	if !ok || !canMove {
		return out, errors.New("this Linear connection can't express a team move")
	}
	v, e := s.proveBinding(ctx, p, b)
	if e != nil {
		return out, e
	}
	plan, e := s.planMoveV2(ctx, p, p2, mover, b, v, in)
	if e != nil {
		return out, e
	}
	out.Plan = plan
	// A family's children are listed in full; Linear's sub-issue count is
	// one page at most, so it would seem to disagree with them.
	if len(out.Plan.Children) > 0 {
		out.Plan.SubIssues = 0
	}
	if !in.Confirm {
		out.Outcome = "preview"
		out.Note = moveWho(b, access)
		return out, s.unchangedV2(ctx, scope, b)
	}
	if len(plan.Refusals) > 0 {
		return out, fmt.Errorf("the move is refused: %s; nothing was sent", strings.Join(plan.Refusals, "; "))
	}
	if plan.PlanSHA256 != in.ExpectedPlanSHA256 {
		return out, errors.New("the plan changed since the preview (the issue, its labels or state, or a team's labels or states); preview again; nothing was sent")
	}
	if profile.Access != "write" {
		return out, errors.New("personal Linear profile is read-only; reconnect explicitly with write access")
	}
	// The server decides who confirms; this only spares a call it would
	// refuse anyway.
	if access.SessionID != "" {
		if !access.Coordinator {
			return out, errors.New("only the project's coordinator session confirms a team move; preview it and ask the project owner or a project lead; nothing was sent")
		}
		if len(b.Lanes) == 0 {
			return out, outsideLaneError("outside_lane: this binding has no lanes, so a coordinator confirms no team move; nothing was sent")
		}
		for _, team := range []string{plan.FromTeam, plan.ToTeam} {
			if e = checkLaneV2(b, access, team); e != nil {
				return out, e
			}
		}
	}
	send := linearprovider.MoveInputV2{ID: plan.IssueID, TeamID: plan.ToTeam, StateID: plan.ToStateID, Labels: plan.labelIDs, ClearCycle: true}
	if e = mover.ValidateMoveV2(send); e != nil {
		return out, fmt.Errorf("this Linear connection can't express the move; nothing was sent: %w", e)
	}
	reserve := map[string]any{"operation_id": in.OperationID, "issue_uuid": plan.IssueID, "from_team": plan.FromTeam, "to_team": plan.ToTeam, "plan_sha256": plan.PlanSHA256, "calls": moveConfirmCalls(plan)}
	if in.Family {
		var kids []any
		for _, c := range plan.Children {
			kids = append(kids, c.IssueID)
		}
		reserve["children"] = kids
	}
	reserved, e := s.moveCall(ctx, scope, "issue_move_reserve", reserve)
	if e != nil {
		return out, fmt.Errorf("the move could not be reserved; nothing was sent: %w", e)
	}
	out.By, out.Lane = str(reserved, "by"), str(reserved, "lane")
	if reserved["replay"] == true {
		state := str(reserved, "state")
		out.Outcome, out.NewIdentifier = state, str(reserved, "new_identifier")
		if state == "reserved" {
			out.Outcome = "uncertain"
			out.Note = "an earlier confirm with this operation ID may have sent the move and never settled; read the issue in Linear; nothing was sent again"
		}
		return out, nil
	}
	settle := func(outcome, identifier string) error {
		args := map[string]any{"operation_id": in.OperationID, "outcome": outcome, "old_identifier": plan.Identifier, "new_identifier": identifier}
		if in.Family && outcome != "not_sent" {
			if out.Children == nil {
				// Nothing was read back: every child is uncertain.
				for _, c := range plan.Children {
					out.Children = append(out.Children, MoveChildResultV2{IssueID: c.IssueID, OldIdentifier: c.Identifier, Outcome: "uncertain", Unverified: []string{"read-back"}})
				}
			}
			var kids []any
			for _, c := range out.Children {
				kids = append(kids, map[string]any{"issue_uuid": c.IssueID, "outcome": c.Outcome, "old_identifier": c.OldIdentifier, "new_identifier": c.NewIdentifier})
			}
			args["children"] = kids
		}
		_, err := s.moveCall(ctx, scope, "issue_move_settle", args)
		return err
	}
	// The lane and the binding hold right up to the send.
	fresh, e := s.writeAccessV2(ctx, scope, b)
	if e == nil && (fresh.SessionID != access.SessionID || !sameLaneV2(b, access, fresh)) {
		e = errLaneChanged
	}
	if e == nil {
		e = s.unchangedV2(ctx, scope, b)
	}
	if e == nil {
		e = ctx.Err()
	}
	if e != nil {
		return out, errors.Join(fmt.Errorf("nothing was sent: %w", e), settle("not_sent", ""))
	}
	moved, sendErr := mover.MoveV2(ctx, send)
	s.wrote(plan.IssueID)
	// The read-back comes before the settle, whatever Linear answered, so
	// the settle states what the read shows: an answer that failed to
	// parse may still have moved the issue. Only a definite refusal with
	// the issue still in its team is not_sent (as FR10's F1).
	back, readErr := p2.IssueV2(ctx, plan.IssueID)
	switch {
	case readErr != nil:
		out.Outcome = "uncertain"
		out.Unverified = []string{"read-back"}
		out.Note = "the issue couldn't be read back after the move was sent; it may or may not have moved; read the issue in Linear; nothing is retried"
		return out, errors.Join(sendErr, readErr, settle("uncertain", ""))
	case sendErr != nil && definiteRefusal(sendErr) && back.TeamID == plan.FromTeam && (!in.Family || childrenStayed(ctx, p2, plan)):
		out.Outcome = "not_sent"
		out.Note = "Linear refused the move, and the issue is still in its team"
		if in.Family {
			out.Note = "Linear refused the move, and the parent and every child are still in their team"
		}
		return out, errors.Join(sendErr, settle("not_sent", ""))
	}
	out.Unverified = moveReadBackV2(plan, moved, back)
	out.NewIdentifier = back.Identifier
	out.Outcome = "accepted"
	if in.Family {
		var whole bool
		out.Children, whole = familyReadBackV2(ctx, p2, mover, plan, back)
		if !whole {
			out.Unverified = append(out.Unverified, "family")
		}
		for _, c := range out.Children {
			if c.Outcome != "accepted" && !slices.Contains(out.Unverified, "children") {
				out.Unverified = append(out.Unverified, "children")
			}
		}
	}
	if len(out.Unverified) > 0 {
		out.Outcome = "uncertain"
		switch {
		case in.Family:
			out.Note = fmt.Sprintf("the family is uncertain and nothing is retried; read the issues in Linear. To move it back: clawbridge linear move %s --team %s --family, previewed first (whether that pulls back a child that didn't follow is unproven)", plan.IssueID, plan.FromTeam)
		case sendErr != nil:
			out.Note = "Linear's answer to the move was an error, and the read-back doesn't show the move as planned; read the issue in Linear; nothing is retried"
		}
		if sendErr != nil {
			return out, errors.Join(sendErr, settle("uncertain", ""))
		}
	}
	if note := s.policyGapNote(ctx, scope, b, plan.ToTeam); note != "" {
		if out.Note != "" {
			out.Note += "; "
		}
		out.Note += note
	}
	if e = settle(out.Outcome, out.NewIdentifier); e != nil {
		return out, fmt.Errorf("the move was sent (%s) but not settled on the server: %w", out.Outcome, e)
	}
	return out, nil
}

// policyGapNote says that the destination team is bound but missing from
// an enabled status policy, so claims and done-releases there leave the
// issue's state alone. It adds no Linear call, and a policy that can't be
// read adds no note and never fails the move.
func (s *Service) policyGapNote(ctx context.Context, scope bus.Request, b bus.TaskboardBindingV2, team string) string {
	policy, e := s.statusPolicy(ctx, scope)
	if e != nil || !policy.Enabled || !contains(b.LinearTeamIDs, team) {
		return ""
	}
	if _, ok := policy.Teams[strings.ToLower(team)]; ok {
		return ""
	}
	return team + " is not in the status policy, so claims and done-releases there won't move its state: the project owner runs `clawbridge linear status-policy set` with the file {\"enabled\": true}"
}

// childrenStayed reads every child of a refused family back: not_sent
// holds only when each one is still in the source team.
func childrenStayed(ctx context.Context, p2 ProviderV2, plan MovePlanV2) bool {
	for _, c := range plan.Children {
		back, e := p2.IssueV2(ctx, c.IssueID)
		if e != nil || back.TeamID != plan.FromTeam {
			return false
		}
	}
	return true
}

// moveConfirmCalls is what a confirm spends against the project's hourly
// Linear budget (r6 O2): the plan's reads again, the write, its read-back,
// and for a family each child's read-back and the re-listing.
func moveConfirmCalls(plan MovePlanV2) int {
	n := plan.Calls + 2
	if len(plan.Children) > 0 {
		n += len(plan.Children) + 1
	}
	return n
}

// moveWho says, in a preview, who may confirm this move.
func moveWho(b bus.TaskboardBindingV2, access bus.TaskboardAccess) string {
	switch {
	case access.SessionID == "":
		return "Only the project owner or a project lead confirms a move, from their own terminal, once the server's operator has proven moves."
	case access.Coordinator && len(b.Lanes) > 0:
		return "As the project's coordinator you confirm a move between your lane's teams only, when your person or the owner asked for it."
	default:
		return "A session previews a move only; the project owner or a project lead confirms it."
	}
}

func (s *Service) moveCall(ctx context.Context, scope bus.Request, op string, args map[string]any) (map[string]any, error) {
	scope.Operation = op
	scope.Args = instanceArgs(scope.Args)
	for k, v := range args {
		scope.Args[k] = v
	}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, e
	}
	m, _ := r.Data.(map[string]any)
	if m == nil {
		return nil, errors.New("the server's answer was not a move record")
	}
	return m, nil
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

// planMoveV2 reads the issue, both teams' states, the labels' target set,
// the project's teams and the sub-issues, and plans the move by IDs. A
// parent or a sub-issue is refused: families move only in a batch (r4 Q4).
func (s *Service) planMoveV2(ctx context.Context, p Provider, p2 ProviderV2, mover moverV2, b bus.TaskboardBindingV2, v verifiedV2, in MoveInputV2) (MovePlanV2, error) {
	plan := MovePlanV2{IssueID: in.IssueID, ToTeam: in.TeamID, Labels: []MoveLabelV2{}}
	refuse := func(format string, a ...any) { plan.Refusals = append(plan.Refusals, fmt.Sprintf(format, a...)) }
	current, e := p2.IssueV2(ctx, in.IssueID)
	plan.Calls++
	if e != nil {
		return plan, e
	}
	if current.ID != in.IssueID {
		return plan, errors.New("issue identity changed")
	}
	if e = checkIssueV2(current, b, v); e != nil {
		return plan, e
	}
	plan.current, plan.updatedAt = current, current.UpdatedAt
	plan.Identifier, plan.FromTeam, plan.ProjectID = current.Identifier, current.TeamID, current.ProjectID
	if plan.FromTeam == plan.ToTeam {
		return plan, errAlreadyInTeam
	}
	for _, team := range []string{plan.FromTeam, plan.ToTeam} {
		if !contains(b.LinearTeamIDs, team) || b.TeamAccess(team) != bus.TeamAccessWrite {
			return plan, outOfScopeError(fmt.Sprintf("team %s isn't a write team of this binding; an issue moves only between bound write teams", team))
		}
	}
	// M2: Linear's handling of sub-issues on a move is unproven.
	children, e := mover.SubIssues(ctx, current.ID)
	plan.Calls++
	if e != nil {
		return plan, e
	}
	plan.SubIssues = children
	switch {
	case children > 0 && !in.Family:
		refuse("the issue has sub-issues (at least %d), which Linear moves with it; move the whole family with --family (family: true)", children)
	case children == 0 && in.Family:
		refuse("the issue has no sub-issues; move it without --family")
	}
	// Q4: no split family; a sub-issue moves only with its parent.
	switch {
	case !current.ParentKnown:
		refuse("Linear's read didn't say whether the issue has a parent, so a move can't keep its family whole")
	case current.ParentID != "" || current.ParentIdentifier != "":
		refuse("the issue is a sub-issue; it moves only with its family, named by its parent with --family")
	}
	// The project must include the target team.
	if current.ProjectID != "" {
		project, e := p.GetProject(ctx, current.ProjectID)
		plan.Calls++
		if e != nil {
			return plan, e
		}
		if project.ID != current.ProjectID {
			return plan, errors.New("project identity changed")
		}
		effective := linearprovider.IssueV2{Issue: linearprovider.Issue{ID: current.ID, TeamID: plan.ToTeam, ProjectID: current.ProjectID}, ProjectKnown: true}
		if !contains(project.TeamIDs, plan.ToTeam) || checkIssueV2(effective, b, v) != nil {
			refuse("the issue's project doesn't include team %s; add the team to the project first, or take the issue out of the project", plan.ToTeam)
		}
	} else if !current.ProjectKnown {
		return plan, errors.New("the issue's project is unknown; nothing was planned")
	}
	if e = planMoveStateV2(ctx, p, &plan, current); e != nil {
		return plan, e
	}
	if e = planMoveLabelsV2(ctx, p2, v, b, &plan, current); e != nil {
		return plan, e
	}
	// M3: a cycle belongs to its team, so it is always cleared; the
	// estimate is kept, and a read-back that differs is reported.
	switch {
	case current.CycleKnown && !current.CycleSet:
		plan.Cycle = "none"
	case current.CycleKnown:
		plan.Cycle = "cleared"
	default:
		plan.Cycle = "cleared, if it has one (Linear's read didn't say)"
	}
	switch {
	case current.Estimate != nil:
		plan.Estimate = fmt.Sprintf("kept (%g)", *current.Estimate)
	case current.EstimateKnown:
		plan.Estimate = "none"
	default:
		plan.Estimate = "unchanged by ClawBridge (Linear's read didn't say)"
	}
	if in.Family && children > 0 {
		if e = s.planFamilyV2(ctx, p, p2, mover, b, v, &plan); e != nil {
			return plan, e
		}
	}
	plan.PlanSHA256 = movePlanDigest(plan)
	return plan, nil
}

// planMoveStateV2 maps the issue's state to the target team's: the state
// of the same name, else the one state of the same type.
func planMoveStateV2(ctx context.Context, p Provider, plan *MovePlanV2, current linearprovider.IssueV2) error {
	from, e := p.Statuses(ctx, plan.FromTeam)
	plan.Calls++
	if e != nil {
		return e
	}
	to, e := p.Statuses(ctx, plan.ToTeam)
	plan.Calls++
	if e != nil {
		return e
	}
	var source *linearprovider.Status
	for i := range from {
		st := from[i]
		if st.TeamID != plan.FromTeam || !linearprovider.ValidID(st.ID) {
			return errors.New("workflow state team is not verified")
		}
		if (current.StateID != "" && st.ID == current.StateID) || (current.StateID == "" && st.Name == current.State) {
			if source != nil {
				return errors.New("the issue's state matches more than one of its team's states")
			}
			source = &from[i]
		}
	}
	if source == nil {
		plan.Refusals = append(plan.Refusals, "the issue's state isn't one of its team's states")
		return nil
	}
	plan.FromState = source.Name
	var byName, byType []linearprovider.Status
	var names []string
	for _, st := range to {
		if st.TeamID != plan.ToTeam || !linearprovider.ValidID(st.ID) {
			return errors.New("workflow state team is not verified")
		}
		names = append(names, st.Name)
		if st.Name == source.Name {
			byName = append(byName, st)
		}
		if source.Type != "" && st.Type == source.Type {
			byType = append(byType, st)
		}
	}
	switch {
	case len(byName) == 1:
		plan.ToState, plan.ToStateID, plan.StateBy = byName[0].Name, byName[0].ID, "name"
	case len(byName) == 0 && len(byType) == 1:
		plan.ToState, plan.ToStateID, plan.StateBy = byType[0].Name, byType[0].ID, "type"
	case source.Type == "triage" && len(byType) == 0:
		plan.Refusals = append(plan.Refusals, "the issue is in triage and the target team has no triage")
	default:
		plan.Refusals = append(plan.Refusals, fmt.Sprintf("no single target state for %q (by name, then by type %q); the target team's states: %s", source.Name, source.Type, strings.Join(names, ", ")))
	}
	return nil
}

// planMoveLabelsV2 maps each of the issue's labels, by name, to the target
// team's label set (its own, its bound parent's, the workspace's). A name
// with no label there, or two in one team, refuses the move.
func planMoveLabelsV2(ctx context.Context, p2 ProviderV2, v verifiedV2, b bus.TaskboardBindingV2, plan *MovePlanV2, current linearprovider.IssueV2) error {
	names := append([]string(nil), current.LabelNames...)
	if current.LabelsKnown && len(current.LabelIDs) > 0 {
		found, e := findTeamLabelsV2(ctx, p2, labelTeamsV2(v, b, plan.FromTeam), current.LabelIDs)
		plan.Calls++
		if e != nil {
			return e
		}
		names = nil
		for _, id := range current.LabelIDs {
			l, ok := found[id]
			if !ok {
				return errors.New("one of the issue's labels isn't its team's or the workspace's; nothing was planned")
			}
			names = append(names, l.Name)
		}
	} else if !current.LabelsKnown && len(names) == 0 {
		// Native reads omit an empty label list; that is no label.
		names = nil
	}
	sort.Strings(names)
	plan.names = names
	var missing []string
	for _, name := range names {
		set, e := labelSetInV2(ctx, p2, v, b, plan.ToTeam, name)
		plan.Calls += set.Calls
		if errors.Is(e, ErrLabelAmbiguous) {
			missing = append(missing, name+" (more than one in a team)")
			continue
		}
		if e != nil {
			return e
		}
		row := MoveLabelV2{Name: name, ToID: set.Apply}
		if i := slices.Index(set.IDs, set.Apply); i >= 0 {
			row.Scope = set.Scopes[i]
		}
		if set.Apply == "" {
			missing = append(missing, name)
		} else {
			plan.labelIDs = append(plan.labelIDs, set.Apply)
		}
		plan.Labels = append(plan.Labels, row)
	}
	if len(missing) > 0 {
		plan.Refusals = append(plan.Refusals, "the target team has no label for: "+strings.Join(missing, ", "))
	}
	if plan.labelIDs != nil {
		ids, e := sortedUniqueIDs(plan.labelIDs, 100)
		if e != nil {
			return errors.New("two of the issue's labels map to one target label; nothing was planned")
		}
		plan.labelIDs = ids
	}
	return nil
}

// movePlanDigest covers the plan by IDs only, with the issue's updatedAt
// (M6): any change to the issue, its state or labels, or the target's
// mapping refuses a confirm.
func movePlanDigest(plan MovePlanV2) string {
	c := plan.current
	var kids []string
	for _, k := range plan.Children {
		kids = append(kids, childDigest(k))
	}
	raw, _ := json.Marshal([]any{plan.IssueID, plan.updatedAt, plan.FromTeam, plan.ToTeam, c.StateID, plan.ToStateID, plan.labelIDs, len(plan.names), c.ProjectID, c.ParentID, c.ParentIdentifier, c.AssigneeID, c.CycleSet, plan.Refusals != nil, kids})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// moveReadBackV2 lists every field the read-back can't prove as planned:
// the same issue in the target team under a new identifier (the move's
// answer and the read agreeing), the planned state and labels, no cycle,
// and assignee, project, parent, priority and estimate unchanged.
func moveReadBackV2(plan MovePlanV2, moved, back linearprovider.IssueV2) []string {
	var off []string
	was := plan.current
	if back.ID != plan.IssueID || back.TeamID != plan.ToTeam {
		off = append(off, "team")
	}
	if back.Identifier == "" || back.Identifier == plan.Identifier || (moved.Identifier != "" && moved.Identifier != back.Identifier) {
		off = append(off, "identifier")
	}
	if (back.StateID != "" && back.StateID != plan.ToStateID) || (back.StateID == "" && back.State != plan.ToState) {
		off = append(off, "state")
	}
	if back.LabelsKnown {
		got, _ := sortedUniqueIDs(append([]string{}, back.LabelIDs...), 100)
		if strings.Join(got, ",") != strings.Join(plan.labelIDs, ",") {
			off = append(off, "labels")
		}
	} else {
		got := append([]string{}, back.LabelNames...)
		sort.Strings(got)
		if strings.Join(got, "\x00") != strings.Join(plan.names, "\x00") {
			off = append(off, "labels")
		}
	}
	if back.CycleKnown && back.CycleSet {
		off = append(off, "cycle")
	}
	if was.EstimateKnown && back.EstimateKnown && fmt.Sprint(was.Estimate == nil, deref(was.Estimate)) != fmt.Sprint(back.Estimate == nil, deref(back.Estimate)) {
		off = append(off, "estimate")
	}
	if back.ProjectID != was.ProjectID {
		off = append(off, "project")
	}
	if was.ParentKnown && (!back.ParentKnown || back.ParentID != was.ParentID || (back.ParentID == "" && back.ParentIdentifier != was.ParentIdentifier)) {
		off = append(off, "parent")
	}
	if was.AssigneeKnown && (!back.AssigneeKnown || back.AssigneeID != was.AssigneeID) {
		off = append(off, "assignee")
	}
	if was.PriorityKnown && (!back.PriorityKnown || *back.Priority != *was.Priority) {
		off = append(off, "priority")
	}
	return off
}

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
