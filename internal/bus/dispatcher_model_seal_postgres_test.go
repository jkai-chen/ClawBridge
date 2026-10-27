package bus

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
)

// AG-L1 on Postgres, as the operator runs it, in a schema of its own so no
// other test's keys are re-sealed: the rows are locked and re-sealed, a
// key that opens under neither seal key stops the rotation, and each
// workspace's history gets the counts.
func TestPostgresOperatorModelSealRotate(t *testing.T) {
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
	schema := strings.ReplaceAll(newID("seal"), "-", "_")
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
	if _, err = s.ExecuteAsIdentity(ctx, Identity{Principal: owner, Issuer: GoogleIssuer, Subject: "sub-owner"}, Request{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "server"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	insert := func(who string, sealKey []byte) {
		t.Helper()
		m, _ := NewModelSeal(sealKey, nil)
		c, n, err := m.seal([]byte(testModelKey), modelAAD(who, "anthropic", "anthropic-messages", "https://api.anthropic.com", "claude-test", 1))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = admin.ExecContext(ctx, `INSERT INTO `+schema+`.dispatcher_model_keys(owner_principal,state,provider_id,api,base_url,model,ciphertext,nonce,key_fingerprint,config_revision,egress_text_version,egress_confirmed_at,created_by,created_at) VALUES($1,'active','anthropic','anthropic-messages','https://api.anthropic.com','claude-test',$2,$3,'wxyz',1,'v2','2026-01-01T00:00:00Z',$1,'2026-01-01T00:00:00Z')`, who, c, n); err != nil {
			t.Fatal(err)
		}
	}
	insert(owner, testSealKey)
	insert("other@identity-test.example", bytes.Repeat([]byte{5}, 32))
	op, err := OpenPostgresOperator(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	newKey := bytes.Repeat([]byte{9}, 32)
	rotating, _ := NewModelSeal(newKey, testSealKey)
	if _, _, err = op.OperatorModelSealRotate(ctx, rotating, false); err == nil || !strings.Contains(err.Error(), "1 stored model keys") {
		t.Fatal(err)
	}
	resealed, deleted, err := op.OperatorModelSealRotate(ctx, rotating, true)
	if err != nil || resealed != 1 || deleted != 1 {
		t.Fatal(resealed, deleted, err)
	}
	var c, n string
	if err = admin.QueryRowContext(ctx, `SELECT ciphertext,nonce FROM `+schema+`.dispatcher_model_keys WHERE owner_principal=$1`, owner).Scan(&c, &n); err != nil {
		t.Fatal(err)
	}
	only, _ := NewModelSeal(newKey, nil)
	if key, err := only.open(c, n, modelAAD(owner, "anthropic", "anthropic-messages", "https://api.anthropic.com", "claude-test", 1)); err != nil || string(key) != testModelKey {
		t.Fatal("the re-sealed key does not open under the new seal key:", err)
	}
	var details string
	if err = admin.QueryRowContext(ctx, `SELECT details FROM `+schema+`.history WHERE action='resealed' AND changed_by_principal=$1`, OperatorPrincipal).Scan(&details); err != nil || !strings.Contains(details, `"resealed":1`) {
		t.Fatal(details, err)
	}
}
