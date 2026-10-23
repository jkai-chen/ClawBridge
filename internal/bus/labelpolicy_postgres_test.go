package bus

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
)

// Only the project owner sets the policy or records an admin write; a member
// reads it.
func TestPostgresLabelPolicyIsOwnerOnly(t *testing.T) {
	f := newPGIdentityFixture(t)
	member := newID("member") + "@identity-test.example"
	f.member(member)
	f.worker("own")
	f.must(member, "sub-member", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "mem", Args: map[string]any{"instance": "mem-i", "role": "worker"}})
	policy := map[string]any{"labels": []any{"Bug"}}
	if _, err := f.as(member, "sub-member", Request{Operation: "label_policy_set", Workspace: f.workspace, Project: "p", Session: "mem", Args: map[string]any{"instance": "mem-i", "expected_revision": 0, "label_policy": policy}}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatal("member set the policy", err)
	}
	audit := map[string]any{"instance": "mem-i", "kind": "project_create", "outcome": "accepted", "operation_id": "op", "linear_team_id": policyTeam}
	if _, err := f.as(member, "sub-member", Request{Operation: "linear_admin_audit", Workspace: f.workspace, Project: "p", Session: "mem", Args: audit}); err == nil {
		t.Fatal("member recorded an admin write")
	}
	// Sessionless (the owner's CLI): a member is still refused, the owner
	// records, and a retry is recorded once.
	if _, err := f.as(member, "sub-member", Request{Operation: "linear_admin_audit", Workspace: f.workspace, Project: "p", Args: map[string]any{"kind": "project_create", "outcome": "accepted", "operation_id": "op", "linear_team_id": policyTeam}}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatal("sessionless member recorded an admin write", err)
	}
	sessionless := Request{Operation: "linear_admin_audit", Workspace: f.workspace, Project: "p", Args: map[string]any{"kind": "project_create", "outcome": "accepted", "operation_id": "cb-rp-01", "linear_team_id": policyTeam, "name": "Review · One"}}
	f.must(f.owner, "sub-owner", sessionless)
	if again := f.must(f.owner, "sub-owner", sessionless); again.Data.(map[string]any)["already"] != true {
		t.Fatal("sessionless retry recorded twice", again)
	}
	if f.count(`SELECT COUNT(*) FROM history WHERE entity='linear_project' AND changed_by_principal=$1`, f.owner) != 1 {
		t.Fatal("sessionless audit not in history once")
	}
	set, err := f.in("label_policy_set", "own", map[string]any{"expected_revision": 0, "label_policy": policy})
	if err != nil || set.Data.(map[string]any)["revision"] != 1 {
		t.Fatal(set, err)
	}
	got, err := f.as(member, "sub-member", Request{Operation: "label_policy_get", Workspace: f.workspace, Project: "p", Session: "mem", Args: map[string]any{"instance": "mem-i"}})
	if err != nil || got.Data.(map[string]any)["revision"] != 1 || got.Data.(map[string]any)["updated_by"] != f.owner {
		t.Fatal("member read", got, err)
	}
	if _, err := f.in("label_policy_set", "own", map[string]any{"expected_revision": 0, "label_policy": policy}); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatal("stale CAS accepted on Postgres", err)
	}
	if f.count(`SELECT COUNT(*) FROM history WHERE entity='linear_label_policy' AND changed_by_principal=$1`, f.owner) != 1 {
		t.Fatal("policy set not in history")
	}
}

// Schema 8 is applied to a schema-7 Postgres store, in a schema of its own so
// no other test sees the dropped table.
func TestPostgresLabelPolicySchemaEightMigratesAVersionSevenStore(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := strings.ReplaceAll(newID("lp"), "-", "_")
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	owner := newID("owner") + "@identity-test.example"
	s, err := OpenPostgres(ctx, parsed.String(), owner)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err = admin.ExecContext(ctx, `DROP TABLE `+schema+`.linear_label_policy; DELETE FROM `+schema+`.clawbridge_schema WHERE version=8`); err != nil {
		t.Fatal(err)
	}
	s, err = OpenPostgres(ctx, parsed.String(), owner)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	var version, tables int
	if err = admin.QueryRowContext(ctx, `SELECT MAX(version) FROM `+schema+`.clawbridge_schema`).Scan(&version); err != nil || version != 8 {
		t.Fatal(version, err)
	}
	if err = admin.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name='linear_label_policy'`, schema).Scan(&tables); err != nil || tables != 1 {
		t.Fatal("linear_label_policy missing after migration", tables, err)
	}
}
