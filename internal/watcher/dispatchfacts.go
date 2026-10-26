package watcher

import (
	"context"
	"errors"

	"clawbridge/internal/dispatch"
	"clawbridge/internal/linearprovider"
)

// The dispatcher's authoritative issue read (live/DISPATCHER-DESIGN.md r4
// §3.1, DP-H1): before the guardrails may confirm a claim, the server reads
// the issue itself, as the Linear app, at decision time. A client's report
// of the issue's project or labels never grants. One constant, read-only
// query; the caller charges it to the hour's budget.
const dispatchIssueQuery = `query DispatchIssue($id: String!) { issue(id: $id) { id project { id } labels(first: 50) { nodes { name } } state { type } } }`

// errIssueUnreadable is any read that cannot be trusted: a failure, another
// issue, or a label list that may be cut.
var errIssueUnreadable = errors.New("issue_unreadable")

// IssueFacts reads an issue's project, labels and state type as the app.
func (w *Watcher) IssueFacts(ctx context.Context, issueID string) (dispatch.Issue, error) {
	if !linearprovider.ValidID(issueID) {
		return dispatch.Issue{}, errIssueUnreadable
	}
	var out struct {
		Issue *struct {
			ID      string `json:"id"`
			Project *struct {
				ID string `json:"id"`
			} `json:"project"`
			Labels struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"labels"`
			State *struct {
				Type string `json:"type"`
			} `json:"state"`
		} `json:"issue"`
	}
	if err := w.App.GraphQL(ctx, dispatchIssueQuery, map[string]any{"id": issueID}, &out); err != nil || out.Issue == nil || out.Issue.ID != issueID || out.Issue.State == nil || out.Issue.State.Type == "" {
		return dispatch.Issue{}, errIssueUnreadable
	}
	// Fifty labels is the read's page: a fuller list is not proven.
	if len(out.Issue.Labels.Nodes) >= 50 {
		return dispatch.Issue{}, errIssueUnreadable
	}
	issue := dispatch.Issue{ID: issueID, StateType: out.Issue.State.Type, Labels: []string{}, Authoritative: true}
	if out.Issue.Project != nil {
		if !linearprovider.ValidID(out.Issue.Project.ID) {
			return dispatch.Issue{}, errIssueUnreadable
		}
		issue.ProjectID = out.Issue.Project.ID
	}
	for _, l := range out.Issue.Labels.Nodes {
		issue.Labels = append(issue.Labels, l.Name)
	}
	return issue, nil
}
