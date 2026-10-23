package integration

import (
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

type l2TableImage struct {
	Columns []string
	Rows    []string
}

// Read the original columns again after migration, so an additive column cannot
// hide a rewrite of old mapping JSON, tombstones, actor state or audit records.
func l2OriginalRows(t *testing.T, db *sql.DB, layout map[string]l2TableImage) map[string]l2TableImage {
	t.Helper()
	if layout == nil {
		layout = map[string]l2TableImage{}
		rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		var tables []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		for _, name := range tables {
			r, err := db.Query(`SELECT * FROM ` + l2SQLName(name) + ` LIMIT 0`)
			if err != nil {
				t.Fatal(err)
			}
			columns, err := r.Columns()
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			layout[name] = l2TableImage{Columns: columns}
		}
	}
	image := make(map[string]l2TableImage, len(layout))
	for name, old := range layout {
		columns := make([]string, len(old.Columns))
		for i, column := range old.Columns {
			columns[i] = l2SQLName(column)
		}
		rows, err := db.Query(`SELECT ` + strings.Join(columns, ",") + ` FROM ` + l2SQLName(name))
		if err != nil {
			t.Fatal(err)
		}
		current := l2TableImage{Columns: old.Columns}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			current.Rows = append(current.Rows, string(encoded))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(current.Rows)
		image[name] = current
	}
	return image
}

func l2SQLName(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func TestL2ActualSchema3MigrationPreservesOriginalRecords(t *testing.T) {
	for _, scenario := range []string{"upgrade_and_reopen", "failed_ddl_rolls_back", "newer_schema_refused"} {
		t.Run(scenario, func(t *testing.T) {
			dump, err := os.ReadFile("testdata/linear-l1-v3.sqlite.sql")
			if err != nil {
				t.Fatal(err)
			}
			u, err := user.Current()
			if err != nil {
				t.Fatal(err)
			}
			principal := strings.ReplaceAll("local:"+u.Uid+":"+u.Username, "'", "''")
			path := filepath.Join(t.TempDir(), "actual-old-v3.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(strings.ReplaceAll(string(dump), "__LOCAL_PRINCIPAL__", principal)); err != nil {
				t.Fatal(err)
			}
			wantVersion := 11
			switch scenario {
			case "failed_ddl_rolls_back":
				// An existing conflicting column forces the migration transaction to fail.
				if _, err := db.Exec(`ALTER TABLE taskboard_bindings ADD COLUMN mapping_version TEXT DEFAULT 'unrelated sentinel'`); err != nil {
					t.Fatal(err)
				}
				wantVersion = 3
			case "newer_schema_refused":
				if _, err := db.Exec(`PRAGMA user_version=12`); err != nil {
					t.Fatal(err)
				}
				wantVersion = 12
			}
			before := l2OriginalRows(t, db, nil)
			store, openErr := bus.Open(path)
			if scenario == "upgrade_and_reopen" {
				if openErr != nil {
					t.Fatal(openErr)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store, err = bus.Open(path)
				if err != nil {
					t.Fatal("reopen", err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				var count, legacy int
				if err := db.QueryRow(`SELECT COUNT(*),SUM(CASE WHEN mapping_version=1 THEN 1 ELSE 0 END) FROM taskboard_bindings`).Scan(&count, &legacy); err != nil || count != 2 || legacy != 2 {
					t.Fatalf("configured and cleared legacy rows not version1: %d/%d %v", legacy, count, err)
				}
			} else {
				if store != nil {
					store.Close()
				}
				if openErr == nil {
					t.Fatal("unsafe migration unexpectedly succeeded")
				}
				if scenario == "newer_schema_refused" && !strings.Contains(openErr.Error(), "newer") {
					t.Fatalf("wrong newer-schema error: %v", openErr)
				}
			}
			var version int
			if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != wantVersion {
				t.Fatalf("schema version %d want %d: %v", version, wantVersion, err)
			}
			if after := l2OriginalRows(t, db, before); !reflect.DeepEqual(before, after) {
				t.Fatal("migration rewrote historical rows, mapping bytes, tombstones, timestamps or audit state")
			}
		})
	}
}
