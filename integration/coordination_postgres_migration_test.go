package integration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"reflect"
	"testing"

	"clawbridge/internal/bus"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Each migration gets an owned schema inside the runner's disposable database;
// never downgrade the shared fixture schema used by other integration cases.
func coordinationLegacyPostgres(t *testing.T) (string, *sql.DB) {
	t.Helper()
	base := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if base == "" {
		if os.Getenv("CLAWBRIDGE_REQUIRE_POSTGRES") == "1" {
			t.Fatal("explicit PG migration gate missing DSN")
		}
		t.Skip("real PostgreSQL not configured; run integration/run-postgres.sh")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	var seed [8]byte
	if _, err = rand.Read(seed[:]); err != nil {
		t.Fatal(err)
	}
	schema := "review_coordination_" + hex.EncodeToString(seed[:])
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Error("owned migration schema cleanup", err)
		}
		admin.Close()
	})
	config, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	dsn := stdlib.RegisterConnConfig(config)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(dsn) })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fixture, err := os.ReadFile("testdata/coordination-v1.postgres.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(fixture)); err != nil {
		t.Fatal("legacy PG fixture", err)
	}
	return dsn, db
}

func TestCoordinationPostgresLegacyMigrationAndRetry(t *testing.T) {
	dsn, db := coordinationLegacyPostgres(t)
	cols := coordinationLegacyColumns(t, db)
	for _, table := range []string{"members", "project_roles"} {
		rows, err := db.Query("SELECT * FROM " + table + " LIMIT 0")
		if err != nil {
			t.Fatal(err)
		}
		cols[table], err = rows.Columns()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	before := coordinationTableSnapshot(t, db, cols)
	var priorID, digest string
	if err := db.QueryRow("SELECT id,payload_hash FROM messages WHERE operation_id='pre-v2-retry'").Scan(&priorID, &digest); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err := bus.OpenPostgres(context.Background(), dsn, pgOwner)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		var version, defaults, legacy int
		db.QueryRow("SELECT MAX(version) FROM clawbridge_schema").Scan(&version)
		db.QueryRow("SELECT COUNT(*) FROM sessions WHERE notification_policy='accept'").Scan(&defaults)
		db.QueryRow("SELECT COUNT(*) FROM messages WHERE coordination IS NULL").Scan(&legacy)
		if version != 51 || defaults != 2 || legacy != 1 {
			t.Fatalf("PG migration/reopen=%d/%d/%d", version, defaults, legacy)
		}
		if before != coordinationTableSnapshot(t, db, cols) {
			t.Fatal("PG migration changed legacy records/digests/deliveries/audit")
		}
	}
	s, err := bus.OpenPostgres(context.Background(), dsn, pgOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Keep the original role recipient after membership changes on retry.
	call := func(op, session string, args map[string]any) bus.Result {
		t.Helper()
		r, e := s.ExecuteAs(context.Background(), pgOwner, bus.Request{Operation: op, Workspace: "w", Project: "p", Session: session, Args: args})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	call("update_session", "worker", actor("worker", map[string]any{"role": "other"}))
	call("register_session", "new-worker", map[string]any{"instance": "new-worker-instance", "role": "worker"})
	r := call("send", "lead", actor("lead", map[string]any{"to": "@role:worker", "body": "legacy persisted before migration", "operation_id": "pre-v2-retry"}))
	if stringField(t, jsonData(t, r.Data), "id") != priorID {
		t.Fatal("PG legacy retry changed message")
	}
	var actual string
	var recipients, count int
	db.QueryRow("SELECT payload_hash FROM messages WHERE id=$1", priorID).Scan(&actual)
	db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	db.QueryRow("SELECT COUNT(*) FROM deliveries d JOIN sessions s ON d.recipient_id=s.id WHERE d.message_id=$1 AND s.name='worker'", priorID).Scan(&recipients)
	if digest != actual || count != 1 || recipients != 1 {
		t.Fatal("PG legacy retry changed hash/fanout or duplicated")
	}
}

func TestCoordinationPostgresMigrationRollbackAndNewerRefused(t *testing.T) {
	for _, mode := range []string{"partial-failure", "newer"} {
		t.Run(mode, func(t *testing.T) {
			dsn, db := coordinationLegacyPostgres(t)
			if mode == "partial-failure" {
				if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN notification_policy TEXT DEFAULT 'sentinel'"); err != nil {
					t.Fatal(err)
				}
			} else {
				db.Exec("INSERT INTO clawbridge_schema(version) VALUES(52)")
			}
			cols := coordinationLegacyColumns(t, db)
			before := coordinationTableSnapshot(t, db, cols)
			s, err := bus.OpenPostgres(context.Background(), dsn, pgOwner)
			if err == nil {
				s.Close()
				t.Fatal("unsafe PG migration accepted")
			}
			if before != coordinationTableSnapshot(t, db, cols) || !reflect.DeepEqual(cols, coordinationLegacyColumns(t, db)) {
				t.Fatal("failed PG migration committed data/partial ALTER")
			}
			var version int
			db.QueryRow("SELECT MAX(version) FROM clawbridge_schema").Scan(&version)
			want := 1
			if mode == "newer" {
				want = 52
			}
			if version != want {
				t.Fatal("failed migration committed version")
			}
		})
	}
}
