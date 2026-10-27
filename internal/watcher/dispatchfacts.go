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

// dispatchIssueTextQuery is the same read with the issue's title and
// description, for a model case in the same Linear call (d5 Q8): the text
// is others' writing, handed to the model fenced as untrusted data.
const dispatchIssueTextQuery = `query DispatchIssueText($id: String!) { issue(id: $id) { id title description project { id } labels(first: 50) { nodes { name } } state { type } } }`

// IssueText is an issue's title and description, as read: untrusted.
type IssueText struct{ Title, Description string }

// errIssueUnreadable is any read that cannot be trusted: a failure, another
// issue, or a label list that may be cut.
var errIssueUnreadable = errors.New("issue_unreadable")

// IssueFacts reads an issue's project, labels and state type as the app.
func (w *Watcher) IssueFacts(ctx context.Context, issueID string) (dispatch.Issue, error) {
	issue, _, err := w.issueRead(ctx, dispatchIssueQuery, issueID)
	return issue, err
}

// IssueFactsText is IssueFacts with the issue's title and description.
func (w *Watcher) IssueFactsText(ctx context.Context, issueID string) (dispatch.Issue, IssueText, error) {
	return w.issueRead(ctx, dispatchIssueTextQuery, issueID)
}

func (w *Watcher) issueRead(ctx context.Context, query, issueID string) (dispatch.Issue, IssueText, error) {
	if !linearprovider.ValidID(issueID) {
		return dispatch.Issue{}, IssueText{}, errIssueUnreadable
	}
	var out struct {
		Issue *struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Project     *struct {
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
	if err := w.App.GraphQL(ctx, query, map[string]any{"id": issueID}, &out); err != nil || out.Issue == nil || out.Issue.ID != issueID || out.Issue.State == nil || out.Issue.State.Type == "" {
		return dispatch.Issue{}, IssueText{}, errIssueUnreadable
	}
	// Fifty labels is the read's page: a fuller list is not proven.
	if len(out.Issue.Labels.Nodes) >= 50 {
		return dispatch.Issue{}, IssueText{}, errIssueUnreadable
	}
	issue := dispatch.Issue{ID: issueID, StateType: out.Issue.State.Type, Labels: []string{}, Authoritative: true}
	if out.Issue.Project != nil {
		if !linearprovider.ValidID(out.Issue.Project.ID) {
			return dispatch.Issue{}, IssueText{}, errIssueUnreadable
		}
		issue.ProjectID = out.Issue.Project.ID
	}
	for _, l := range out.Issue.Labels.Nodes {
		issue.Labels = append(issue.Labels, l.Name)
	}
	return issue, IssueText{Title: out.Issue.Title, Description: out.Issue.Description}, nil
}
