package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
)

// FR21: `linear issue history ISSUE` takes a UUID or an identifier, a
// limit 1–50 (default 20) and a cursor; anything else is refused before a
// profile or the server is touched.
func TestLinearIssueHistoryFlags(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	for _, ref := range []string{id, "ENG-88"} {
		v, pos, err := parseLinear("issue-history", []string{ref, "--profile", "native", "--limit", "5", "--cursor", "c1"}, &bytes.Buffer{})
		if err != nil || len(pos) != 1 || pos[0] != ref || v.limit != 5 || v.cursor != "c1" || v.profile != "native" {
			t.Fatal(ref, v.limit, v.cursor, pos, err)
		}
	}
	if v, _, err := parseLinear("issue-history", []string{"ENG-88", "--profile", "native"}, &bytes.Buffer{}); err != nil || v.limit != 20 {
		t.Fatal("default limit", v.limit, err)
	}
	for _, args := range [][]string{
		{},
		{"eng-88"},
		{"ENG-88", "ENG-89"},
		{"ENG-88", "--profile", "native", "--limit", "0"},
		{"ENG-88", "--profile", "native", "--limit", "51"},
		{"ENG-88", "--profile", "native", "--comments"},
	} {
		if _, _, err := parseLinear("issue-history", args, &bytes.Buffer{}); err == nil {
			t.Fatal("accepted", args)
		}
	}
	isolated(t)
	previous := newLinearManager
	t.Cleanup(func() { newLinearManager = previous })
	newLinearManager = func(string) *linearconn.Manager {
		t.Fatal("refused arguments touched the manager")
		return nil
	}
	var out, diag bytes.Buffer
	if err := Run(context.Background(), []string{"linear", "issue", "history", "--help"}, &out, &diag); err != nil || !strings.Contains(diag.String(), "-cursor") {
		t.Fatal(err, diag.String())
	}
	// linear issue --help and the usage say history comes first.
	diag.Reset()
	if err := Run(context.Background(), []string{"linear", "issue", "--help"}, &out, &diag); err != nil || !strings.Contains(diag.String(), "clawbridge linear issue history ISSUE [--profile P] [--limit N] [--cursor C]") || !strings.Contains(diag.String(), "history comes first") {
		t.Fatal(err, diag.String())
	}
	out.Reset()
	if err := Run(context.Background(), []string{"linear", "--help"}, &out, &diag); err != nil || !strings.Contains(out.String(), "issue history ISSUE [--profile P] [--limit N] [--cursor C]") || !strings.Contains(out.String(), "history comes first") {
		t.Fatal(err, out.String())
	}
	for _, args := range [][]string{
		{"linear", "issue", "history", "ENG-88", "--profile", "p", "--limit", "60"},
		{"linear", "issue", "history", "not-an-issue", "--profile", "p"},
		{"linear", "issue", "history", "--profile", "p"},
	} {
		rejectCLI(t, args...)
	}
	err := Run(context.Background(), []string{"linear", "issue-history", "ENG-88"}, &out, &diag)
	if err == nil || !strings.Contains(err.Error(), "use linear issue history ISSUE") {
		t.Fatal(err)
	}
}

func historyFixture() linearboard.IssueHistoryPageV2 {
	yes, no := true, false
	zero, two, three := 0.0, 2.0, 3.0
	state := func(id, name, kind string) *linearapp.HistoryState {
		return &linearapp.HistoryState{ID: id, Name: name, Type: kind}
	}
	return linearboard.IssueHistoryPageV2{
		IssueUUID: "d7000000-0000-4000-8000-0000000000d1", Identifier: "ENG-1", Limit: 20, HasMore: true, NextCursor: strings.Repeat("Q", 180),
		Entries: []linearapp.HistoryEntry{
			{At: "2026-10-30T03:12:09Z", Actor: linearapp.HistoryActor{Kind: "integration", ID: "f1000000-0000-4000-8000-000000000001", Name: "GitHub"},
				State: &linearapp.HistoryChange[linearapp.HistoryState]{From: state("c1", "In Progress", "started"), To: state("c2", "Todo", "unstarted")}},
			{At: "2026-10-30T03:10:00.5+01:00", Actor: linearapp.HistoryActor{Kind: "app", ID: "a7000000-0000-4000-8000-0000000000a1", Name: "ClawBridge app"},
				Assignee: &linearapp.HistoryChange[linearapp.HistoryNamed]{To: &linearapp.HistoryNamed{ID: "b2", Name: "ben"}},
				Labels:   &linearapp.HistoryLabels{Added: []linearapp.HistoryLabel{{ID: "l1", Name: "Bug"}, {ID: "f2000000-0000-4000-8000-000000000009"}}, Removed: []linearapp.HistoryLabel{{ID: "l2", Name: "Feature"}}}},
			{At: "2026-10-29T10:00:00Z", Actor: linearapp.HistoryActor{Kind: "user", ID: "b1", Name: "Ada Example"},
				Team:     &linearapp.HistoryChange[linearapp.HistoryTeam]{From: &linearapp.HistoryTeam{ID: "t1", Key: "ENG"}, To: &linearapp.HistoryTeam{ID: "t2", Key: "OPS"}},
				Project:  &linearapp.HistoryChange[linearapp.HistoryNamed]{To: &linearapp.HistoryNamed{ID: "p1", Name: "Launch"}},
				Priority: &linearapp.HistoryChange[float64]{From: &zero, To: &two},
				Parent:   &linearapp.HistoryChange[linearapp.HistoryParent]{To: &linearapp.HistoryParent{ID: "d5", Identifier: "ENG-0"}}},
			{At: "2026-10-29T09:00:00Z", Actor: linearapp.HistoryActor{Kind: "system"}, Archived: &yes},
			// False and an unchanged priority are no change: other.
			{At: "2026-10-28T12:00:00Z", Actor: linearapp.HistoryActor{Kind: "user", ID: "b1", Name: "Ada Example"}, Archived: &no, Trashed: &no,
				Priority: &linearapp.HistoryChange[float64]{From: &three, To: &three}},
			{At: "2026-10-28T09:00:00Z", Actor: linearapp.HistoryActor{Kind: "user", ID: "b7000000-0000-4000-8000-0000000000a3", Name: "b7000000-0000-4000-8000-0000000000a3"}, Other: true},
		},
	}
}

// The text view: one line per change, TIME ACTOR: CHANGE[; CHANGE…], in
// UTC to the minute, then the next page's cursor whole on its own line.
func TestLinearIssueHistoryText(t *testing.T) {
	page := historyFixture()
	var out bytes.Buffer
	if err := clirender.Write(&out, clirender.Human, "linear_issue_history", bus.Result{Source: "linear", Data: page}); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"2026-10-30 03:12Z integration GitHub: state In Progress → Todo",
		"2026-10-30 02:10Z app ClawBridge app: assignee none → ben; labels +Bug +f2000000-0000-4000-8000-000000000009 -Feature",
		"2026-10-29 10:00Z user Ada Example: team ENG → OPS; project none → Launch; priority none → high; parent none → ENG-0",
		"2026-10-29 09:00Z system: archived",
		"2026-10-28 12:00Z user Ada Example: other change",
		"2026-10-28 09:00Z user b7000000-0000-4000-8000-0000000000a3: other change",
		"next: --cursor " + page.NextCursor,
	}, "\n") + "\n"
	if out.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", out.String(), want)
	}
	// The last page has no cursor line; an empty one says so; names are
	// made safe for the terminal.
	page.HasMore, page.NextCursor, page.Entries = false, "", page.Entries[:1]
	page.Entries[0].Actor.Name = "Git\x1b[2JHub"
	out.Reset()
	_ = clirender.Write(&out, clirender.Human, "linear_issue_history", bus.Result{Source: "linear", Data: page})
	if out.String() != "2026-10-30 03:12Z integration Git\\x1b[2JHub: state In Progress → Todo\n" {
		t.Fatalf("%q", out.String())
	}
	page.Entries = []linearapp.HistoryEntry{}
	out.Reset()
	_ = clirender.Write(&out, clirender.Human, "linear_issue_history", bus.Result{Source: "linear", Data: page})
	if out.String() != "No history.\n" {
		t.Fatalf("%q", out.String())
	}
}

// --json is the page as the board returned it, in the CLI's result.
func TestLinearIssueHistoryJSON(t *testing.T) {
	page := historyFixture()
	var out bytes.Buffer
	if err := clirender.Write(&out, clirender.JSON, "linear_issue_history", bus.Result{Source: "linear", Data: page}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Source string `json:"source"`
		Data   struct {
			IssueUUID  string           `json:"issue_uuid"`
			Identifier string           `json:"identifier"`
			HasMore    bool             `json:"has_more"`
			NextCursor string           `json:"next_cursor"`
			Entries    []map[string]any `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err, out.String())
	}
	if got.Source != "linear" || got.Data.IssueUUID != page.IssueUUID || got.Data.Identifier != "ENG-1" || !got.Data.HasMore || got.Data.NextCursor != page.NextCursor || len(got.Data.Entries) != 6 {
		t.Fatalf("%+v", got)
	}
	if e := got.Data.Entries[5]; e["other"] != true || e["state"] != nil {
		t.Fatal(e)
	}
	if e := got.Data.Entries[3]; e["archived"] != true || e["trashed"] != nil || e["actor"].(map[string]any)["kind"] != "system" {
		t.Fatal(e)
	}
}

// #319: an out-of-scope refusal of `linear issue history` names the scope
// it used, as every linear command's does. The refusal is the board's own
// (taken from a real out-of-scope read); the history read returns it.
func TestLinearIssueHistoryOutOfScopeNamesTheScope(t *testing.T) {
	scopeFixture(t)
	view := []string{"view", "list", "--team", "11111111-2222-4333-8444-555555555555", "--profile", "prof-1"}
	outside := errors.Unwrap(runLinear(context.Background(), Config{Project: "p"}, view, &bytes.Buffer{}, &bytes.Buffer{}))
	if !linearboard.IsOutOfScope(outside) {
		t.Fatalf("no bare out-of-scope refusal: %v", outside)
	}
	previous := issueHistoryRead
	t.Cleanup(func() { issueHistoryRead = previous })
	var asked string
	issueHistoryRead = func(_ *linearboard.Service, _ context.Context, _ bus.Request, ref string, in linearboard.IssueHistoryInputV2) (linearboard.IssueHistoryPageV2, error) {
		asked = ref
		return linearboard.IssueHistoryPageV2{}, outside
	}
	var out bytes.Buffer
	err := runLinear(context.Background(), Config{Project: "p"}, []string{"issue", "history", "ENG-88", "--profile", "prof-1"}, &out, &bytes.Buffer{})
	want := outside.Error() + "; this used workspace w and project p"
	if err == nil || err.Error() != want || !linearboard.IsOutOfScope(err) || asked != "ENG-88" || out.Len() != 0 {
		t.Fatalf("got %v (asked %q)\nwant %s", err, asked, want)
	}
}

// A team or project side outside the project's binding comes from the
// server as {"outside":true}: the text names it so, and the JSON keeps it
// with no ID, key or name.
func TestLinearIssueHistoryOutsideSides(t *testing.T) {
	raw := `{"issue_uuid":"d7000000-0000-4000-8000-0000000000d1","identifier":"ENG-1","limit":20,"has_more":false,"entries":[` +
		`{"at":"2026-10-29T10:00:00Z","actor":{"kind":"user","id":"b1","name":"Ada Example"},` +
		`"team":{"from":{"id":"t1","key":"ENG"},"to":{"outside":true}},"project":{"from":{"outside":true},"to":null}}]}`
	var page linearboard.IssueHistoryPageV2
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := clirender.Write(&out, clirender.Human, "linear_issue_history", bus.Result{Source: "linear", Data: page}); err != nil {
		t.Fatal(err)
	}
	want := "2026-10-29 10:00Z user Ada Example: team ENG → (outside this project's binding); project (outside this project's binding) → none\n"
	if out.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", out.String(), want)
	}
	back, _ := json.Marshal(page.Entries[0])
	if !strings.Contains(string(back), `"team":{"from":{"id":"t1","key":"ENG"},"to":{"outside":true}},"project":{"from":{"outside":true},"to":null}`) {
		t.Fatal(string(back))
	}
	// A move between two places outside the binding says only that.
	page.Entries[0].Team.From = &linearapp.HistoryTeam{Outside: true}
	page.Entries[0].Project = nil
	out.Reset()
	if err := clirender.Write(&out, clirender.Human, "linear_issue_history", bus.Result{Source: "linear", Data: page}); err != nil {
		t.Fatal(err)
	}
	if want = "2026-10-29 10:00Z user Ada Example: team changed outside this project's binding\n"; out.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", out.String(), want)
	}
}
