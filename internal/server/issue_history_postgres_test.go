package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
)

const (
	awHistoryAda    = "b7000000-0000-4000-8000-0000000000a1"
	awHistoryBen    = "b7000000-0000-4000-8000-0000000000a2"
	awMissingIssue  = "d7000000-0000-4000-8000-0000000000d9"
	awHistoryLinear = "linear-end-cursor-1"
)

func (f *awFixture) history(as bus.Identity, session, instance string, args map[string]any) (map[string]any, error) {
	if session != "" {
		args["instance"] = instance
	}
	out, err := f.writer.Execute(context.Background(), as, bus.Request{Operation: "linear_issue_history", Workspace: f.workspace, Project: "p", Session: session, Args: args})
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(out.Data)
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	return data, nil
}

func (f *awFixture) budget() bus.LinearBudget {
	f.t.Helper()
	access := f.must(bus.Request{Operation: "taskboard_access", Workspace: f.workspace, Project: "p", Args: map[string]any{"ledger": 1}})
	b, err := f.store.LinearBudgetOf(context.Background(), access.Data.(map[string]any)["project_id"].(string))
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

// historyKind: the hour's app_read.history calls are calls, and the only
// kind charged.
func historyKind(b bus.LinearBudget, calls int) bool {
	return len(b.Kinds) == 1 && b.Kinds[0].Kind == "app_read.history" && b.Kinds[0].Calls == calls
}

// End to end on Postgres: an issue's history is read as the app, its
// actors named by ClawBridge's stored name, else Linear's, and each kind
// told apart; the Linear calls are at most three and charged under the
// caller; the cursor pages and is refused for another issue; an issue
// outside the binding, or one Linear doesn't return, reads no history.
func TestPostgresIssueHistoryAsTheApp(t *testing.T) {
	f := newAWFixture(t, awOrg)
	// The owner is linked to a Linear user and has a stored name.
	f.must(bus.Request{Operation: "intake_trust_add", Workspace: f.workspace, Project: "p", Args: map[string]any{"linear_user_id": awHistoryAda, "principal": f.owner}})
	f.must(bus.Request{Operation: "member_display_name_set", Workspace: f.workspace, Args: map[string]any{"display_name": "Ada Example"}})
	f.api.issues = map[string]awHistoryIssue{
		awIssue: {team: awTeam, more: true, end: awHistoryLinear, nodes: []any{
			map[string]any{"createdAt": "2026-10-30T10:00:02Z", "actorId": awHistoryAda, "actor": map[string]any{"id": awHistoryAda, "displayName": "ada"}, "fromState": map[string]any{"id": "c1000000-0000-4000-8000-0000000000a1", "name": "Backlog", "type": "backlog"}, "toState": map[string]any{"id": "c1000000-0000-4000-8000-0000000000a2", "name": "Todo", "type": "unstarted"}},
			map[string]any{"createdAt": "2026-10-30T10:00:01Z", "actorId": awAppUser, "actor": map[string]any{"id": awAppUser, "displayName": "clawbridge", "app": true}, "addedLabels": []any{map[string]any{"id": "f1000000-0000-4000-8000-000000000001", "name": "Bug"}}, "removedLabels": []any{}},
			map[string]any{"createdAt": "2026-10-30T10:00:00Z", "actorId": awHistoryBen, "actor": map[string]any{"id": awHistoryBen, "displayName": "ben@example.com"}},
		}},
		awForeignIssue: {team: awOtherTeam, nodes: []any{}},
	}
	before := f.api.graphqlCount()
	first, err := f.history(f.identity(), awSession, awSession+"-i", map[string]any{"issue_uuid": awIssue, "limit": 3})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.api.graphqlCount() - before
	if calls < 2 || calls > historyCallCap {
		t.Fatal("Linear calls for one read:", calls)
	}
	entries, _ := first["entries"].([]any)
	if len(entries) != 3 || first["has_more"] != true || first["next_cursor"] == "" || first["issue_uuid"] != awIssue {
		t.Fatal(first)
	}
	actor := func(i int) map[string]any { return entries[i].(map[string]any)["actor"].(map[string]any) }
	if a := actor(0); a["kind"] != "user" || a["name"] != "Ada Example" || a["id"] != awHistoryAda {
		t.Fatal("stored name:", a)
	}
	if a := actor(1); a["kind"] != "app" || a["id"] != awAppUser {
		t.Fatal("the app:", a)
	}
	if a := actor(2); a["kind"] != "user" || a["name"] != awHistoryBen {
		t.Fatal("an email was shown:", a)
	}
	if entries[0].(map[string]any)["state"] == nil || entries[1].(map[string]any)["labels"] == nil || entries[2].(map[string]any)["other"] != true {
		t.Fatal(entries)
	}
	if v := f.api.historyVars[0]; v["first"] != float64(3) || v["after"] != nil {
		t.Fatal(v)
	}
	// Charged by the server under the session: to agents, as the app's
	// history read.
	if b := f.budget(); b.By.Agents != calls || b.By.People != 0 || !historyKind(b, calls) {
		t.Fatal("charge", b, calls)
	}
	// The next page, as the person: Linear's cursor goes back to Linear,
	// and the read is charged to people.
	before = f.api.graphqlCount()
	if _, err = f.history(f.identity(), "", "", map[string]any{"issue_uuid": awIssue, "cursor": first["next_cursor"]}); err != nil {
		t.Fatal(err)
	}
	second := f.api.graphqlCount() - before
	if v := f.api.historyVars[1]; v["after"] != awHistoryLinear || v["first"] != float64(20) {
		t.Fatal(v)
	}
	if b := f.budget(); b.By.People != second || b.By.Agents != calls || !historyKind(b, calls+second) {
		t.Fatal("charge", b, second)
	}
	// The cursor is for this issue only: refused before Linear.
	before = f.api.graphqlCount()
	_, err = f.history(f.identity(), "", "", map[string]any{"issue_uuid": awForeignIssue, "cursor": first["next_cursor"]})
	refusedWith(t, err, "invalid", "invalid_cursor")
	if f.api.graphqlCount() != before {
		t.Fatal("a refused cursor reached Linear")
	}
	// An issue outside the binding: its scope is read, its history never.
	reads := len(f.api.historyVars)
	_, err = f.history(f.identity(), "", "", map[string]any{"issue_uuid": awForeignIssue})
	refusedWith(t, err, "invalid", "outside the selected task-board scope")
	_, err = f.history(f.identity(), "", "", map[string]any{"issue_uuid": awMissingIssue})
	refusedWith(t, err, "invalid", "no issue UUID in this binding")
	if len(f.api.historyVars) != reads {
		t.Fatal("history read for an issue outside the binding")
	}
	// A reader may read it too.
	reader := awUnique("reader") + "@appwrite-test.example"
	f.must(bus.Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": reader, "role": "reader"}})
	if _, err = f.history(bus.Identity{Principal: reader, Issuer: bus.GoogleIssuer, Subject: "sub-reader"}, "", "", map[string]any{"issue_uuid": awIssue}); err != nil {
		t.Fatal("a reader:", err)
	}
	// A stranger is refused before the app reads anything.
	before = f.api.graphqlCount()
	_, err = f.history(bus.Identity{Principal: awUnique("stranger") + "@appwrite-test.example", Issuer: bus.GoogleIssuer, Subject: "sub-stranger"}, "", "", map[string]any{"issue_uuid": awIssue})
	if err == nil || !strings.Contains(err.Error(), "membership") || f.api.graphqlCount() != before {
		t.Fatal("a stranger read the history:", err)
	}
	// The client's older-server probe: a stranger's request with an
	// argument the read never takes is refused by name, not forbidden,
	// before membership, Linear or any charge.
	charged := f.budget()
	_, err = f.history(bus.Identity{Principal: awUnique("stranger") + "@appwrite-test.example", Issuer: bus.GoogleIssuer, Subject: "sub-stranger"}, "", "", map[string]any{"issue_uuid": awIssue, "probe": 1})
	refusedWith(t, err, "invalid", historyArgs)
	if b := f.budget(); f.api.graphqlCount() != before || b.By.People != charged.By.People || b.By.Agents != charged.By.Agents || b.Used != charged.Used {
		t.Fatal("the probe reached Linear or was charged", b, charged)
	}
	if n := f.mcp.count("save_comment") + f.mcp.count("save_issue"); n != 0 {
		t.Fatal("the read wrote to Linear", n)
	}
}

// A project set: the pair must be one of the binding's projects, and the
// history names no team or project outside the binding: those sides read
// as outside, with no ID, key or name.
func TestPostgresIssueHistoryProjectSetPair(t *testing.T) {
	f := newAWFixture(t, awOrg)
	bound := "e7000000-0000-4000-8000-0000000000e1"
	b := f.must(bus.Request{Operation: "taskboard_get_v2", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i"}})
	revision := b.Data.(map[string]any)["revision"]
	f.must(bus.Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{
		"instance": awSession + "-i", "expected_revision": revision, "mode": "project_set",
		"linear_team_ids": []string{awTeam}, "linear_default_team_id": awTeam, "linear_project_ids": []string{bound}, "linear_workspace_id": awOrg,
	}})
	foreign := "e7000000-0000-4000-8000-0000000000e2"
	f.api.issues = map[string]awHistoryIssue{
		awIssue: {team: awTeam, project: bound, nodes: []any{
			map[string]any{"createdAt": "2026-10-30T10:00:00Z", "fromTeam": map[string]any{"id": awOtherTeam, "key": "SEC"}, "toTeam": map[string]any{"id": awTeam, "key": "ENG"},
				"fromProject": map[string]any{"id": foreign, "name": "Secret Example"}, "toProject": map[string]any{"id": bound, "name": "Launch"}},
		}},
		awForeignIssue: {team: awTeam, project: foreign, nodes: []any{}},
	}
	out, err := f.history(f.identity(), "", "", map[string]any{"issue_uuid": awIssue})
	if err != nil || out["has_more"] != false {
		t.Fatal(out, err)
	}
	entry := out["entries"].([]any)[0].(map[string]any)
	raw, _ := json.Marshal(entry)
	if !strings.Contains(string(raw), `"team":{"from":{"outside":true},"to":{"id":"`+awTeam+`","key":"ENG"}}`) ||
		!strings.Contains(string(raw), `"project":{"from":{"outside":true},"to":{"id":"`+bound+`","name":"Launch"}}`) ||
		strings.Contains(string(raw), awOtherTeam) || strings.Contains(string(raw), "SEC") || strings.Contains(string(raw), foreign) || strings.Contains(string(raw), "Secret") {
		t.Fatal("outside the binding:", string(raw))
	}
	_, err = f.history(f.identity(), "", "", map[string]any{"issue_uuid": awForeignIssue})
	refusedWith(t, err, "invalid", "pair is outside the verified scope")
}

// An issue that moves to an unbound team between the scope read and the
// history read is refused on the history read's own scope: no entries go
// back, and both calls are still charged.
func TestPostgresIssueHistoryMovedBetweenReads(t *testing.T) {
	f := newAWFixture(t, awOrg)
	f.api.issues = map[string]awHistoryIssue{
		awIssue: {team: awTeam, movedTo: awOtherTeam, nodes: []any{
			map[string]any{"createdAt": "2026-10-30T10:00:00Z", "actorId": awHistoryAda, "actor": map[string]any{"id": awHistoryAda, "displayName": "ada"}},
		}},
	}
	before, charged := f.api.graphqlCount(), f.budget().By.People
	out, err := f.history(f.identity(), "", "", map[string]any{"issue_uuid": awIssue})
	refusedWith(t, err, "invalid", "outside the selected task-board scope")
	if out != nil {
		t.Fatal("entries went back:", out)
	}
	// The scope read and the history read, and the app's identity on a
	// fresh fixture.
	calls := f.api.graphqlCount() - before
	if calls < 2 || calls > historyCallCap {
		t.Fatal("Linear calls:", calls)
	}
	if got := f.budget().By.People - charged; got != calls {
		t.Fatal("charged", got, "for", calls, "calls")
	}
}

// A token Linear refuses mid-read is re-minted inside the one history
// call: the read still succeeds within the cap, and the re-sent request is
// charged. Only Linear's entity-not-found reads as no issue; any other
// failure is unavailable.
func TestPostgresIssueHistoryRemintAndFailures(t *testing.T) {
	f := newAWFixture(t, awOrg)
	// An app whose clock passes the mint pace, so a 401 can re-mint.
	clock := time.Now()
	u, _ := url.Parse("https://mcp.linear.app")
	app, err := linearapp.New(linearapp.Config{
		ClientID: awUnique("client"), Secret: "s3cret-e2e", Key: []byte("0123456789abcdef0123456789abcdef"), Store: f.store,
		HTTP: &http.Client{Transport: f.api}, MCPTransport: awToLocal{u, http.DefaultTransport}, Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.writer = &AppWriter{App: app, Store: f.store}
	f.api.issues = map[string]awHistoryIssue{
		awIssue:        {team: awTeam, nodes: []any{map[string]any{"createdAt": "2026-10-30T10:00:00Z"}}},
		awForeignIssue: {team: awTeam, broken: true},
	}
	if _, err = f.history(f.identity(), "", "", map[string]any{"issue_uuid": awIssue}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	f.api.unauthorizedHistory = 1
	mints, before, charged := f.api.mints, f.api.graphqlCount(), f.budget().By.People
	out, err := f.history(f.identity(), "", "", map[string]any{"issue_uuid": awIssue})
	if err != nil || len(out["entries"].([]any)) != 1 {
		t.Fatal("a re-mint mid-read failed the read:", out, err)
	}
	sent := f.api.graphqlCount() - before
	// Scope, the refused page, the new token's viewer read and the page
	// again: more requests than the cap, in two GraphQL calls.
	if f.api.mints != mints+1 || sent != 4 || sent <= historyCallCap || f.api.unauthorizedHistory != 0 {
		t.Fatal("re-mint", f.api.mints-mints, "requests", sent)
	}
	if got := f.budget().By.People - charged; got != sent {
		t.Fatal("charged", got, "of", sent)
	}
	_, err = f.history(f.identity(), "", "", map[string]any{"issue_uuid": awForeignIssue})
	if code, _ := serviceError(err); code != "unavailable" || strings.Contains(err.Error(), "no issue UUID") {
		t.Fatal("a failed read was not unavailable:", err)
	}
	_, err = f.history(f.identity(), "", "", map[string]any{"issue_uuid": awMissingIssue})
	refusedWith(t, err, "invalid", "no issue UUID in this binding")
}

// A request whose re-mints and re-sends made more than historyChargeCap
// counted calls is charged exactly the cap, to whoever asked.
func TestPostgresIssueHistoryChargeIsCapped(t *testing.T) {
	f := newAWFixture(t, awOrg)
	access := f.must(bus.Request{Operation: "taskboard_access", Workspace: f.workspace, Project: "p", Args: map[string]any{"ledger": 1}})
	pid := access.Data.(map[string]any)["project_id"].(string)
	var calls atomic.Int64
	calls.Store(historyChargeCap + 3)
	f.writer.chargeHistory(context.Background(), pid, false, &calls)
	if b := f.budget(); b.By.People != historyChargeCap || !historyKind(b, historyChargeCap) {
		t.Fatalf("charged %+v; want exactly %d", b, historyChargeCap)
	}
	calls.Store(historyChargeCap + 3)
	f.writer.chargeHistory(context.Background(), pid, true, &calls)
	if b := f.budget(); b.By.Agents != historyChargeCap || !historyKind(b, 2*historyChargeCap) {
		t.Fatalf("charged %+v; want %d to agents", b, historyChargeCap)
	}
}
