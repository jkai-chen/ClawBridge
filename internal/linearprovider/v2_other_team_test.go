package linearprovider

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// subTeamID is a second team, such as a bound sub-team of tid.
const subTeamID = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"

// Linear lists another team's issues under a team filter: a parent team's
// filter includes its sub-teams' issues, and a project that holds several
// teams lists them all. Such a row is returned for the caller to drop and
// count (Page.FilteredOtherTeam), never a failure of the whole page. A row
// outside the asked project still is one.
func TestIssuesV2ReturnsAnotherTeamsRows(t *testing.T) {
	row := func(id, team, project string) map[string]any {
		m := map[string]any{"id": id, "teamId": team, "projectId": project, "labels": []any{}}
		if project == "" {
			m["projectId"] = nil
		}
		return m
	}
	second := "0b0b0b0b-0b0b-4b0b-8b0b-0b0b0b0b0b0b"
	list := func(rows ...any) *routed {
		return &routed{fake: &fake{tools: []*mcp.Tool{nativeIssueList(), tool("get_issue", map[string]string{"id": "string"})}}, results: map[string]any{
			"list_issues": map[string]any{"issues": rows, "hasNextPage": false},
		}}
	}
	p, _ := NewWithCaller(context.Background(), list(row(iid, tid, uid), row(second, subTeamID, uid)))
	page, e := p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid, ProjectID: uid}})
	if e != nil || len(page.Items) != 2 || page.Items[0].TeamID != tid || page.Items[1].TeamID != subTeamID {
		t.Fatal("another team's row failed the page:", page, e)
	}
	// With no project asked (intake's team listing), the same.
	p, _ = NewWithCaller(context.Background(), list(row(second, subTeamID, "")))
	if page, e = p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid}}); e != nil || len(page.Items) != 1 {
		t.Fatal(page, e)
	}
	// Intake S1: the parent team's needs-agent label filter lists a
	// labelled sub-team issue. It is returned with its label, for the board
	// to read on its own team.
	needs := "0d0d0d0d-0d0d-4d0d-8d0d-0d0d0d0d0d0d"
	labelled := row(second, subTeamID, "")
	labelled["labels"] = []any{map[string]any{"id": needs, "name": "needs-agent"}}
	p, _ = NewWithCaller(context.Background(), list(labelled))
	if page, e = p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid}, LabelID: needs}); e != nil || len(page.Items) != 1 || page.Items[0].TeamID != subTeamID {
		t.Fatal("a labelled sub-team issue under the parent's S1 filter:", page, e)
	}
	// A row outside the asked project is still the provider escaping the
	// filter, for any team.
	for _, team := range []string{tid, subTeamID} {
		p, _ = NewWithCaller(context.Background(), list(row(second, team, "0c0c0c0c-0c0c-4c0c-8c0c-0c0c0c0c0c0c")))
		if _, e = p.IssuesV2(context.Background(), IssueQueryV2{IssueQuery: IssueQuery{TeamID: tid, ProjectID: uid}}); e == nil {
			t.Fatal("a row outside the asked project was accepted, team", team)
		}
	}
}
