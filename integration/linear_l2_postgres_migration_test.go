package integration

import (
	"os"
	"regexp"
	"testing"
)

func TestL2PostgresSchema3MappingPreservationRollbackAndNewerRefusal(t *testing.T) {
	for _, mode := range []string{"upgrade_reopen", "failed_ddl_rollback", "newer_refused"} {
		t.Run(mode, func(t *testing.T) {
			f := l1VersionTwoStore(t, true)
			ddl, err := os.ReadFile("testdata/linear-l1-v3-taskboard.postgres.sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(string(ddl)); err != nil {
				t.Fatal(err)
			}
			if f.version(t) != 3 {
				t.Fatal("schema3 source-derived fixture not installed")
			}
			// Preserve the ACTUAL old serializer's mapping JSON bytes, not a
			// newly marshaled equivalent. Rows/PG identities are synthetic.
			sqliteDump, err := os.ReadFile("testdata/linear-l1-v3.sqlite.sql")
			if err != nil {
				t.Fatal(err)
			}
			match := regexp.MustCompile(`INSERT INTO "taskboard_bindings" VALUES\('[^']+',1,'([^']+)'`).FindSubmatch(sqliteDump)
			if len(match) != 2 {
				t.Fatal("actual old mapping JSON missing from historical fixture")
			}
			if _, err := f.db.Exec(`INSERT INTO taskboard_bindings(project_id,revision,mapping,updated_at) SELECT id,1,$1,'2026-10-22T00:00:00Z' FROM projects WHERE slug='p'`, string(match[1])); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`INSERT INTO projects(id,workspace_id,slug,name,git_remote,created_at) SELECT 'prj_l2_pg_tombstone',workspace_id,'cleared','Synthetic cleared','','2026-10-22T00:00:00Z' FROM projects WHERE slug='p'; INSERT INTO taskboard_bindings(project_id,revision,mapping,updated_at) VALUES('prj_l2_pg_tombstone',2,NULL,'2026-10-22T00:00:01Z')`); err != nil {
				t.Fatal(err)
			}
			wantVersion := 11
			switch mode {
			case "failed_ddl_rollback":
				if _, err := f.db.Exec(`ALTER TABLE taskboard_bindings ADD COLUMN mapping_version TEXT DEFAULT 'sentinel'`); err != nil {
					t.Fatal(err)
				}
				wantVersion = 3
			case "newer_refused":
				if _, err := f.db.Exec(`INSERT INTO clawbridge_schema(version) VALUES(12)`); err != nil {
					t.Fatal(err)
				}
				wantVersion = 12
			}
			columns := coordinationLegacyColumns(t, f.db)
			for _, table := range []string{"taskboard_bindings", "members", "project_roles"} {
				rows, err := f.db.Query("SELECT * FROM " + table + " LIMIT 0")
				if err != nil {
					t.Fatal(err)
				}
				columns[table], err = rows.Columns()
				rows.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			before := coordinationTableSnapshot(t, f.db, columns)
			store, err := f.open()
			if mode == "upgrade_reopen" {
				if err != nil {
					t.Fatal(err)
				}
				store.Close()
				store, err = f.open()
				if err != nil {
					t.Fatal(err)
				}
				store.Close()
				var count, legacy int
				if err := f.db.QueryRow(`SELECT COUNT(*),COUNT(*) FILTER(WHERE mapping_version=1) FROM taskboard_bindings`).Scan(&count, &legacy); err != nil || count != 2 || legacy != 2 {
					t.Fatalf("configured/tombstone legacy mapping versions %d/%d: %v", legacy, count, err)
				}
			} else {
				if store != nil {
					store.Close()
				}
				if err == nil {
					t.Fatal("unsafe/newer migration accepted")
				}
			}
			if f.version(t) != wantVersion || before != coordinationTableSnapshot(t, f.db, columns) {
				t.Fatal("schema migration/refusal changed old mapping bytes, revisions, tombstone, actor, roles or audit")
			}
		})
	}
}
