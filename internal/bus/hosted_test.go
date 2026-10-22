package bus

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestPostgresRoleDenialHasNoSideEffects(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner, member, reader := "owner@bus-test.example", "member@bus-test.example", "reader@bus-test.example"
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("unit_")
	call := func(principal, op, session string, a map[string]any) Result {
		t.Helper()
		result, e := s.ExecuteAs(ctx, principal, Request{Operation: op, Workspace: w, Project: "p", Session: session, Args: a})
		if e != nil {
			t.Fatalf("%s: %v", op, e)
		}
		return result
	}
	call(owner, "workspace_init", "", map[string]any{"mode": "server"})
	call(owner, "project_create", "", map[string]any{"slug": "p"})
	projects := call(owner, "list_projects", "", nil).Data.(map[string]any)
	if len(projects["projects"].([]map[string]any)) != 1 || projects["projects"].([]map[string]any)[0]["role"] != "owner" {
		t.Fatal("workspace owner did not see project role")
	}
	call(owner, "project_role_set", "", map[string]any{"principal": member, "role": "member"})
	call(owner, "project_role_set", "", map[string]any{"principal": reader, "role": "member"})
	call(member, "register_session", "worker", map[string]any{"instance": "worker-inst", "role": "lead"})
	call(reader, "register_session", "reader", map[string]any{"instance": "reader-inst"})
	call(owner, "project_role_set", "", map[string]any{"principal": reader, "role": "reader"})
	members := call(owner, "list_members", "", nil).Data.(map[string]any)
	if len(members["members"].([]map[string]any)) != 3 {
		t.Fatal("project member listing omitted assignments")
	}
	before := call(owner, "history", "", nil)
	for _, tc := range []struct {
		principal, op, session string
		args                   map[string]any
	}{
		{reader, "send", "reader", map[string]any{"instance": "reader-inst", "to": "worker", "body": "denied", "operation_id": "deny"}},
		{reader, "register_session", "new", nil},
		{member, "update_session", "worker", map[string]any{"instance": "worker-inst", "target_session": "reader", "notes": "routing lead is not admin"}},
		{"outsider@bus-test.example", "list_sessions", "", nil},
	} {
		_, e := s.ExecuteAs(ctx, tc.principal, Request{Operation: tc.op, Workspace: w, Project: "p", Session: tc.session, Args: tc.args})
		var coded interface{ Code() string }
		if !errors.As(e, &coded) || coded.Code() != "forbidden" {
			t.Fatalf("%s: expected forbidden, got %v", tc.op, e)
		}
	}
	after := call(owner, "history", "", nil)
	if asJSON(before) != asJSON(after) {
		t.Fatal("authorization denials changed audit")
	}
	snap := call(reader, "snapshot", "reader", map[string]any{"instance": "reader-inst"})
	if snap.Source != "server" {
		t.Fatal("wrong hosted source")
	}
	info := snap.Data.(map[string]any)
	if info["principal"] != reader || info["verified"] != true {
		t.Fatal("wrong per-call identity")
	}
}
func TestDialectBindsOnlyPlaceholders(t *testing.T) {
	tr := &transaction{postgres: true}
	got := tr.bind(`SELECT '?' AS literal, 'it''s ?' AS quoted FROM sessions WHERE id=? AND name=?`)
	if got != `SELECT '?' AS literal, 'it''s ?' AS quoted FROM sessions WHERE id=$1 AND name=$2` {
		t.Fatal(got)
	}
	if got = tr.bind(`SELECT rowid AS sequence,* FROM sessions WHERE rowid>? ORDER BY rowid`); got != `SELECT * FROM sessions WHERE sequence>$1 ORDER BY sequence` {
		t.Fatal(got)
	}
}
func TestTrustedIdentityAPICannotOverrideLocalPrincipal(t *testing.T) {
	h := newHarness(t)
	before := h.s.principal
	if _, e := h.s.ExecuteAs(context.Background(), "forged@example.test", Request{Operation: "whoami"}); e == nil {
		t.Fatal("local identity accepted trusted-server override")
	}
	if h.s.principal != before {
		t.Fatal("Store identity mutated")
	}
}

func TestHostedAuditUsesFingerprints(t *testing.T) {
	original := map[string]any{"instance": "other-principal-token", "nested": map[string]any{"prior_instance": "old-token"}, "role": "member"}
	filtered := hostedAuditDetails(original).(map[string]any)
	if _, ok := filtered["instance"]; ok {
		t.Fatal("audit exports resumable instance")
	}
	if filtered["instance_fingerprint"] != hashPayload("other-principal-token") {
		t.Fatal("audit fingerprint lost")
	}
	if original["instance"] != "other-principal-token" {
		t.Fatal("audit redaction mutated caller data")
	}
}
func TestPostgresSessionNameCannotImpersonateSessionID(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner := "owner@bus-test.example"
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("unit_")
	exec := func(op, session string, a map[string]any) (Result, error) {
		return s.ExecuteAs(ctx, owner, Request{Operation: op, Workspace: w, Project: "p", Session: session, Args: a})
	}
	call := func(op, session string, a map[string]any) map[string]any {
		t.Helper()
		r, e := exec(op, session, a)
		if e != nil {
			t.Fatalf("%s: %v", op, e)
		}
		return r.Data.(map[string]any)
	}
	call("workspace_init", "", map[string]any{"mode": "server"})
	call("project_create", "", map[string]any{"slug": "p"})
	sessions := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		sessions[name] = call("register_session", name, map[string]any{"instance": name + "-inst"})["id"].(string)
	}
	victim := sessions["b"]
	if _, e := exec("register_session", victim, map[string]any{"instance": "impostor-inst"}); e == nil {
		t.Fatal("ID-shaped session name registered")
	}
	if _, e := s.db.ExecContext(ctx, `UPDATE sessions SET name=$1 WHERE id=$2`, victim, sessions["c"]); e != nil {
		t.Fatal(e)
	}
	sent := call("send", "a", map[string]any{"instance": "a-inst", "to": victim, "body": "to b", "operation_id": "before"})
	if d := sent["deliveries"].([]map[string]any); len(d) != 1 || d[0]["recipient_id"] != victim {
		t.Fatal("ID address was ambiguous or reached the impostor", d)
	}
	question := call("send", "b", map[string]any{"instance": "b-inst", "to": "a", "body": "question", "operation_id": "question"})
	call("retire_session", "b", map[string]any{"instance": "b-inst"})
	for _, tc := range []struct {
		op string
		a  map[string]any
	}{
		{"send", map[string]any{"instance": "a-inst", "to": victim, "body": "after", "operation_id": "after"}},
		{"reply", map[string]any{"instance": "a-inst", "message_id": question["id"], "body": "answer", "operation_id": "reply"}},
	} {
		if _, e := exec(tc.op, "a", tc.a); e == nil {
			t.Fatalf("%s to a retired session ID reached the impostor name", tc.op)
		}
	}
}
func TestPrincipalGrantsNormaliseToTokenForm(t *testing.T) {
	for in, want := range map[string]string{"Bob@X.com": "bob@x.com", " Owner@Example.COM\t": "owner@example.com", "bob@x.com": "bob@x.com"} {
		if got := normalPrincipal(in); got != want || !validPrincipal(got) {
			t.Fatalf("normalPrincipal(%q) = %q, want valid %q", in, got, want)
		}
	}
}
func TestPostgresMixedCaseGrantMatchesTokenPrincipal(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	// Operators and owners may type mixed case; the server always executes as
	// the lowercased verified token email.
	s, err := OpenPostgres(ctx, dsn, " Owner@Bus-Test.example ")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("unit_")
	call := func(principal, op, session string, a map[string]any) {
		t.Helper()
		if _, e := s.ExecuteAs(ctx, principal, Request{Operation: op, Workspace: w, Project: "p", Session: session, Args: a}); e != nil {
			t.Fatalf("%s as %s: %v", op, principal, e)
		}
	}
	owner, bob := "owner@bus-test.example", "bob@bus-test.example"
	call(owner, "workspace_init", "", map[string]any{"mode": "server"})
	call(owner, "project_create", "", map[string]any{"slug": "p"})
	call(owner, "project_role_set", "", map[string]any{"principal": "Bob@Bus-Test.example", "role": "member"})
	call(bob, "register_session", "bob", map[string]any{"instance": "bob-inst"})
}
func TestPostgresProjectRoleRevokeRemovesAccess(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner, deputy := "owner@bus-test.example", "deputy@bus-test.example"
	bob, carol := "bob@bus-test.example", "carol@bus-test.example"
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("unit_")
	exec := func(principal, project, op, session string, a map[string]any) (Result, error) {
		return s.ExecuteAs(ctx, principal, Request{Operation: op, Workspace: w, Project: project, Session: session, Args: a})
	}
	call := func(principal, project, op, session string, a map[string]any) map[string]any {
		t.Helper()
		r, e := exec(principal, project, op, session, a)
		if e != nil {
			t.Fatalf("%s as %s: %v", op, principal, e)
		}
		data, _ := r.Data.(map[string]any)
		return data
	}
	forbidden := func(e error) bool {
		var coded interface{ Code() string }
		return errors.As(e, &coded) && coded.Code() == "forbidden"
	}
	call(owner, "", "workspace_init", "", map[string]any{"mode": "server"})
	call(owner, "", "project_create", "", map[string]any{"slug": "p"})
	call(owner, "", "project_create", "", map[string]any{"slug": "q"})
	call(owner, "p", "project_role_set", "", map[string]any{"principal": deputy, "role": "owner"})
	call(owner, "p", "project_role_set", "", map[string]any{"principal": bob, "role": "member"})
	call(owner, "p", "project_role_set", "", map[string]any{"principal": carol, "role": "member"})
	call(owner, "q", "project_role_set", "", map[string]any{"principal": carol, "role": "member"})
	call(owner, "p", "register_session", "lead", map[string]any{"instance": "lead-inst"})
	bobSession := call(bob, "p", "register_session", "bob", map[string]any{"instance": "bob-inst"})["id"].(string)
	call(carol, "p", "register_session", "carol", map[string]any{"instance": "carol-inst"})

	for _, tc := range []struct{ principal, target string }{{owner, owner}, {deputy, owner}, {deputy, deputy}, {bob, carol}} {
		if _, e := exec(tc.principal, "p", "project_role_revoke", "", map[string]any{"principal": tc.target}); e == nil {
			t.Fatalf("%s revoked %s", tc.principal, tc.target)
		}
	}
	revoked := call(owner, "p", "project_role_revoke", "", map[string]any{"principal": "Bob@Bus-Test.example"})
	if retired, _ := revoked["retired_sessions"].([]string); len(retired) != 1 || retired[0] != bobSession || revoked["membership_removed"] != true {
		t.Fatal("revoke did not retire sessions or remove the last membership", revoked)
	}
	for _, op := range []string{"history", "list_sessions", "inbox"} {
		if _, e := exec(bob, "p", op, "bob", map[string]any{"instance": "bob-inst"}); !forbidden(e) {
			t.Fatalf("revoked principal %s: %v", op, e)
		}
	}
	for _, to := range []string{bobSession, "bob"} {
		if _, e := exec(owner, "p", "send", "lead", map[string]any{"instance": "lead-inst", "to": to, "body": "after revoke", "operation_id": "after-revoke-" + to}); e == nil {
			t.Fatalf("message to revoked session %s was accepted", to)
		}
	}
	if _, e := exec(owner, "p", "project_role_revoke", "", map[string]any{"principal": bob}); e == nil {
		t.Fatal("repeated revoke succeeded")
	}
	// A principal with a role elsewhere keeps workspace membership.
	revoked = call(owner, "p", "project_role_revoke", "", map[string]any{"principal": carol})
	if revoked["membership_removed"] != false {
		t.Fatal("membership removed while another project role remains", revoked)
	}
	if _, e := exec(carol, "p", "history", "", nil); !forbidden(e) {
		t.Fatal("revoked project still readable", e)
	}
	call(carol, "q", "list_sessions", "", nil)
}
