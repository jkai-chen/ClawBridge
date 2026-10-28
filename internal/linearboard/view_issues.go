package linearboard

import (
	"context"
	"errors"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// An agent's issue read by a stored view (K3, LINEAR-VIEWS-PLAN §13.2): the
// usual binding-bound issue read, on the view's team, with the stored
// filter evaluated on each row. Linear never evaluates it for ClawBridge,
// so a view whose filter uses anything outside the subset is refused here.

// ViewIssuesPage is an issues page filtered by a view: the plain issues
// answer plus when the filter was read back from Linear, and the rows left
// out because a field the filter needs is unknown, by reason.
type ViewIssuesPage struct {
	linearprovider.Page[linearprovider.IssueV2]
	ViewID      string         `json:"view_id"`
	FilterAsOf  string         `json:"filter_as_of"`
	AgentFilter string         `json:"agent_filter"`
	Unknown     map[string]int `json:"unknown,omitempty"`
}

type issueView struct {
	id     string
	team   string
	filter ViewFilter
	states map[string]map[string]string
	// unknown counts the rows left out, by reason.
	unknown map[string]int
}

// narrow keeps the view's team, and the filter's projects when it names
// them; a team board's pairs carry no project and keep the rows' check.
func (v *issueView) narrow(pairs []pairV2) []pairV2 {
	projects := v.filter.Projects()
	var out []pairV2
	for _, p := range pairs {
		if p.TeamID != v.team || (p.ProjectID != "" && projects != nil && !contains(projects, p.ProjectID)) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// match evaluates the filter on one row; the team's state types are read
// once, only when the filter asks for a state type.
func (v *issueView) match(ctx context.Context, p Provider, row linearprovider.IssueV2) (bool, error) {
	types := v.states[row.TeamID]
	if types == nil && v.filter.needsStateTypes() {
		states, e := p.Statuses(ctx, row.TeamID)
		if e != nil {
			return false, e
		}
		types = map[string]string{}
		for _, st := range states {
			types[st.ID] = st.Type
		}
		v.states[row.TeamID] = types
	}
	ok, unknown := v.filter.Match(row, types)
	if unknown != "" {
		v.unknown[unknown]++
	}
	return ok, nil
}

// ViewIssuesV2 reads one page of issues by a stored view.
func (s *Service) ViewIssuesV2(ctx context.Context, scope bus.Request, viewID string, in IssuesInputV2) (ViewIssuesPage, error) {
	if !linearprovider.ValidID(viewID) {
		return ViewIssuesPage{}, errors.New("view_id must be a Linear view ID")
	}
	if in.LabelID != "" {
		return ViewIssuesPage{}, errors.New("view_id and label_id don't combine; the view's filter names its labels")
	}
	stored, e := s.storedView(ctx, scope, viewID)
	if e != nil {
		return ViewIssuesPage{}, e
	}
	if !stored.bound {
		return ViewIssuesPage{}, errors.New("this view's team left the binding; its issues aren't read")
	}
	f, e := ParseViewFilter([]byte(stored.filter))
	if e != nil || f.SHA256 != stored.sha {
		return ViewIssuesPage{}, errors.New("the recorded view's filter is unreadable")
	}
	if len(f.Uses) > 0 {
		return ViewIssuesPage{}, errors.New("agent_filter: unavailable (uses: " + strings.Join(f.Uses, ", ") + "): ClawBridge can't evaluate this view's filter, so agents don't filter by it")
	}
	view := &issueView{id: viewID, team: stored.team, filter: f, states: map[string]map[string]string{}, unknown: map[string]int{}}
	in.view = view
	page, e := s.IssuesV2(ctx, scope, in)
	if e != nil {
		return ViewIssuesPage{}, e
	}
	out := ViewIssuesPage{Page: page, ViewID: viewID, FilterAsOf: stored.asOf, AgentFilter: AgentFilterAvailable}
	if out.Items == nil {
		out.Items = []linearprovider.IssueV2{}
	}
	if len(view.unknown) > 0 {
		out.Unknown = view.unknown
	}
	return out, nil
}

// needsStateTypes reports a state-type clause.
func (f ViewFilter) needsStateTypes() bool {
	for _, c := range f.clauses {
		if c.key == "state_type" {
			return true
		}
	}
	return false
}
