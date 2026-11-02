package linearprovider

import (
	"context"
	"math"
	"sort"
	"strings"
)

// Project milestones over Linear's MCP (FR55 PR 1). list_milestones answers
// one text content of JSON {milestones:[{id, name, progress 0..1, sortOrder,
// targetDate}]}; an issue's milestone is read as projectMilestone {id, name}
// or null. A value of any other shape is unknown, never guessed.

// Milestone is one milestone of a project.
type Milestone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// TargetDate is YYYY-MM-DD, nil for none; set only when TargetDateKnown.
	TargetDate      *string `json:"-"`
	TargetDateKnown bool    `json:"-"`
	// Progress is Linear's 0..1 as sent, never computed here; set only when
	// ProgressKnown.
	Progress      *float64 `json:"-"`
	ProgressKnown bool     `json:"-"`
	SortOrder     *float64 `json:"-"`
}

// MilestoneRowsMax bounds one project's listing.
const MilestoneRowsMax = 250

// milestoneReadCapable: list_milestones exists and takes a project string.
func (p *Provider) milestoneReadCapable() bool {
	sample := "00000000-0000-4000-8000-000000000001"
	_, _, e := p.v2Negotiation([]string{"list_milestones"}, []arg{a(sample, "project")})
	return e == nil
}

// milestoneCapable: save_issue takes milestone as a string on create and
// update, and list_milestones can prove which project a milestone is in.
func (p *Provider) milestoneCapable() bool {
	if !p.milestoneReadCapable() {
		return false
	}
	sample := "00000000-0000-4000-8000-000000000001"
	m := arg{[]string{"milestone"}, sample, "string"}
	ok := func(names []string, args ...arg) bool { _, _, e := p.v2Negotiation(names, args); return e == nil }
	return ok([]string{"create_issue", "save_issue"}, a(sample, "team", "teamId"), a("title", "title"), a("", "description"), m) &&
		ok([]string{"update_issue", "save_issue"}, a(sample, "id", "issueId"), m)
}

// ListMilestones reads a project's milestones, ordered by sort order and
// then name. A row whose ID is not a UUID or whose name is empty is refused
// (invalid_response): the list is never used in part.
func (p *Provider) ListMilestones(ctx context.Context, project string) ([]Milestone, error) {
	if !ValidID(project) {
		return nil, fail("invalid")
	}
	if !p.milestoneReadCapable() {
		return nil, fail("capability_unavailable")
	}
	m, e := p.v2Call(ctx, []string{"list_milestones"}, a(project, "project"))
	if e != nil {
		return nil, e
	}
	raw, ok := m["milestones"].([]any)
	if !ok {
		return nil, fail("invalid_response")
	}
	if len(raw) > MilestoneRowsMax {
		return nil, fail("result_limit")
	}
	out := make([]Milestone, 0, len(raw))
	seen := map[string]bool{}
	for _, v := range raw {
		row, ok := v.(map[string]any)
		if !ok {
			return nil, fail("invalid_response")
		}
		x, e := parseMilestone(row)
		if e != nil || seen[x.ID] {
			return nil, fail("invalid_response")
		}
		seen[x.ID] = true
		out = append(out, x)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.SortOrder != nil && b.SortOrder != nil && *a.SortOrder != *b.SortOrder:
			return *a.SortOrder < *b.SortOrder
		case (a.SortOrder != nil) != (b.SortOrder != nil):
			return a.SortOrder != nil
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name) || (strings.EqualFold(a.Name, b.Name) && a.Name < b.Name)
	})
	return out, nil
}

func parseMilestone(row map[string]any) (Milestone, error) {
	id := strings.ToLower(str(row, "id"))
	name := str(row, "name")
	if !ValidID(id) || strings.TrimSpace(name) == "" {
		return Milestone{}, fail("invalid_response")
	}
	x := Milestone{ID: id, Name: name}
	x.TargetDate, x.TargetDateKnown = parseMilestoneDate(row)
	if n, ok := row["progress"].(float64); ok && !math.IsNaN(n) && n >= 0 && n <= 1 {
		x.Progress, x.ProgressKnown = &n, true
	}
	if n, ok := row["sortOrder"].(float64); ok && !math.IsNaN(n) && !math.IsInf(n, 0) {
		x.SortOrder = &n
	}
	return x, nil
}

// parseMilestoneDate reads targetDate: a YYYY-MM-DD string, or null for
// none; an absent key or any other shape is unknown.
func parseMilestoneDate(row map[string]any) (*string, bool) {
	raw, present := row["targetDate"]
	if !present {
		return nil, false
	}
	if s, ok := raw.(string); ok && len(s) != 10 {
		return nil, false
	}
	return parseDateValue(raw)
}

// parseMilestoneRef reads an issue's projectMilestone: an object with a
// UUID id, or null for none. An absent key or any other shape is unknown.
func parseMilestoneRef(m map[string]any) (*string, bool) {
	raw, present := m["projectMilestone"]
	if !present {
		return nil, false
	}
	if raw == nil {
		return nil, true
	}
	node, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	id, ok := node["id"].(string)
	if !ok || !ValidID(id) {
		return nil, false
	}
	return &id, true
}

// MilestoneClearRefused is the refusal text of an issue milestone clear:
// no recorded Linear tool proves how to clear one, so it is never sent.
const MilestoneClearRefused = "clearing a milestone isn't available yet; nothing was sent"

// MilestoneSetRefused is the refusal text when the connection can't set one.
const MilestoneSetRefused = "this Linear connection can't set a milestone; nothing was sent"
