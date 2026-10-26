package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

const teamOne = "11111111-1111-4111-8111-111111111111"

func taskboardArgs(rev int) map[string]any {
	return map[string]any{"expected_revision": rev, "linear_project_id": nil, "linear_team_ids": []string{teamOne}, "linear_default_team_id": teamOne}
}
func TestTaskboardCASAndNoStamp(t *testing.T) {
	h := newHarness(t)
	if _, e := h.s.db.Exec(`CREATE TRIGGER taskboard_no_touch BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'no actor touch'); END`); e != nil {
		t.Fatal(e)
	}
	get := func() TaskboardBinding {
		v := h.call("taskboard_get", "a", nil)
		b, e := ParseTaskboardBinding(v)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	if b := get(); b.Configured || b.Revision != 0 {
		t.Fatal(b)
	}
	before := count(t, h.s.db, "history")
	b, e := ParseTaskboardBinding(h.call("taskboard_set", "a", taskboardArgs(0)))
	if e != nil || !b.Configured || b.Revision != 1 {
		t.Fatal(b, e)
	}
	if b = get(); b.Revision != 1 || b.WorkspaceID == "" || b.ProjectID == "" || b.LinearTeamIDs[0] != teamOne {
		t.Fatal(b)
	}
	h.fail("taskboard_clear", "", map[string]any{"expected_revision": 0})
	if count(t, h.s.db, "history") != before+1 {
		t.Fatal("stale write audited")
	}
	h.call("taskboard_clear", "", map[string]any{"expected_revision": 1})
	if b = get(); b.Configured || b.Revision != 2 || b.Provider != "" {
		t.Fatal(b)
	}
	for _, a := range []map[string]any{nil, {"expected_revision": nil}, {"expected_revision": -1}, {"expected_revision": 1.5}, {"expected_revision": 2, "profile": "bad"}} {
		h.fail("taskboard_clear", "", a)
	}
	for _, key := range []string{"provider", "linear_team_ids", "linear_default_team_id", "linear_workspace_provenance"} {
		a := taskboardArgs(2)
		a[key] = nil
		h.fail("taskboard_set", "", a)
	}

}
func TestTaskboardConcurrentCAS(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := h.s.Execute(context.Background(), h.request("taskboard_set", "", taskboardArgs(0)))
			if e == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatal(success)
	}
}
func TestTaskboardSchemaFourAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrate.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	var v int
	if e = s.db.QueryRow("PRAGMA user_version").Scan(&v); e != nil || v != 22 {
		t.Fatal(v, e)
	}
	if _, e = s.db.Exec("DROP TABLE project_flow_nudges; DROP TABLE project_flow_moves; DROP TABLE project_flow_state; ALTER TABLE sessions DROP COLUMN alive_until; ALTER TABLE sessions DROP COLUMN alive_state; DROP TABLE health_posts; DROP TABLE device_nonces; DROP TABLE owner_transfers; DROP TABLE invites; DROP TABLE devices; DROP TABLE dispatcher_launcher_tokens; DROP TABLE dispatcher_agent_credentials; DROP TABLE dispatcher_model_keys; DROP TABLE dispatcher_cases; DROP TABLE dispatcher_model_tokens; DROP TABLE dispatcher_releases; DROP TABLE dispatcher_escalations; DROP TABLE dispatcher_decisions; DROP TABLE dispatcher_policies; ALTER TABLE session_assignments DROP COLUMN issue_project_id; ALTER TABLE session_assignments DROP COLUMN issue_labels; DROP TABLE linear_doc_publications; DROP TABLE linear_doc_policies; DROP TABLE project_health; DROP TABLE linear_status_policy; ALTER TABLE assignment_markers DROP COLUMN state_state; ALTER TABLE assignment_markers DROP COLUMN state_policy_revision; ALTER TABLE assignment_markers DROP COLUMN state_target; ALTER TABLE assignment_markers DROP COLUMN state_landed; ALTER TABLE assignment_markers DROP COLUMN state_skip_reason; ALTER TABLE assignment_markers DROP COLUMN outcome; DROP TABLE linear_doc_seeds; DROP TABLE intake_policies; DROP TABLE intake_trusted; DROP TABLE intake_queue; DROP TABLE intake_tokens; DROP TABLE intake_counters; DROP TABLE watch_ticks; DROP TABLE linear_write_unsettled; DROP TABLE linear_write_ledger; DROP TABLE linear_app_tokens; DROP TABLE linear_label_policy; DROP TABLE linear_comment_ledger; DROP TABLE watch_cursors; DROP TABLE watch_events; DROP TABLE watch_lease; DROP TABLE assignment_markers; DROP TABLE principal_subjects; DROP TABLE principal_events; ALTER TABLE workspaces DROP COLUMN allowed_domains; DROP TABLE taskboard_bindings; DROP TABLE session_assignments; ALTER TABLE projects DROP COLUMN coordinator_session; PRAGMA user_version=2"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec("PRAGMA user_version=23"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if s, e = Open(path); e == nil {
		s.Close()
		t.Fatal("newer schema accepted")
	}
}

func TestTaskboardActorFenceAndTombstone(t *testing.T) {
	h := newHarness(t)
	h.fail("taskboard_get", "a", map[string]any{"instance": "other"})
	h.fail("taskboard_get", "", map[string]any{"instance": "a-instance"})
	if _, e := h.s.db.Exec(`UPDATE sessions SET status='retired' WHERE name='a'`); e != nil {
		t.Fatal(e)
	}
	h.call("taskboard_get", "a", nil)
	h.fail("taskboard_set", "a", taskboardArgs(0))
	h.call("taskboard_clear", "", map[string]any{"expected_revision": 0})
	b, e := ParseTaskboardBinding(h.call("taskboard_get", "", nil))
	if e != nil || b.Configured || b.Revision != 1 {
		t.Fatal(b, e)
	}
	h.call("taskboard_set", "", taskboardArgs(1))
}
func TestTaskboardMigrationRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`PRAGMA user_version=2`); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if s, e = Open(path); e == nil {
		s.Close()
		t.Fatal("conflicting table should not partially migrate")
	}
	db, e := sql.Open("sqlite", path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var v int
	if e = db.QueryRow("PRAGMA user_version").Scan(&v); e != nil || v != 2 {
		t.Fatal(v, e)
	}
}

func TestTaskboardExactNestedShapeAndAccess(t *testing.T) {
	h := newHarness(t)
	initial := h.call("taskboard_get", "", nil).(map[string]any)
	if len(initial) != 6 || initial["binding"] != nil || initial["updated_at"] != nil {
		t.Fatal(initial)
	}
	data := h.call("taskboard_set", "", taskboardArgs(0)).(map[string]any)
	mapping := data["binding"].(map[string]any)
	if len(data) != 6 || len(mapping) != 5 || mapping["provider"] != "linear" || mapping["linear_project_id"] != nil || mapping["linear_workspace_id"] != nil {
		t.Fatal(data)
	}
	b, e := ParseTaskboardBinding(data)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	var round TaskboardBinding
	if e = json.Unmarshal(raw, &round); e != nil || round.LinearTeamIDs[0] != teamOne {
		t.Fatal(round, e)
	}
	missing := taskboardArgs(1)
	delete(missing, "linear_project_id")
	h.fail("taskboard_set", "", missing)
	forbidden := taskboardArgs(1)
	forbidden["provider"] = "linear"
	h.fail("taskboard_set", "", forbidden)
	if _, e = h.s.db.Exec(`CREATE TRIGGER access_no_touch BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'no actor touch'); END`); e != nil {
		t.Fatal(e)
	}
	for _, session := range []string{"", "a"} {
		v := h.call("taskboard_access", session, map[string]any{"ledger": 1}).(map[string]any)
		if len(v) != 6 || (session == "" && v["session_name"] != nil) || (session != "" && v["session_name"] != session) {
			t.Fatal(v)
		}
		a, e := ParseTaskboardAccess(v)
		if e != nil || !a.CanWrite || a.ProjectRole != "owner" {
			t.Fatal(a, e)
		}
		if session == "" && v["session_id"] != nil {
			t.Fatal(v)
		}
	}
	h.fail("taskboard_access", "a", map[string]any{"instance": "wrong", "ledger": 1})
	if _, e = h.s.db.Exec(`DROP TRIGGER access_no_touch; UPDATE sessions SET status='retired' WHERE name='a'`); e != nil {
		t.Fatal(e)
	}
	a, e := ParseTaskboardAccess(h.call("taskboard_access", "a", map[string]any{"ledger": 1}))
	if e != nil || a.CanWrite || a.SessionID == "" {
		t.Fatal(a, e)
	}
}

func TestTaskboardStrictWireParser(t *testing.T) {
	h := newHarness(t)
	valid := h.call("taskboard_set", "", taskboardArgs(0))
	for _, key := range []string{"workspace_id", "project_id", "configured", "revision", "binding", "updated_at"} {
		raw, _ := json.Marshal(valid)
		var m map[string]any
		json.Unmarshal(raw, &m)
		delete(m, key)
		if _, e := ParseTaskboardBinding(m); e == nil {
			t.Fatal("missing", key)
		}
	}
	for _, key := range []string{"provider", "linear_team_ids", "linear_default_team_id", "linear_project_id", "linear_workspace_id"} {
		raw, _ := json.Marshal(valid)
		var m map[string]any
		json.Unmarshal(raw, &m)
		delete(m["binding"].(map[string]any), key)
		if _, e := ParseTaskboardBinding(m); e == nil {
			t.Fatal("missing mapping", key)
		}
	}
	for _, v := range []any{nil, map[string]any{"can_write": true}, map[string]any{"workspace_id": "w", "project_id": "p", "session_id": nil, "can_write": true, "project_role": "reader"}} {
		if _, e := ParseTaskboardAccess(v); e == nil {
			t.Fatal(v)
		}
	}
}
