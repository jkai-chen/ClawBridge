package bus

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestPostgresAssignmentAuthorityIsBounded(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner := "owner@assign-test.example"
	people := map[string]string{"lead": "lead@assign-test.example", "member": "member@assign-test.example", "reader": "reader@assign-test.example"}
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("unit_")
	exec := func(principal, op, session string, a map[string]any) (any, error) {
		a["instance"] = session + "-inst"
		r, e := s.ExecuteAs(ctx, principal, Request{Operation: op, Workspace: w, Project: "p", Session: session, Args: a})
		return r.Data, e
	}
	must := func(principal, op, session string, a map[string]any) map[string]any {
		t.Helper()
		data, e := exec(principal, op, session, a)
		if e != nil {
			t.Fatalf("%s %s as %s: %v", op, session, principal, e)
		}
		m, _ := data.(map[string]any)
		return m
	}
	forbidden := func(e error) bool {
		var coded interface{ Code() string }
		return errors.As(e, &coded) && coded.Code() == "forbidden"
	}
	if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "workspace_init", Workspace: w, Args: map[string]any{"mode": "server"}}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "project_create", Workspace: w, Args: map[string]any{"slug": "p"}}); e != nil {
		t.Fatal(e)
	}
	must(owner, "register_session", "boss", map[string]any{})
	for name, principal := range people {
		role := name
		if name == "reader" {
			role = "member" // Register first; readers cannot register.
		}
		if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "project_role_set", Workspace: w, Project: "p", Args: map[string]any{"principal": principal, "role": role}}); e != nil {
			t.Fatal(e)
		}
		must(principal, "register_session", name, map[string]any{})
	}
	if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "project_role_set", Workspace: w, Project: "p", Args: map[string]any{"principal": people["reader"], "role": "reader"}}); e != nil {
		t.Fatal(e)
	}
	// Readers propose nothing; members cannot designate a coordinator.
	if _, e := exec(people["reader"], "assignment_propose", "reader", map[string]any{"issue_uuid": issueOne}); !forbidden(e) {
		t.Fatal("reader proposed:", e)
	}
	if _, e := exec(people["member"], "coordinator_set", "member", map[string]any{"target_session": "member"}); !forbidden(e) {
		t.Fatal("member designated a coordinator:", e)
	}
	// A lead may not designate the owner's session, but may designate its own.
	if _, e := exec(people["lead"], "coordinator_set", "lead", map[string]any{"target_session": "boss"}); !forbidden(e) {
		t.Fatal("lead designated the owner's session:", e)
	}
	must(people["lead"], "coordinator_set", "lead", map[string]any{"target_session": "lead"})
	member := must(people["member"], "assignment_propose", "member", map[string]any{"issue_uuid": issueOne})["assignment"].(map[string]any)
	boss := must(owner, "assignment_propose", "boss", map[string]any{"issue_uuid": issueTwo})["assignment"].(map[string]any)
	// The lead coordinator confirms a member's assignment, never the owner's.
	must(people["lead"], "assignment_confirm", "lead", map[string]any{"target_session": "member", "revision": member["revision"]})
	if _, e := exec(people["lead"], "assignment_confirm", "lead", map[string]any{"target_session": "boss", "revision": boss["revision"]}); !forbidden(e) {
		t.Fatal("lead confirmed the owner's assignment:", e)
	}
	// The owner is not the coordinator, so even the owner cannot confirm.
	if _, e := exec(owner, "assignment_confirm", "boss", map[string]any{"revision": boss["revision"]}); !forbidden(e) {
		t.Fatal("non-coordinator owner confirmed:", e)
	}
	// Revoking the lead retires its session and clears the designation.
	if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "project_role_revoke", Workspace: w, Project: "p", Args: map[string]any{"principal": people["lead"]}}); e != nil {
		t.Fatal(e)
	}
	who := must(owner, "whoami", "boss", map[string]any{})
	if who["project"].(map[string]any)["coordinator_session"] != nil {
		t.Fatal("revoked lead kept the coordinator designation")
	}
	listed := must(owner, "list_sessions", "boss", map[string]any{"issue": issueOne})["sessions"].([]map[string]any)
	if len(listed) != 1 || listed[0]["name"] != "member" || listed[0]["assignment"].(map[string]any)["state"] != "confirmed" {
		t.Fatal(listed)
	}
}
