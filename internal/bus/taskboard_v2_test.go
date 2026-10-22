package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

const projectOne = "11111111-1111-1111-1111-111111111111"
const projectTwo = "22222222-2222-2222-2222-222222222222"

func taskboardArgsV2(rev int, projects ...string) map[string]any {
	mode := "project_set"
	if len(projects) == 0 {
		mode = "team_board"
		projects = []string{}
	}
	return map[string]any{"expected_revision": rev, "mode": mode, "linear_team_ids": []string{teamOne}, "linear_default_team_id": teamOne, "linear_project_ids": projects}
}
func TestTaskboardV2CompatibilityAndCAS(t *testing.T) {
	h := newHarness(t)
	if _, e := h.s.db.Exec(`CREATE TRIGGER taskboard_v2_no_touch BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'no actor touch'); END`); e != nil {
		t.Fatal(e)
	}
	b, e := ParseTaskboardBindingV2(h.call("taskboard_get_v2", "a", nil))
	if e != nil || b.MappingVersion != 2 || b.Revision != 0 || b.Configured {
		t.Fatal(b, e)
	}
	h.call("taskboard_set", "a", taskboardArgs(0))
	var mapping string
	if e = h.s.db.QueryRow(`SELECT mapping FROM taskboard_bindings`).Scan(&mapping); e != nil {
		t.Fatal(e)
	}
	b, e = ParseTaskboardBindingV2(h.call("taskboard_get_v2", "a", nil))
	if e != nil || b.Mode != "team_board" || b.Revision != 1 {
		t.Fatal(b, e)
	}
	var after string
	h.s.db.QueryRow(`SELECT mapping FROM taskboard_bindings`).Scan(&after)
	if mapping != after {
		t.Fatal("read rewrote mapping")
	}
	b, e = ParseTaskboardBindingV2(h.call("taskboard_set_v2", "a", taskboardArgsV2(1, projectTwo, projectOne)))
	if e != nil || b.LinearProjectIDs[0] != projectOne || b.Revision != 2 {
		t.Fatal(b, e)
	}
	for _, op := range []string{"taskboard_get", "taskboard_set", "taskboard_clear"} {
		a := map[string]any{}
		if op == "taskboard_set" {
			a = taskboardArgs(2)
		}
		if op == "taskboard_clear" {
			a["expected_revision"] = 2
		}
		_, e = h.s.Execute(context.Background(), h.request(op, "a", a))
		c, ok := e.(interface{ Code() string })
		if !ok || c.Code() != "unsupported_mapping" {
			t.Fatal(op, e)
		}
	}
	h.fail("taskboard_clear_v2", "a", map[string]any{"expected_revision": 1})
	h.call("taskboard_clear_v2", "a", map[string]any{"expected_revision": 2})
	old, e := ParseTaskboardBinding(h.call("taskboard_get", "a", nil))
	if e != nil || old.Configured || old.Revision != 3 {
		t.Fatal(old, e)
	}
	h.call("taskboard_set_v2", "a", taskboardArgsV2(3, projectOne))
	old, e = ParseTaskboardBinding(h.call("taskboard_get", "a", nil))
	if e != nil || old.LinearProjectID != projectOne {
		t.Fatal(old, e)
	}
}
func TestTaskboardV2StrictFields(t *testing.T) {
	h := newHarness(t)
	for _, a := range []map[string]any{
		{"expected_revision": 0, "mode": "team_board", "linear_team_ids": []string{teamOne}, "linear_default_team_id": teamOne},
		{"expected_revision": 0, "mode": "team_board", "linear_team_ids": []string{teamOne}, "linear_default_team_id": teamOne, "linear_project_ids": nil},
		taskboardArgsV2(0, projectOne, projectOne),
	} {
		h.fail("taskboard_set_v2", "", a)
	}
	for _, key := range []string{"provider", "linear_project_id", "profile", "updated_at"} {
		a := taskboardArgsV2(0)
		a[key] = "invalid"
		h.fail("taskboard_set_v2", "", a)
	}
	value := h.call("taskboard_set_v2", "", taskboardArgsV2(0))
	raw, _ := json.Marshal(value)
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	if len(fields) != 7 || len(fields["binding"].(map[string]any)) != 6 {
		t.Fatal(fields)
	}
	var b TaskboardBindingV2
	if e := json.Unmarshal(raw, &b); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"workspace_id", "project_id", "configured", "revision", "mapping_version", "binding", "updated_at"} {
		var v map[string]any
		json.Unmarshal(raw, &v)
		delete(v, key)
		if _, e := ParseTaskboardBindingV2(v); e == nil {
			t.Fatal(key)
		}
	}
}
func TestTaskboardV4MigrationPreservesLegacyBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	// Genuine schema3 definition from prior accepted L1; mapping bytes intentionally unsorted.
	_, e = db.Exec(schema + `ALTER TABLE messages ADD COLUMN coordination TEXT; ALTER TABLE sessions ADD COLUMN notification_policy TEXT NOT NULL DEFAULT 'accept';` + taskboardSchema + `PRAGMA user_version=3;
 INSERT INTO workspaces VALUES('w','w','local',90,'owner','time');INSERT INTO projects VALUES('p','w','p','p','','time');`)
	if e != nil {
		t.Fatal(e)
	}
	mapping := `{"provider":"linear","linear_team_ids":["22222222-2222-2222-2222-222222222222","11111111-1111-1111-1111-111111111111"],"linear_default_team_id":"22222222-2222-2222-2222-222222222222","linear_project_id":null,"linear_workspace_id":null}`
	if _, e = db.Exec(`INSERT INTO taskboard_bindings VALUES('p',7,?,'timestamp')`, mapping); e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var stored string
	var rev, version int
	if e = s.db.QueryRow(`SELECT mapping,revision,mapping_version FROM taskboard_bindings`).Scan(&stored, &rev, &version); e != nil || stored != mapping || rev != 7 || version != 1 {
		t.Fatal(stored, rev, version, e)
	}
	s.db.Exec(`UPDATE workspaces SET owner_principal=?`, s.principal)
	h := &harness{t, s}
	old, e := ParseTaskboardBinding(h.call("taskboard_get", "", nil))
	if e != nil || old.LinearTeamIDs[0] != projectTwo {
		t.Fatal(old, e)
	}
	v2, e := ParseTaskboardBindingV2(h.call("taskboard_get_v2", "", nil))
	if e != nil || v2.LinearTeamIDs[0] != projectOne {
		t.Fatal(v2, e)
	}
	s.db.QueryRow(`SELECT mapping FROM taskboard_bindings`).Scan(&stored)
	if stored != mapping {
		t.Fatal("mapping normalized on disk")
	}
}

func TestTaskboardV2ConcurrentCAS(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := h.s.Execute(context.Background(), h.request("taskboard_set_v2", "", taskboardArgsV2(0, projectOne, projectTwo)))
			if e == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatal(accepted)
	}
	b, e := ParseBindingV2(h.call("taskboard_get_v2", "", nil))
	if e != nil || b.Revision != 1 {
		t.Fatal(b, e)
	}
}
func TestTaskboardV4MigrationRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial-v4.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	// Simulate an out-of-band partial ALTER: migration must not bless it or mutate metadata.
	if _, e = s.db.Exec(`PRAGMA user_version=3`); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if reopened, e := Open(path); e == nil {
		reopened.Close()
		t.Fatal("partial migration accepted")
	}
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var version int
	if e = db.QueryRow(`PRAGMA user_version`).Scan(&version); e != nil || version != 3 {
		t.Fatal(version, e)
	}
}
