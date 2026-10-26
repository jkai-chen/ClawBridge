package bus

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A3 (§10.7): the owner or a lead sets who closes, either way; a member
// cannot; the whole status policy stays the owner's.
func TestPostgresClosesSetIsTheOwnersOrALeads(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner, lead, member := "owner@closes-test.example", "lead@closes-test.example", "member@closes-test.example"
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("unit_")
	exec := func(principal, op string, a map[string]any) (any, error) {
		r, e := s.ExecuteAs(ctx, principal, Request{Operation: op, Workspace: w, Project: "p", Args: a})
		return r.Data, e
	}
	must := func(principal, op string, a map[string]any) {
		t.Helper()
		if _, e := exec(principal, op, a); e != nil {
			t.Fatalf("%s as %s: %v", op, principal, e)
		}
	}
	if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "workspace_init", Workspace: w, Args: map[string]any{"mode": "server"}}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "project_create", Workspace: w, Args: map[string]any{"slug": "p"}}); e != nil {
		t.Fatal(e)
	}
	must(owner, "project_role_set", map[string]any{"principal": lead, "role": "lead"})
	must(owner, "project_role_set", map[string]any{"principal": member, "role": "member"})
	must(owner, "taskboard_set_v2", map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": []string{cpOne, cpTwo}})
	must(owner, "status_policy_set", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)})
	set := func(principal string, rev int, closes, via string) error {
		_, e := exec(principal, "status_closes_set", map[string]any{"expected_revision": rev, "linear_project": cpOne, "closes": closes, "via": via})
		return e
	}
	if e := set(member, 1, "human", "cli"); e == nil || !strings.Contains(e.Error(), "owner or lead") {
		t.Fatal("a member set who closes:", e)
	}
	if e := set(lead, 1, "agent", "cli-tty"); e != nil {
		t.Fatal("a lead loosening:", e)
	}
	if e := set(lead, 2, "human", "cli"); e != nil {
		t.Fatal("a lead tightening:", e)
	}
	if e := set(owner, 3, "agent", "cli-tty"); e != nil {
		t.Fatal("the owner:", e)
	}
	// L1: the whole policy, and so the minimal one, stays the owner's.
	if _, e := exec(lead, "status_policy_set", map[string]any{"expected_revision": 4, "status_policy": sfPolicy(false)}); e == nil || !strings.Contains(e.Error(), "owner") {
		t.Fatal("a lead set the whole policy:", e)
	}
}
