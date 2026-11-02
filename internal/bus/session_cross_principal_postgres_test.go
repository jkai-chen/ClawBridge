package bus

import (
	"context"
	"os"
	"testing"
)

// Two real members of one hosted project: bob can neither register over nor
// act as alice's session, even holding her session name and real instance.
func TestPostgresAnotherPrincipalCannotRegisterOverOrActAsASession(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner, alice, bob := "owner@xprin-test.example", "alice@xprin-test.example", "bob@xprin-test.example"
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("unit_")
	exec := func(principal, op, session string, a map[string]any) (any, error) {
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
	if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "workspace_init", Workspace: w, Args: map[string]any{"mode": "server"}}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "project_create", Workspace: w, Args: map[string]any{"slug": "p"}}); e != nil {
		t.Fatal(e)
	}
	for _, principal := range []string{alice, bob} {
		if _, e := s.ExecuteAs(ctx, owner, Request{Operation: "project_role_set", Workspace: w, Project: "p", Args: map[string]any{"principal": principal, "role": "member"}}); e != nil {
			t.Fatal(e)
		}
	}
	must(alice, "register_session", "alice-s", map[string]any{"instance": "alice-inst", "role": "worker", "notes": "alice notes"})
	must(bob, "register_session", "bob-s", map[string]any{"instance": "bob-inst", "role": "worker"})
	// Read the row directly: whoami naming the instance is itself a heartbeat
	// read and would move last_seen. Start from a fixed old last_seen and
	// updated_at, which any accepted call by alice would move.
	var pid string
	if e := s.db.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, w).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	const old = "2000-01-01T00:00:00Z"
	if _, e := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen=$1,updated_at=$1 WHERE project_id=$2 AND name='alice-s'`, old, pid); e != nil {
		t.Fatal(e)
	}
	read := func() map[string]any {
		t.Helper()
		var owner, instance, role, notes, lastSeen, updatedAt, aliveUntil, aliveState string
		if e := s.db.QueryRowContext(ctx, `SELECT owner_principal,instance,COALESCE(role,''),COALESCE(notes,''),last_seen,updated_at,COALESCE(alive_until,''),COALESCE(alive_state,'') FROM sessions WHERE project_id=$1 AND name='alice-s'`, pid).Scan(&owner, &instance, &role, &notes, &lastSeen, &updatedAt, &aliveUntil, &aliveState); e != nil {
			t.Fatal(e)
		}
		return map[string]any{"owner_principal": owner, "instance": instance, "role": role, "notes": notes,
			"last_seen": lastSeen, "updated_at": updatedAt, "alive_until": aliveUntil, "alive_state": aliveState}
	}
	before := read()
	if before["owner_principal"] != alice || before["instance"] != "alice-inst" || before["role"] != "worker" || before["notes"] != "alice notes" || before["last_seen"] != old {
		t.Fatal("setup", before)
	}

	if _, e := exec(bob, "register_session", "alice-s", map[string]any{"role": "worker"}); e == nil {
		t.Fatal("bob registered over alice's active session")
	} else if code := errorCode(t, e); code != "conflict" {
		t.Fatalf("code %q, want conflict", code)
	}
	if _, e := exec(bob, "register_session", "alice-s", map[string]any{"instance": "alice-inst", "resume": true}); e == nil {
		t.Fatal("bob resumed alice's session")
	} else if code := errorCode(t, e); code != "conflict" {
		t.Fatalf("code %q, want conflict", code)
	}
	for op, a := range map[string]map[string]any{
		"heartbeat":      {},
		"inbox":          {},
		"update_session": {"notes": "hijacked"},
		"send":           {"to": "bob-s", "body": "as alice", "operation_id": "xprin-1"},
	} {
		a["instance"] = "alice-inst"
		_, e := exec(bob, op, "alice-s", a)
		if code := errorCode(t, e); code != "forbidden" {
			t.Fatalf("%s as bob on alice's session: code %q, want forbidden", op, code)
		}
	}
	// The refused send must not have delivered anything to bob-s.
	for _, m := range must(bob, "inbox", "bob-s", map[string]any{"instance": "bob-inst", "unread_only": false})["messages"].([]map[string]any) {
		if m["sender"] == "alice-s" || m["body"] == "as alice" {
			t.Fatalf("a refused send reached bob-s's inbox: %v", m)
		}
	}
	// Control: alice's own session with a stale instance is the other refusal.
	_, e := exec(alice, "update_session", "alice-s", map[string]any{"instance": "alice-stale", "notes": "stale"})
	if code := errorCode(t, e); code != "invalid" {
		t.Fatalf("stale instance: code %q, want invalid", code)
	}
	for _, k := range []string{"owner_principal", "instance", "role", "notes", "last_seen", "updated_at", "alive_until", "alive_state"} {
		if after := read(); after[k] != before[k] {
			t.Fatalf("alice's session %s changed: %v -> %v", k, before[k], after[k])
		}
	}
}
