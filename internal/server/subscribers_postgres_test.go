package server

import (
	"context"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

const (
	subIssueOne = "00000000-0000-4000-8000-0000000000e1"
	subIssueTwo = "00000000-0000-4000-8000-0000000000e2"
)

func subscriptionRows(t *testing.T, f *awFixture) map[string]string {
	t.Helper()
	rows, err := f.store.ExecuteAsIdentity(context.Background(), f.identity(), bus.Request{Operation: "issue_subscribers_get", Workspace: f.workspace, Project: "p", Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, v := range rows.Data.(map[string]any)["rows"].(map[string]int) {
		if v > 0 {
			out[k] = strings.Repeat("x", v)
		}
	}
	return out
}

// An issue the app created is queued for its subscribers once the server
// is proven, and never before (B2); a receipt that names no issue queues
// nothing.
func TestPostgresAppCreateQueuesSubscribers(t *testing.T) {
	f := newAWFixture(t, awOrg)
	scope := bus.Request{Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i"}}
	ctx := context.Background()
	f.writer.queueSubscribers(ctx, f.identity(), scope, "create", linearboard.ReceiptV2{Kind: "issue", Outcome: "accepted", ID: subIssueOne})
	if got := subscriptionRows(t, f); len(got) != 0 {
		t.Fatal("queued unproven:", got)
	}
	f.store.SetIssueSubscribersProven(true)
	// An update or a comment queues nothing.
	f.writer.queueSubscribers(ctx, f.identity(), scope, "update", linearboard.ReceiptV2{Kind: "issue", Outcome: "accepted", ID: subIssueTwo})
	f.writer.queueSubscribers(ctx, f.identity(), scope, "create", linearboard.ReceiptV2{Kind: "issue", Outcome: "accepted", ID: subIssueOne})
	f.writer.queueSubscribers(ctx, f.identity(), scope, "create", linearboard.ReceiptV2{Kind: "issue", Outcome: "accepted", ID: subIssueOne})
	f.writer.queueSubscribers(ctx, f.identity(), scope, "create", linearboard.ReceiptV2{Kind: "issue", Outcome: "accepted"})
	if got := subscriptionRows(t, f); got["queued"] != "x" {
		t.Fatal(got)
	}
}

// The backfill: a dry run lists the app's created issues in the bound teams
// since the date, and the claims, by identifier, and queues nothing;
// --apply queues each once. A session, a member, and a setting that is off
// are refused.
func TestPostgresSubscribersBackfill(t *testing.T) {
	f := newAWFixture(t, awOrg)
	f.store.SetIssueSubscribersProven(true)
	f.api.backfill = []any{map[string]any{"id": strings.ToUpper(subIssueOne), "identifier": "FIX-1", "team": map[string]any{"id": awTeam}}, map[string]any{"id": subIssueTwo, "identifier": "FIX-2", "team": map[string]any{"id": awTeam}}}
	run := func(session string, args map[string]any) (map[string]any, error) {
		r, err := f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: "linear_app_subscribers_backfill", Workspace: f.workspace, Project: "p", Session: session, Args: args})
		if err != nil {
			return nil, err
		}
		return awJSON(t, r.Data), nil
	}
	if _, err := run(awSession, map[string]any{"instance": awSession + "-i", "since": "2026-10-27T00:00:00Z"}); err == nil {
		t.Fatal("a session backfilled")
	}
	member := awUnique("member") + "@appwrite-test.example"
	f.must(bus.Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": member, "role": "member"}})
	if _, err := f.writer.Execute(context.Background(), bus.Identity{Principal: member, Issuer: bus.GoogleIssuer, Subject: "sub-member"}, bus.Request{Operation: "linear_app_subscribers_backfill", Workspace: f.workspace, Project: "p", Args: map[string]any{"since": "2026-10-27T00:00:00Z"}}); err == nil || !strings.Contains(err.Error(), "lead or owner") {
		t.Fatal("a member backfilled:", err)
	}
	if _, err := run("", map[string]any{"since": "yesterday"}); err == nil {
		t.Fatal("a bad since")
	}
	dry, err := run("", map[string]any{"since": "2026-10-27T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if dry["count"] != float64(2) || dry["applied"] != false || dry["read_calls"] != float64(1) || dry["subscribe_calls"] != float64(4) || len(subscriptionRows(t, f)) != 0 {
		t.Fatalf("%+v", dry)
	}
	if v := f.api.backfillVars[0]; v["app"] != awAppUser || v["since"] != "2026-10-27T00:00:00Z" || !strings.Contains(strings.ToLower(strings.Join(toStrings(v["teams"]), ",")), strings.ToLower(awTeam)) {
		t.Fatalf("%+v", v)
	}
	if strings.Contains(strings.Join(toStrings(dry["issues"]), " "), "@") {
		t.Fatal("an email in the dry run")
	}
	applied, err := run("", map[string]any{"since": "2026-10-27T00:00:00Z", "apply": true})
	if err != nil || applied["queued"] != float64(2) {
		t.Fatalf("%+v %v", applied, err)
	}
	if again, _ := run("", map[string]any{"since": "2026-10-27T00:00:00Z", "apply": true}); again["queued"] != float64(0) {
		t.Fatal("queued twice:", again)
	}
	// Off: the dry run still lists, as a plain read; --apply is refused
	// before anything is read. Not proven on the server: the same.
	f.must(bus.Request{Operation: "issue_subscribers_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": 0, "enabled": false}})
	for _, proven := range []bool{true, false} {
		f.store.SetIssueSubscribersProven(proven)
		if dry, err = run("", map[string]any{"since": "2026-10-27T00:00:00Z"}); err != nil || dry["count"] != float64(2) || dry["applied"] != false {
			t.Fatalf("a dry run refused with the setting off (proven %v): %+v %v", proven, dry, err)
		}
		reads := len(f.api.backfillVars)
		if _, err = run("", map[string]any{"since": "2026-10-27T00:00:00Z", "apply": true}); err == nil || !strings.Contains(err.Error(), "off for this project") || len(f.api.backfillVars) != reads {
			t.Fatal("an apply with the setting off:", err)
		}
	}
	// On but not proven: --apply is refused too.
	f.store.SetIssueSubscribersProven(true)
	f.must(bus.Request{Operation: "issue_subscribers_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": 1, "enabled": true}})
	f.store.SetIssueSubscribersProven(false)
	if _, err = run("", map[string]any{"since": "2026-10-27T00:00:00Z", "apply": true}); err == nil || !strings.Contains(err.Error(), "not yet proven") {
		t.Fatal("an apply on an unproven server:", err)
	}
}

func toStrings(v any) []string {
	out := []string{}
	if list, ok := v.([]any); ok {
		for _, x := range list {
			switch y := x.(type) {
			case string:
				out = append(out, y)
			case map[string]any:
				for _, z := range y {
					if s, ok := z.(string); ok {
						out = append(out, s)
					}
				}
			}
		}
	}
	return out
}

// The backfill lists only the binding's own issues: a team board's bound
// teams; a project set's own projects, so a project another binding holds
// in the same team is never listed (V037 §4.4). Linear's filter carries
// the projects, and each row is checked again.
func TestPostgresSubscribersBackfillStaysInTheBinding(t *testing.T) {
	const (
		projectMine  = "c7000000-0000-4000-8000-0000000000e1"
		projectOther = "c7000000-0000-4000-8000-0000000000e2"
		otherTeam    = "c7000000-0000-4000-8000-0000000000c9"
		issueMine    = "c7000000-0000-4000-8000-0000000000f1"
		issueOther   = "c7000000-0000-4000-8000-0000000000f2"
		issueBare    = "c7000000-0000-4000-8000-0000000000f3"
		issueAway    = "c7000000-0000-4000-8000-0000000000f4"
	)
	f := newAWFixture(t, awOrg)
	f.store.SetIssueSubscribersProven(true)
	f.api.backfill = []any{
		map[string]any{"id": issueMine, "identifier": "FIX-1", "team": map[string]any{"id": awTeam}, "project": map[string]any{"id": projectMine}},
		map[string]any{"id": issueOther, "identifier": "FIX-2", "team": map[string]any{"id": strings.ToUpper(awTeam)}, "project": map[string]any{"id": projectOther}},
		map[string]any{"id": issueBare, "identifier": "FIX-3", "team": map[string]any{"id": awTeam}, "project": nil},
		map[string]any{"id": issueAway, "identifier": "AWAY-1", "team": map[string]any{"id": otherTeam}, "project": map[string]any{"id": projectMine}},
	}
	run := func() []string {
		t.Helper()
		r, err := f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: "linear_app_subscribers_backfill", Workspace: f.workspace, Project: "p", Args: map[string]any{"since": "2026-10-27T00:00:00Z"}})
		if err != nil {
			t.Fatal(err)
		}
		return toStrings(awJSON(t, r.Data)["issues"])
	}
	// A team board: the bound team's, any project or none; another team's
	// never. The query names no projects.
	got := strings.Join(run(), " ")
	if !strings.Contains(got, "FIX-1") || !strings.Contains(got, "FIX-2") || !strings.Contains(got, "FIX-3") || strings.Contains(got, "AWAY-1") {
		t.Fatal(got)
	}
	if q, v := f.api.backfillQueries[0], f.api.backfillVars[0]; q != "BackfillIssues" || v["projects"] != nil {
		t.Fatal(q, v)
	}
	// A project set: its own project's only.
	f.must(bus.Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{
		"instance": awSession + "-i", "expected_revision": 1, "mode": "project_set", "linear_team_ids": []string{awTeam},
		"linear_default_team_id": awTeam, "linear_project_ids": []string{projectMine}, "linear_workspace_id": awOrg,
	}})
	got = strings.Join(run(), " ")
	if !strings.Contains(got, "FIX-1") || strings.Contains(got, "FIX-2") || strings.Contains(got, "FIX-3") || strings.Contains(got, "AWAY-1") {
		t.Fatal("outside the binding's projects:", got)
	}
	if q, v := f.api.backfillQueries[1], f.api.backfillVars[1]; q != "BackfillProjectIssues" || strings.Join(toStrings(v["projects"]), ",") != projectMine {
		t.Fatal(q, v)
	}
}
