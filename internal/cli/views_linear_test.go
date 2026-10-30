package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearprovider"
)

// An incomplete multi-team page of linear issues ends, in text, by naming
// its team, the teams after it and the cursor; --json is the page as it
// was, and a page without a team (one pair, or the last page) has no
// footer.
func TestLinearIssuesTextSaysMoreTeamsFollow(t *testing.T) {
	render := func(mode clirender.Mode, page linearprovider.Page[linearprovider.IssueV2]) string {
		t.Helper()
		var out bytes.Buffer
		if err := emit(clirender.WithMode(t.Context(), mode), &out, "linear_issues", bus.Result{Source: "linear", Data: page}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	issue := linearprovider.IssueV2{Issue: linearprovider.Issue{ID: "11111111-1111-4111-8111-111111111111", Identifier: "ENG-1", Title: "First"}}
	page := linearprovider.Page[linearprovider.IssueV2]{Items: []linearprovider.IssueV2{issue}, NextCursor: "cur-next", PageTeam: "ENG", RemainingTeams: []string{"OPS", "WEB"}}
	text := render(clirender.Human, page)
	footer := "page of team ENG; more teams follow: OPS, WEB; pass --cursor cur-next to continue"
	if !strings.Contains(text, footer) || strings.Index(text, footer) < strings.Index(text, "ENG-1") {
		t.Fatalf("footer missing or before the rows:\n%s", text)
	}
	page.RemainingTeams = nil
	if text = render(clirender.Human, page); !strings.Contains(text, "page of team ENG; pass --cursor cur-next to continue") || strings.Contains(text, "more teams follow") {
		t.Fatalf("last team's continued page:\n%s", text)
	}
	page.RemainingTeams = []string{"OPS"}
	want, _ := json.Marshal(bus.Result{Source: "linear", Data: page})
	if got := render(clirender.JSON, page); got != string(want)+"\n" || strings.Contains(got, "more teams follow") {
		t.Fatalf("--json changed:\n%s", got)
	}
	for _, plain := range []linearprovider.Page[linearprovider.IssueV2]{{Items: []linearprovider.IssueV2{issue}, NextCursor: "cur-next"}, {Items: []linearprovider.IssueV2{issue}, Complete: true}} {
		if text = render(clirender.Human, plain); strings.Contains(text, "page of team") {
			t.Fatalf("footer on a page without a team:\n%s", text)
		}
	}
}
