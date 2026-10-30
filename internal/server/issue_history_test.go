package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
)

const (
	ihIssue   = "d2100000-0000-4000-8000-000000000001"
	ihProject = "p-example"
	ihTeam    = "c2100000-0000-4000-8000-000000000001"
	ihRead    = "c2100000-0000-4000-8000-000000000002"
	ihLinked  = "c2100000-0000-4000-8000-000000000003"
	ihOther   = "c2100000-0000-4000-8000-000000000004"
	ihLP      = "e2100000-0000-4000-8000-000000000001"
	ihOtherLP = "e2100000-0000-4000-8000-000000000002"
)

func refusedWith(t *testing.T, err error, code, text string) {
	t.Helper()
	if err == nil {
		t.Fatal("accepted; want", text)
	}
	got, _ := serviceError(err)
	if got != code || !strings.Contains(responseMessage(got, err), text) {
		t.Fatalf("refusal %s %q; want %s %q", got, responseMessage(got, err), code, text)
	}
}

// ClawBridge's cursor wraps Linear's, bound to the issue, the project and
// the binding revision; any other is refused before Linear is read.
func TestHistoryCursorIsBoundToTheIssueAndRevision(t *testing.T) {
	next, err := encodeHistoryCursor(historyCursor{Issue: ihIssue, Project: ihProject, Revision: 7, After: "linear-end-cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if after, err := decodeHistoryCursor(next, ihIssue, ihProject, 7); err != nil || after != "linear-end-cursor" {
		t.Fatal(after, err)
	}
	if after, err := decodeHistoryCursor("", ihIssue, ihProject, 7); err != nil || after != "" {
		t.Fatal(after, err)
	}
	for name, c := range map[string]struct {
		issue, project string
		revision       int64
	}{
		"another issue":    {"d2100000-0000-4000-8000-000000000009", ihProject, 7},
		"another project":  {ihIssue, "p-other", 7},
		"another revision": {ihIssue, ihProject, 8},
	} {
		_, err := decodeHistoryCursor(next, c.issue, c.project, c.revision)
		refusedWith(t, err, "invalid", "invalid_cursor")
		_ = name
	}
	raw := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{
		"not base64!",
		raw(`{"v":1,"issue":"` + ihIssue + `","project":"` + ihProject + `","revision":7,"after":"x","extra":1}`),
		raw(`{"v":2,"issue":"` + ihIssue + `","project":"` + ihProject + `","revision":7,"after":"x"}`),
		raw(`{"v":1,"issue":"` + ihIssue + `","project":"` + ihProject + `","revision":7,"after":""}`),
		raw(`{"v":1,"issue":"` + ihIssue + `","project":"` + ihProject + `","revision":7,"after":"x"}{}`),
		strings.Repeat("A", historyCursorMax+1),
	} {
		_, err := decodeHistoryCursor(bad, ihIssue, ihProject, 7)
		refusedWith(t, err, "invalid", "invalid_cursor")
	}
	// Linear's cursor is kept within the board's bound.
	if _, err := encodeHistoryCursor(historyCursor{Issue: ihIssue, Project: ihProject, Revision: 7, After: strings.Repeat("c", historyLinearCursorMax+1)}); err == nil {
		t.Fatal("an oversized Linear cursor was wrapped")
	}
}

// The arguments: issue_uuid, limit 1–50 (20 by default) and cursor only.
func TestHistoryRequestArguments(t *testing.T) {
	issue, limit, cursor, err := historyRequest(map[string]any{"instance": "i", "issue_uuid": ihIssue})
	if err != nil || issue != ihIssue || limit != 20 || cursor != "" {
		t.Fatal(issue, limit, cursor, err)
	}
	if _, limit, cursor, err = historyRequest(map[string]any{"issue_uuid": ihIssue, "limit": float64(50), "cursor": "c"}); err != nil || limit != 50 || cursor != "c" {
		t.Fatal(limit, cursor, err)
	}
	for _, args := range []map[string]any{
		{},
		{"issue_uuid": "ENG-1"},
		{"issue_uuid": strings.ToUpper(ihIssue)},
		{"issue_uuid": ihIssue, "limit": float64(0)},
		{"issue_uuid": ihIssue, "limit": float64(51)},
		{"issue_uuid": ihIssue, "limit": 2.5},
		{"issue_uuid": ihIssue, "limit": "20"},
		{"issue_uuid": ihIssue, "cursor": 3},
		{"issue_uuid": ihIssue, "cursor": strings.Repeat("A", historyCursorMax+1)},
		{"issue_uuid": ihIssue, "team": ihTeam},
		{"issue_uuid": ihIssue, "query": "{ viewer { id } }"},
	} {
		if _, _, _, err := historyRequest(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

// The app's read of the issue's team and project is checked against the
// stored binding, in the board's words.
func TestHistoryScopeAgainstTheBinding(t *testing.T) {
	team := bus.TaskboardBindingV2{Mode: "team_board", LinearTeamIDs: []string{ihTeam}, LinearOtherTeams: map[string]string{ihRead: bus.TeamAccessRead, ihLinked: bus.TeamAccessLinked}}
	set := bus.TaskboardBindingV2{Mode: "project_set", LinearTeamIDs: []string{ihTeam}, LinearProjectIDs: []string{ihLP}}
	in := func(team, project string) linearapp.IssueScope {
		return linearapp.IssueScope{ID: ihIssue, TeamID: team, ProjectID: project, Found: true}
	}
	for name, ok := range map[string]struct {
		b bus.TaskboardBindingV2
		s linearapp.IssueScope
	}{
		"a write team":            {team, in(ihTeam, "")},
		"a read team":             {team, in(ihRead, ihOtherLP)},
		"a verified project pair": {set, in(ihTeam, ihLP)},
	} {
		if err := historyScope(ok.b, ok.s); err != nil {
			t.Fatal(name, err)
		}
	}
	for name, c := range map[string]struct {
		b    bus.TaskboardBindingV2
		s    linearapp.IssueScope
		want string
	}{
		"an unbound team":       {team, in(ihOther, ""), historyOutsideScope},
		"a linked-only team":    {team, in(ihLinked, ""), "linked_only"},
		"a pair not verified":   {set, in(ihTeam, ihOtherLP), historyPairOutside},
		"no project in a set":   {set, in(ihTeam, ""), historyPairOutside},
		"a set's unbound team":  {set, in(ihOther, ihLP), historyOutsideScope},
		"an issue Linear lacks": {team, linearapp.IssueScope{}, "no issue UUID in this binding"},
	} {
		err := historyScope(c.b, c.s)
		refusedWith(t, err, "invalid", c.want)
		if !strings.Contains(err.Error(), "outside") && !strings.Contains(err.Error(), "linked_only") {
			t.Fatal(name, "is not refused as out of scope:", err)
		}
	}
}

// A server without the app refuses the read in its own words; nothing
// else is touched.
func TestHistoryNeedsTheApp(t *testing.T) {
	var none *AppWriter
	_, err := none.Execute(context.Background(), bus.Identity{Principal: "a@example.com"}, bus.Request{Operation: "linear_issue_history", Args: map[string]any{"issue_uuid": ihIssue}})
	refusedWith(t, err, "invalid", bus.IssueHistoryAppUnavailable)
	_, err = (&AppWriter{}).Execute(context.Background(), bus.Identity{Principal: "a@example.com"}, bus.Request{Operation: "linear_issue_history", Args: map[string]any{"issue_uuid": ihIssue}})
	refusedWith(t, err, "invalid", bus.IssueHistoryAppUnavailable)
}

// Bad arguments are refused before membership, the bus or Linear.
func TestHistoryRefusesBeforeAnyAccess(t *testing.T) {
	member := 0
	w := &AppWriter{App: &linearapp.App{}, Member: func(context.Context, bus.Identity, string) error { member++; return nil }}
	_, err := w.Execute(context.Background(), bus.Identity{Principal: "a@example.com"}, bus.Request{Operation: "linear_issue_history", Args: map[string]any{"issue_uuid": "ENG-1"}})
	refusedWith(t, err, "invalid", historyIssueRequired)
	if member != 0 {
		t.Fatal("membership was read for a refused request")
	}
}

// The client's older-server probe ({issue_uuid, probe: 1}) is refused by
// name even for a non-member: the argument check runs before membership,
// so it is never forbidden, and nothing is read or charged.
func TestHistoryProbeIsRefusedByNameForANonMember(t *testing.T) {
	member := 0
	w := &AppWriter{App: &linearapp.App{}, Member: func(context.Context, bus.Identity, string) error {
		member++
		return bus.PublicError("forbidden", "workspace membership required")
	}}
	_, err := w.Execute(context.Background(), bus.Identity{Principal: "stranger@example.com"}, bus.Request{Operation: "linear_issue_history", Workspace: "ws-example", Project: "p", Args: map[string]any{"issue_uuid": ihIssue, "probe": 1}})
	refusedWith(t, err, "invalid", historyArgs)
	if member != 0 {
		t.Fatal("membership was read for the probe")
	}
}

// A failed app read keeps no Linear text: rate limited says when, the cap
// says so, anything else is unavailable.
func TestHistoryLinearErrors(t *testing.T) {
	// An unavailable answer reaches the wire in the server's own words;
	// its cause stays in the error.
	for err, want := range map[error]string{historyLinearError(errHistoryCalls): "more than 3 Linear calls", historyLinearError(&linearapp.Error{Code: "unavailable"}): historyUnreadable} {
		if code, _ := serviceError(err); code != "unavailable" || !strings.Contains(err.Error(), want) {
			t.Fatal(code, err)
		}
	}
	refusedWith(t, historyLinearError(&linearapp.Error{Code: "rate_limited"}), "rate_limited", "retry at")
}

// The cap counts the GraphQL calls a request asks for, whatever HTTP
// requests each takes: the fourth is refused before it reaches Linear.
func TestHistoryCallCapCountsLogicalCalls(t *testing.T) {
	sent := 0
	gql := historyCalls(func(context.Context, string, map[string]any, any) error { sent++; return nil }, historyCallCap)
	for i := 0; i < historyCallCap; i++ {
		if err := gql(context.Background(), "query Q { x }", nil, nil); err != nil {
			t.Fatal(i, err)
		}
	}
	if err := gql(context.Background(), "query Q { x }", nil, nil); err != errHistoryCalls || sent != historyCallCap {
		t.Fatal("a fourth call went:", err, sent)
	}
}

// A team or project side outside the binding reads as outside, with no
// ID, key or name; in-binding sides and every other field stand. A team
// board binds no projects, so every project side is outside there.
func TestHistoryHidesNamesOutsideTheBinding(t *testing.T) {
	team := func(id, key string) *linearapp.HistoryTeam { return &linearapp.HistoryTeam{ID: id, Key: key} }
	project := func(id, name string) *linearapp.HistoryNamed { return &linearapp.HistoryNamed{ID: id, Name: name} }
	outsideTeam, outsideProject := &linearapp.HistoryTeam{Outside: true}, &linearapp.HistoryNamed{Outside: true}
	entries := func() []linearapp.HistoryEntry {
		return []linearapp.HistoryEntry{
			{Actor: linearapp.HistoryActor{Kind: "user", ID: "b1", Name: "Ada"},
				Team:     &linearapp.HistoryChange[linearapp.HistoryTeam]{From: team(ihOther, "SEC"), To: team(strings.ToUpper(ihTeam), "ENG")},
				Project:  &linearapp.HistoryChange[linearapp.HistoryNamed]{From: project(ihOtherLP, "Secret"), To: project(ihLP, "Launch")},
				Assignee: &linearapp.HistoryChange[linearapp.HistoryNamed]{To: project("b2", "Ben")},
				Parent:   &linearapp.HistoryChange[linearapp.HistoryParent]{To: &linearapp.HistoryParent{ID: "d9", Identifier: "SEC-1"}}},
			{Actor: linearapp.HistoryActor{Kind: "system"},
				Team: &linearapp.HistoryChange[linearapp.HistoryTeam]{From: team(ihRead, "OPS"), To: team(ihLinked, "LNK")}},
			{Actor: linearapp.HistoryActor{Kind: "system"}, Project: &linearapp.HistoryChange[linearapp.HistoryNamed]{To: project(ihLP, "Launch")}},
		}
	}
	set := bus.TaskboardBindingV2{Mode: "project_set", LinearTeamIDs: []string{ihTeam}, LinearProjectIDs: []string{ihLP}}
	got := historyOutside(set, entries())
	if e := got[0]; *e.Team.From != *outsideTeam || *e.Team.To != *team(strings.ToUpper(ihTeam), "ENG") ||
		*e.Project.From != *outsideProject || *e.Project.To != *project(ihLP, "Launch") ||
		*e.Assignee.To != *project("b2", "Ben") || e.Parent.To.Identifier != "SEC-1" || e.Actor.Name != "Ada" {
		t.Fatalf("%+v", e)
	}
	if e := got[1]; *e.Team.From != *outsideTeam || *e.Team.To != *outsideTeam {
		t.Fatalf("a set's unlisted teams: %+v %+v", e.Team.From, e.Team.To)
	}
	if e := got[2]; e.Project.From != nil || *e.Project.To != *project(ihLP, "Launch") {
		t.Fatalf("%+v", e.Project)
	}
	// A team board: its listed teams (write, read and linked) stand; it
	// binds no projects, so every project name is hidden.
	board := bus.TaskboardBindingV2{Mode: "team_board", LinearTeamIDs: []string{ihTeam}, LinearOtherTeams: map[string]string{ihRead: bus.TeamAccessRead, ihLinked: bus.TeamAccessLinked}}
	got = historyOutside(board, entries())
	if e := got[1]; *e.Team.From != *team(ihRead, "OPS") || *e.Team.To != *team(ihLinked, "LNK") {
		t.Fatalf("listed teams: %+v %+v", e.Team.From, e.Team.To)
	}
	if *got[0].Team.From != *outsideTeam || *got[0].Project.From != *outsideProject || *got[0].Project.To != *outsideProject || *got[2].Project.To != *outsideProject {
		t.Fatalf("a team board's projects: %+v %+v", got[0].Project, got[2].Project)
	}
	// On the wire: no ID, key or name.
	raw, _ := json.Marshal(got[0])
	for _, hidden := range []string{ihOther, "SEC\"", ihOtherLP, "Secret", ihLP, "Launch"} {
		if strings.Contains(string(raw), hidden) {
			t.Fatal("shown:", hidden, string(raw))
		}
	}
	if !strings.Contains(string(raw), `"team":{"from":{"outside":true},"to":{"id":"`+strings.ToUpper(ihTeam)+`","key":"ENG"}},"project":{"from":{"outside":true},"to":{"outside":true}}`) {
		t.Fatal(string(raw))
	}
}
