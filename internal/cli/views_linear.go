package cli

import (
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearprovider"
)

// linear issues walks the binding's (team, project) pairs one page at a
// time, so a first page shows one team only and reads like a filter. Its
// text layout ends by saying which teams follow; --json is unchanged.

func init() {
	clirender.Register("linear_issues", linearIssuesView)
}

func linearIssuesView(v any) (clirender.Doc, bool) {
	r, ok := v.(bus.Result)
	if !ok {
		return clirender.Doc{}, false
	}
	page, ok := r.Data.(linearprovider.Page[linearprovider.IssueV2])
	if !ok || page.Complete || page.PageTeam == "" {
		return clirender.Doc{}, false
	}
	d, err := clirender.Generic(v)
	if err != nil {
		return clirender.Doc{}, false
	}
	d.Note(issuesFooter(page))
	return d, true
}

// issuesFooter names the page's team, the teams after it and the cursor.
func issuesFooter(page linearprovider.Page[linearprovider.IssueV2]) string {
	line := "page of team " + page.PageTeam
	if len(page.RemainingTeams) > 0 {
		line += "; more teams follow: " + strings.Join(page.RemainingTeams, ", ")
	}
	return line + "; pass --cursor " + page.NextCursor + " to continue"
}
