package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

// This fixture was produced by the actual pre-increment CLI at 59d68b5, not
// downgraded from a v2 store or populated after migrating. All content is fake.
func coordinationLegacySQLite(t *testing.T) (string, *sql.DB) {
	t.Helper()
	raw, err := os.ReadFile("testdata/coordination-v1.sqlite.sql")
	if err != nil {
		t.Fatal(err)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	principal := "local:" + u.Uid + ":" + u.Username
	text := strings.ReplaceAll(string(raw), "__LOCAL_FIXTURE_PRINCIPAL__", strings.ReplaceAll(principal, "'", "''"))
	path := filepath.Join(t.TempDir(), "persisted-v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(text); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func coordinationTableSnapshot(t *testing.T, db *sql.DB, columns map[string][]string) string {
	t.Helper()
	all := map[string][]string{}
	for table, cols := range columns {
		rows, err := db.Query("SELECT " + strings.Join(cols, ",") + " FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		var records []string
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range values {
				if b, ok := v.([]byte); ok {
					values[i] = string(b)
				}
			}
			raw, _ := json.Marshal(values)
			records = append(records, string(raw))
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(records)
		all[table] = records
	}
	raw, _ := json.Marshal(all)
	return string(raw)
}

func coordinationLegacyColumns(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	all := map[string][]string{}
	for _, table := range []string{"workspaces", "projects", "sessions", "messages", "deliveries", "history"} {
		rows, err := db.Query("SELECT * FROM " + table + " LIMIT 0")
		if err != nil {
			t.Fatal(err)
		}
		all[table], err = rows.Columns()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return all
}

func TestCoordinationSQLiteActualLegacyMigrationAndRetry(t *testing.T) {
	path, db := coordinationLegacySQLite(t)
	columns := coordinationLegacyColumns(t, db)
	before := coordinationTableSnapshot(t, db, columns)
	var priorID, digest string
	if err := db.QueryRow("SELECT id,payload_hash FROM messages WHERE operation_id='pre-v2-retry'").Scan(&priorID, &digest); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err := bus.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		var version, defaults, legacy int
		db.QueryRow("PRAGMA user_version").Scan(&version)
		db.QueryRow("SELECT COUNT(*) FROM sessions WHERE notification_policy='accept'").Scan(&defaults)
		db.QueryRow("SELECT COUNT(*) FROM messages WHERE coordination IS NULL").Scan(&legacy)
		if version != 15 || defaults != 2 || legacy != 1 {
			t.Fatalf("migration/reopen version/defaults=%d/%d/%d", version, defaults, legacy)
		}
		if before != coordinationTableSnapshot(t, db, columns) {
			t.Fatal("migration/reopen changed pre-existing domain records, digests or receipts")
		}
	}
	s, err := bus.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f := &fixture{t: t, path: path, store: s}
	// Change dynamic fanout AFTER migration; the old retry must still return its
	// original recipient snapshot and digest, never a fresh role expansion.
	f.call("update_session", "w", "p", "worker", actor("worker", map[string]any{"role": "other"}))
	f.call("register_session", "w", "p", "new-worker", map[string]any{"instance": "new-worker-instance", "role": "worker"})
	got := f.call("send", "w", "p", "lead", actor("lead", map[string]any{"to": "@role:worker", "body": "legacy persisted before migration", "operation_id": "pre-v2-retry"}))
	if stringField(t, got, "id") != priorID {
		t.Fatal("migration retry created a different message")
	}
	if _, ok := object(t, got)["coordination"]; ok {
		t.Fatal("legacy retry gained coordination")
	}
	var actualDigest string
	var recipients, count int
	db.QueryRow("SELECT payload_hash FROM messages WHERE id=?", priorID).Scan(&actualDigest)
	db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	db.QueryRow("SELECT COUNT(*) FROM deliveries d JOIN sessions s ON d.recipient_id=s.id WHERE d.message_id=? AND s.name='worker'", priorID).Scan(&recipients)
	if actualDigest != digest || recipients != 1 || count != 1 {
		t.Fatal("legacy retry changed digest/recipient snapshot or duplicated message")
	}
}

func TestCoordinationSQLiteMigrationFailureAtomicAndNewerRefused(t *testing.T) {
	for _, mode := range []string{"partial-migration-failure", "newer-schema"} {
		t.Run(mode, func(t *testing.T) {
			path, db := coordinationLegacySQLite(t)
			if mode == "partial-migration-failure" {
				if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN notification_policy TEXT DEFAULT 'sentinel'"); err != nil {
					t.Fatal(err)
				}
			} else {
				db.Exec("PRAGMA user_version=16")
			}
			cols := coordinationLegacyColumns(t, db)
			before := coordinationTableSnapshot(t, db, cols)
			s, err := bus.Open(path)
			if err == nil {
				s.Close()
				t.Fatal("unsafe migration accepted")
			}
			if before != coordinationTableSnapshot(t, db, cols) {
				t.Fatal("failed migration changed existing records")
			}
			var version int
			db.QueryRow("PRAGMA user_version").Scan(&version)
			want := 1
			if mode == "newer-schema" {
				want = 16
			}
			if version != want {
				t.Fatal("failed migration committed schema version")
			}
			newCols := coordinationLegacyColumns(t, db)
			if !reflect.DeepEqual(cols, newCols) {
				t.Fatal("failed migration committed its first ALTER TABLE")
			}
		})
	}
}

func TestCoordinationOptionalCorrelationValidatesNonRequestParent(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	parent := coordinationSend(f, "worker", "progress-parent", coordinationObject("opaque-9", "progress"))
	id := stringField(t, parent, "id")
	for _, intent := range []string{"accept", "ready"} {
		c := coordinationObject("opaque-9", intent)
		c["request_id"] = id
		f.call("reply", "w", "p", "worker", actor("worker", map[string]any{"message_id": id, "body": "context only, not approval", "operation_id": "any-parent-" + intent, "coordination": c}))
		c["request_id"] = "msg_missing"
		f.reject("send", "w", "p", "worker", actor("worker", map[string]any{"to": "lead", "body": "invalid optional link", "operation_id": "bad-optional-" + intent, "coordination": c}))
	}
	// Explicitly require two identical successful retries to remain equivalent.
	args := actor("worker", map[string]any{"to": "lead", "body": "new context", "operation_id": "opaque-exact", "coordination": coordinationObject("opaque-9", "ready")})
	a, err := f.store.Execute(context.Background(), f.request("send", "w", "p", "worker", args))
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.store.Execute(context.Background(), f.request("send", "w", "p", "worker", args))
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("exact coordination retry changed")
	}
}
