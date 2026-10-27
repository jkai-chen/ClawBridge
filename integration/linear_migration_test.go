package integration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type l1LegacyStore struct {
	db      *sql.DB
	pg      bool
	open    func() (*bus.Store, error)
	execute func(*bus.Store, bus.Request) (bus.Result, error)
}

func l1VersionTwoStore(t *testing.T, pg bool) *l1LegacyStore {
	t.Helper()
	f := &l1LegacyStore{pg: pg}
	var fixture string
	if !pg {
		path := filepath.Join(t.TempDir(), "actual-v2.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		f.db = db
		f.open = func() (*bus.Store, error) { return bus.Open(path) }
		f.execute = func(s *bus.Store, r bus.Request) (bus.Result, error) { return s.Execute(context.Background(), r) }
		raw, err := os.ReadFile("testdata/linear-v2.sqlite.sql")
		if err != nil {
			t.Fatal(err)
		}
		u, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		principal := "local:" + u.Uid + ":" + u.Username
		fixture = strings.ReplaceAll(string(raw), "__LOCAL_FIXTURE_PRINCIPAL__", strings.ReplaceAll(principal, "'", "''"))
	} else {
		base := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
		if base == "" {
			if os.Getenv("CLAWBRIDGE_REQUIRE_POSTGRES") == "1" {
				t.Fatal("explicit PG gate missing test DSN")
			}
			t.Skip("real PostgreSQL not configured; run integration/run-postgres.sh after freeze")
		}
		admin, err := sql.Open("pgx", base)
		if err != nil {
			t.Fatal(err)
		}
		var seed [8]byte
		if _, err = rand.Read(seed[:]); err != nil {
			t.Fatal(err)
		}
		schema := "review_linear_" + hex.EncodeToString(seed[:])
		if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
				t.Error("owned L1 schema cleanup", err)
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
		f.db = db
		f.open = func() (*bus.Store, error) { return bus.OpenPostgres(context.Background(), dsn, pgOwner) }
		f.execute = func(s *bus.Store, r bus.Request) (bus.Result, error) {
			return s.ExecuteAs(context.Background(), pgOwner, r)
		}
		raw, err := os.ReadFile("testdata/linear-v2.postgres.sql")
		if err != nil {
			t.Fatal(err)
		}
		fixture = string(raw)
	}
	t.Cleanup(func() { f.db.Close() })
	if _, err := f.db.Exec(fixture); err != nil {
		t.Fatal("synthetic v2 fixture", err)
	}
	return f
}

func (f *l1LegacyStore) version(t *testing.T) int {
	t.Helper()
	query := "PRAGMA user_version"
	if f.pg {
		query = "SELECT MAX(version) FROM clawbridge_schema"
	}
	var version int
	if err := f.db.QueryRow(query).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestL1VersionTwoMigrationPreservesContextPolicyAndRetries(t *testing.T) {
	for _, pg := range []bool{false, true} {
		name := "SQLite"
		if pg {
			name = "Postgres"
		}
		t.Run(name, func(t *testing.T) {
			f := l1VersionTwoStore(t, pg)
			if f.version(t) != 2 {
				t.Fatal("fixture is not actual schema2")
			}
			cols := coordinationLegacyColumns(t, f.db)
			before := coordinationTableSnapshot(t, f.db, cols)
			mailColumns := map[string][]string{"messages": cols["messages"], "deliveries": cols["deliveries"]}
			mailBefore := coordinationTableSnapshot(t, f.db, mailColumns)
			for i := 0; i < 2; i++ {
				s, err := f.open()
				if err != nil {
					t.Fatal(err)
				}
				s.Close()
				if f.version(t) != 30 || before != coordinationTableSnapshot(t, f.db, cols) {
					t.Fatal("v2->v11 migration/reopen changed legacy context/policy/digests/receipts/audit")
				}
				var count int
				if err = f.db.QueryRow("SELECT COUNT(*) FROM taskboard_bindings").Scan(&count); err != nil || count != 0 {
					t.Fatal("new shared binding table missing/not empty", err)
				}
			}
			s, err := f.open()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			call := func(op, session string, args map[string]any) map[string]any {
				t.Helper()
				r, err := f.execute(s, bus.Request{Operation: op, Workspace: "w", Project: "p", Session: session, Args: args})
				if err != nil {
					t.Fatal(err)
				}
				return object(t, jsonData(t, r.Data))
			}
			var policy string
			if err = f.db.QueryRow("SELECT notification_policy FROM sessions WHERE name='worker'").Scan(&policy); err != nil || policy != "hold" {
				t.Fatal("existing wake policy changed", err)
			}
			var originalID, rawContext, originalHash string
			if err = f.db.QueryRow("SELECT id,coordination,payload_hash FROM messages WHERE operation_id='l1-v2-coordination'").Scan(&originalID, &rawContext, &originalHash); err != nil {
				t.Fatal(err)
			}
			var contextObject map[string]any
			if json.Unmarshal([]byte(rawContext), &contextObject) != nil {
				t.Fatal("v2 context invalid")
			}
			retry := call("send", "lead", actor("lead", map[string]any{"to": "worker", "body": "synthetic v2 coordination context", "operation_id": "l1-v2-coordination", "coordination": contextObject}))
			if retry["id"] != originalID || !reflect.DeepEqual(retry["coordination"], contextObject) {
				t.Fatal("coordination retry changed identity/context")
			}
			call("update_session", "worker", actor("worker", map[string]any{"role": "other"}))
			call("register_session", "new-worker", map[string]any{"instance": "new-worker-instance", "role": "worker"})
			var legacyID string
			f.db.QueryRow("SELECT id FROM messages WHERE operation_id='l1-v2-legacy'").Scan(&legacyID)
			legacy := call("send", "lead", actor("lead", map[string]any{"to": "@role:worker", "body": "synthetic v2 legacy retry body", "operation_id": "l1-v2-legacy"}))
			if legacy["id"] != legacyID || legacy["coordination"] != nil || len(legacy["deliveries"].([]any)) != 1 {
				t.Fatal("legacy retry changed original identity/fanout/absence")
			}
			var actualHash string
			f.db.QueryRow("SELECT payload_hash FROM messages WHERE operation_id='l1-v2-coordination'").Scan(&actualHash)
			if actualHash != originalHash {
				t.Fatal("pre-v3 digest changed")
			}
			if mailBefore != coordinationTableSnapshot(t, f.db, mailColumns) {
				t.Fatal("v3 retries changed original message/delivery/read snapshots or duplicated fanout")
			}
		})
	}
}

func TestL1VersionTwoMigrationFailureAndNewerRefusal(t *testing.T) {
	for _, pg := range []bool{false, true} {
		name := "SQLite"
		if pg {
			name = "Postgres"
		}
		t.Run(name, func(t *testing.T) {
			for _, mode := range []string{"migration-collision", "newer"} {
				t.Run(mode, func(t *testing.T) {
					f := l1VersionTwoStore(t, pg)
					if mode == "migration-collision" {
						if _, err := f.db.Exec("CREATE TABLE taskboard_bindings(sentinel TEXT); INSERT INTO taskboard_bindings VALUES('preserve unrelated collision')"); err != nil {
							t.Fatal(err)
						}
					} else {
						query := "PRAGMA user_version=31"
						if pg {
							query = "INSERT INTO clawbridge_schema(version) VALUES(31)"
						}
						if _, err := f.db.Exec(query); err != nil {
							t.Fatal(err)
						}
					}
					cols := coordinationLegacyColumns(t, f.db)
					before := coordinationTableSnapshot(t, f.db, cols)
					s, err := f.open()
					if err == nil {
						s.Close()
						t.Fatal("unsafe migration/newer version accepted")
					}
					want := 2
					if mode == "newer" {
						want = 31
					}
					if f.version(t) != want || before != coordinationTableSnapshot(t, f.db, cols) {
						t.Fatal("failed migration changed version or legacy state")
					}
					if mode == "migration-collision" {
						var sentinel string
						if err = f.db.QueryRow("SELECT sentinel FROM taskboard_bindings").Scan(&sentinel); err != nil || sentinel != "preserve unrelated collision" {
							t.Fatal("collision table altered")
						}
					}
				})
			}
		})
	}
}
