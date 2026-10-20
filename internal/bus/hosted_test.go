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
	call(owner, "project_role_set", "", map[string]any{"principal": member, "role": "member"})
	call(owner, "project_role_set", "", map[string]any{"principal": reader, "role": "member"})
	call(member, "register_session", "worker", map[string]any{"instance": "worker-inst", "role": "lead"})
	call(reader, "register_session", "reader", map[string]any{"instance": "reader-inst"})
	call(owner, "project_role_set", "", map[string]any{"principal": reader, "role": "reader"})
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
