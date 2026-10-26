package bus

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A4: resuming project flow is the owner's or a lead's; a member's is
// refused; turning it on stays the owner's (the whole policy).
func TestPostgresProjectFlowResumeIsTheOwnersOrALeads(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner, lead, member := "owner@flow-test.example", "lead@flow-test.example", "member@flow-test.example"
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetProjectFlowProven(true)
	w := newID("unit_")
	exec := func(principal, op string, a map[string]any) error {
		_, e := s.ExecuteAs(ctx, principal, Request{Operation: op, Workspace: w, Project: "p", Args: a})
		return e
	}
	must := func(principal, op string, a map[string]any) {
		t.Helper()
		if e := exec(principal, op, a); e != nil {
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
	must(owner, "taskboard_set_v2", taskboardArgsV2(0, projectOne))
	on := map[string]any{"enabled": true, "teams": map[string]any{}, "project_flow": true}
	if e := exec(lead, "status_policy_set", map[string]any{"expected_revision": 0, "status_policy": on}); e == nil || !strings.Contains(e.Error(), "owner") {
		t.Fatal("a lead turned project flow on:", e)
	}
	must(owner, "status_policy_set", map[string]any{"expected_revision": 0, "status_policy": on})
	var pid string
	if e := s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE workspace_id=(SELECT id FROM workspaces WHERE name=$1) AND slug='p'`, w).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	hold := func() {
		t.Helper()
		if _, e := s.db.ExecContext(ctx, `INSERT INTO project_flow_state(project_id,linear_project_id,held_since,held_type) VALUES($1,$2,'2026-10-26T00:00:00Z','backlog') ON CONFLICT (project_id,linear_project_id) DO UPDATE SET held_since=EXCLUDED.held_since`, pid, projectOne); e != nil {
			t.Fatal(e)
		}
	}
	hold()
	if e := exec(member, "project_flow_resume", map[string]any{"linear_project": projectOne}); e == nil || !strings.Contains(e.Error(), "owner or lead") {
		t.Fatal("a member resumed:", e)
	}
	must(lead, "project_flow_resume", map[string]any{"linear_project": projectOne})
	hold()
	must(owner, "project_flow_resume", map[string]any{"linear_project": projectOne})
}
