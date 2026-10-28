package linearprovider

import "context"

// An issue's move to another team (FR7, live/ISSUE-TEAM-MOVE-DESIGN.md):
// one save_issue that sends the new team with the state and the full label
// set already mapped to it, and clears the old team's cycle and estimate.
// Nothing is left to Linear's defaults.

// parseTeamFieldsV2 reads the fields a team move depends on, each only when
// the answer reports it in a form that can't be misread: a cycle (by ID or
// object, or an explicit null), an estimate (a number or null) and the
// sub-issues (a list, or a connection's nodes).
func parseTeamFieldsV2(m map[string]any, x *IssueV2) {
	for _, key := range []string{"cycleId", "cycle"} {
		raw, present := m[key]
		if !present {
			continue
		}
		switch v := raw.(type) {
		case nil:
			x.CycleKnown = true
		case string:
			x.CycleKnown, x.CycleSet = true, v != ""
		case map[string]any:
			if id := str(v, "id"); id != "" {
				x.CycleKnown, x.CycleSet = true, true
			} else if len(v) == 0 {
				x.CycleKnown = true
			}
		}
		if x.CycleKnown {
			break
		}
	}
	if raw, present := m["estimate"]; present {
		switch v := raw.(type) {
		case nil:
			x.EstimateKnown = true
		case float64:
			value := v
			x.Estimate, x.EstimateKnown = &value, true
		}
	}
	for _, key := range []string{"children", "subIssues", "subissues"} {
		raw, present := m[key]
		if !present {
			continue
		}
		rows, ok := raw.([]any)
		if conn, isConn := raw.(map[string]any); isConn {
			rows, ok = conn["nodes"].([]any)
		}
		if raw == nil {
			rows, ok = nil, true
		}
		if ok {
			x.Children, x.ChildrenKnown = len(rows), true
			return
		}
	}
}

// MoveInputV2 is a team move: the target team, its state and the full
// label set, all by UUID, and which old-team fields it clears.
type MoveInputV2 struct {
	ID, TeamID, StateID       string
	Labels                    []string
	ClearCycle, ClearEstimate bool
}

func moveArgsV2(in MoveInputV2) ([]arg, error) {
	if !ValidID(in.ID) || !ValidID(in.TeamID) || !ValidID(in.StateID) {
		return nil, fail("invalid")
	}
	labels := in.Labels
	if labels == nil {
		labels = []string{}
	}
	ids, e := canonicalIDs(labels, 100)
	if e != nil {
		return nil, e
	}
	args := []arg{a(in.ID, "id", "issueId"), a(in.TeamID, "team", "teamId"), a(in.StateID, "stateId", "state"), {[]string{"labelIds", "labels"}, ids, "array"}}
	if in.ClearCycle {
		args = append(args, arg{[]string{"cycleId", "cycle"}, nil, "null"})
	}
	if in.ClearEstimate {
		args = append(args, arg{[]string{"estimate"}, nil, "null"})
	}
	return args, nil
}

// ValidateMoveV2 negotiates the move without any call, so the ledger refuses
// a provider that can't express it before anything is recorded.
func (p *Provider) ValidateMoveV2(in MoveInputV2) error {
	args, e := moveArgsV2(in)
	if e != nil {
		return e
	}
	_, _, e = p.v2Negotiation([]string{"save_issue", "update_issue"}, args)
	return e
}

// MoveV2 sends the move. The answer must name the same issue; the caller
// proves the rest with a read by UUID.
func (p *Provider) MoveV2(ctx context.Context, in MoveInputV2) (IssueV2, error) {
	args, e := moveArgsV2(in)
	if e != nil {
		return IssueV2{}, e
	}
	m, e := p.v2Call(ctx, []string{"save_issue", "update_issue"}, args...)
	if e != nil {
		return IssueV2{}, e
	}
	x, e := p.parseIssueV2(ctx, unwrap(m, "issue"))
	if e != nil {
		return IssueV2{}, e
	}
	if x.ID != in.ID {
		return IssueV2{}, fail("scope_mismatch")
	}
	return x, nil
}

// SubIssues counts the issues Linear lists under parent, up to a page. Only
// an empty answer proves there are none: any row counts, so a filter Linear
// ignored can't pass for "no sub-issues" (FR7 M2).
func (p *Provider) SubIssues(ctx context.Context, parent string) (int, error) {
	if !ValidID(parent) {
		return 0, fail("invalid")
	}
	args, e := pageArgs(PageOptions{Limit: 10})
	if e != nil {
		return 0, e
	}
	m, e := p.v2Call(ctx, []string{"list_issues"}, append(args, a(parent, "parentId"))...)
	if e != nil {
		return 0, e
	}
	rows, _, _, e := list(m, "issues")
	if e != nil {
		return 0, e
	}
	return len(rows), nil
}
