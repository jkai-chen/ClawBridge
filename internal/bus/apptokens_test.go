package bus

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sealed(app, org, sha string, minted time.Time) AppToken {
	return AppToken{AppClientID: app, LinearOrgID: org, TokenSHA256: sha, ScopesDigest: strings.Repeat("5", 64), Ciphertext: "c2VhbGVk", Nonce: "bm9uY2U=", MintedAt: minted, ExpiresAt: minted.Add(30 * 24 * time.Hour)}
}

// The app identity is the hosted server's: a local store refuses it.
func TestAppTokensNeedAHostedServer(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.s.LiveAppToken(context.Background(), "app", ""); err == nil || !strings.Contains(err.Error(), "hosted server") {
		t.Fatal(err)
	}
}

// Schema 9 is applied to a schema-8 store.
func TestAppIdentitySchemaNineMigratesAVersionEightStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`ALTER TABLE sessions DROP COLUMN alive_until; ALTER TABLE sessions DROP COLUMN alive_state; DROP TABLE health_posts; DROP TABLE device_nonces; DROP TABLE owner_transfers; DROP TABLE invites; DROP TABLE devices; DROP TABLE dispatcher_launcher_tokens; DROP TABLE dispatcher_agent_credentials; DROP TABLE dispatcher_model_keys; DROP TABLE dispatcher_cases; DROP TABLE dispatcher_model_tokens; DROP TABLE dispatcher_releases; DROP TABLE dispatcher_escalations; DROP TABLE dispatcher_decisions; DROP TABLE dispatcher_policies; ALTER TABLE session_assignments DROP COLUMN issue_project_id; ALTER TABLE session_assignments DROP COLUMN issue_labels; DROP TABLE linear_doc_publications; DROP TABLE linear_doc_policies; DROP TABLE project_health; DROP TABLE linear_status_policy; ALTER TABLE assignment_markers DROP COLUMN state_state; ALTER TABLE assignment_markers DROP COLUMN state_policy_revision; ALTER TABLE assignment_markers DROP COLUMN state_target; ALTER TABLE assignment_markers DROP COLUMN state_landed; ALTER TABLE assignment_markers DROP COLUMN state_skip_reason; ALTER TABLE assignment_markers DROP COLUMN outcome; DROP TABLE linear_doc_seeds; DROP TABLE intake_policies; DROP TABLE intake_trusted; DROP TABLE intake_queue; DROP TABLE intake_tokens; DROP TABLE intake_counters; DROP TABLE watch_ticks; ALTER TABLE assignment_markers DROP COLUMN engineer; DROP TABLE linear_write_unsettled; DROP TABLE linear_write_ledger; DROP TABLE linear_app_tokens; PRAGMA user_version=8;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, tables int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 20 {
		t.Fatal(version, err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='linear_app_tokens'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatal("linear_app_tokens missing", tables, err)
	}
}

func TestSharedLedgerNeedsAHostedServer(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.s.LedgerGet(context.Background(), strings.Repeat("a", 64), "op"); err == nil || !strings.Contains(err.Error(), "hosted server") {
		t.Fatal(err)
	}
}
