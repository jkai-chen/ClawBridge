package bus

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Only the project owner sets the project's watch budget; every member
// reads it.
func TestPostgresProjectWatchBudget(t *testing.T) {
	f := newPGIdentityFixture(t)
	people := map[string]string{}
	for _, role := range []string{"reader", "member", "lead"} {
		people[role] = newID(role) + "@identity-test.example"
		f.member(people[role])
		f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": people[role], "role": role}})
	}
	set := Request{Operation: "comment_watch_budget_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"watch_budget": 50}}
	for _, role := range []string{"reader", "member", "lead"} {
		if _, err := f.as(people[role], "sub-"+role, set); err == nil || !strings.Contains(err.Error(), "owner") {
			t.Fatalf("a %s set the watch budget: %v", role, err)
		}
	}
	f.must(f.owner, "sub-owner", set)
	got := f.must(people["reader"], "sub-reader", Request{Operation: "comment_watch_status", Workspace: f.workspace, Project: "p", Args: map[string]any{}}).Data.(map[string]any)
	if got["watch_budget"] != 50 || got["budget_limit"] != 50 || got["budget_source"] != "project" {
		t.Fatal(got)
	}
	f.must(f.owner, "sub-owner", Request{Operation: "comment_watch_budget_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"watch_budget": nil}})
	got = f.must(f.owner, "sub-owner", Request{Operation: "comment_watch_status", Workspace: f.workspace, Project: "p", Args: map[string]any{}}).Data.(map[string]any)
	if got["watch_budget"] != nil || got["budget_source"] == "project" {
		t.Fatal(got)
	}
}

// comment_watch_status shows the calls used this hour, agent writes
// included.
func TestPostgresWatchStatusShowsBudgetUsed(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	status := func() map[string]any {
		return f.must(f.owner, "sub-owner", Request{Operation: "comment_watch_status", Workspace: f.workspace, Project: "p", Args: map[string]any{}}).Data.(map[string]any)
	}
	if got := status(); got["budget_used"] != 0 || got["budget_source"] != "default" || got["budget_limit"] != LinearBudgetDefault {
		t.Fatal(got)
	}
	var pid string
	if err := f.s.db.QueryRow(`SELECT id FROM projects WHERE workspace_id=(SELECT id FROM workspaces WHERE name=$1) AND slug='p'`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ChargeLinearBudget(ctx, pid, 7, "app_write.update"); err != nil {
		t.Fatal(err)
	}
	if got := status(); got["budget_used"] != 7 {
		t.Fatal("the charged calls are not shown:", got)
	}
	b, err := f.s.LinearBudgetOf(ctx, pid)
	if err != nil || b.Used != 7 || b.Limit != 0 || b.Capped || b.Source != "default" || !b.ResetsAt.After(time.Now()) {
		t.Fatalf("%+v %v", b, err)
	}
	// The write's calls are its kind's, one event; a second adds to it.
	if err = f.s.ChargeLinearBudget(ctx, pid, 9, "app_write.comment"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.ChargeLinearBudget(ctx, pid, 9, "app_write.comment"); err != nil {
		t.Fatal(err)
	}
	if b, err = f.s.LinearBudgetOf(ctx, pid); err != nil || b.Used != 25 || len(b.Kinds) != 2 || b.Kinds[0] != (LinearBudgetKind{Kind: "app_write.comment", Calls: 18, Events: 2}) || b.Kinds[1] != (LinearBudgetKind{Kind: "app_write.update", Calls: 7, Events: 1}) {
		t.Fatalf("by kind: %+v %v", b, err)
	}
	if got := status(); len(got["kinds"].([]LinearBudgetKind)) != 2 {
		t.Fatal("status lacks the kinds:", got)
	}
	if kinds, err := f.s.LinearBudgetKindsAt(ctx, pid, b.ResetsAt.Add(-time.Hour).Format("2006-01-02T15")); err != nil || len(kinds) != 2 {
		t.Fatal(kinds, err)
	}
	f.must(f.owner, "sub-owner", Request{Operation: "comment_watch_budget_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"watch_budget": 50}})
	if b, err = f.s.LinearBudgetOf(ctx, pid); err != nil || b.Limit != 50 || !b.Capped || b.Source != "project" {
		t.Fatalf("the project's own budget: %+v %v", b, err)
	}
	// Calls charged in an earlier hour are not this hour's.
	if _, err := f.s.db.ExecContext(ctx, `UPDATE watch_lease SET budget_hour='2000-01-01T00' WHERE project_id=$1`, pid); err != nil {
		t.Fatal(err)
	}
	if got := status(); got["budget_used"] != 0 {
		t.Fatal("an earlier hour's calls are shown:", got)
	}
}

// Only the serving store names its connections: OpenPostgres (tests, tools)
// and OpenPostgresOperator (operator commands) leave application_name
// alone, so doctor never counts them as servers; a DSN's own name wins.
func TestPostgresOnlyServeNamesItsConnections(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CLAWBRIDGE_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	name := func(s *Store) string {
		t.Helper()
		var n string
		if err := s.db.QueryRowContext(ctx, `SELECT current_setting('application_name')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	served, err := OpenPostgresServer(ctx, dsn, "owner@instances-test.example")
	if err != nil {
		t.Fatal(err)
	}
	defer served.Close()
	if n := name(served); !strings.HasPrefix(n, ServerInstancePrefix) || len(n) != len(ServerInstancePrefix)+16 || n != served.instance {
		t.Fatal("the serving store's name:", n)
	}
	if got, err := served.LiveServerInstances(ctx); err != nil || got.Live < 1 {
		t.Fatalf("the serving store does not see itself: %+v %v", got, err)
	}
	plain, err := OpenPostgres(ctx, dsn, "owner@instances-test.example")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	operator, err := OpenPostgresOperator(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	for _, s := range []*Store{plain, operator} {
		if n := name(s); strings.HasPrefix(n, ServerInstancePrefix) {
			t.Fatal("a store that doesn't serve is named as a server:", n)
		}
		if got, _ := s.LiveServerInstances(ctx); got != (ServerInstances{}) {
			t.Fatal("a store that doesn't serve counts servers:", got)
		}
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	own, err := OpenPostgresServer(ctx, dsn+sep+"application_name=operator-chosen", "owner@instances-test.example")
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if n := name(own); n != "operator-chosen" {
		t.Fatal("the DSN's own name was replaced:", n)
	}
	if got, _ := own.LiveServerInstances(ctx); got.Known {
		t.Fatal("a store without its own name claims a count:", got)
	}
}
