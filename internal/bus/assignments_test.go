package bus

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const issueOne = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
const issueTwo = "0b0b0b0b-0b0b-4b0b-8b0b-0b0b0b0b0b0b"

func (h *harness) errorOf(op, session string, a map[string]any) string {
	h.t.Helper()
	_, e := h.s.Execute(context.Background(), h.request(op, session, a))
	if e == nil {
		h.t.Fatalf("%s unexpectedly succeeded", op)
	}
	return e.Error()
}
func (h *harness) propose(session string, a map[string]any) map[string]any {
	h.t.Helper()
	return h.call("assignment_propose", session, a).(map[string]any)
}
func (h *harness) live(session string) map[string]any {
	h.t.Helper()
	who := h.call("whoami", session, nil).(map[string]any)["session"].(map[string]any)
	a, _ := who["assignment"].(map[string]any)
	return a
}
func (h *harness) sessionID(name string) string {
	h.t.Helper()
	return h.call("whoami", name, nil).(map[string]any)["session"].(map[string]any)["id"].(string)
}
func (h *harness) actions(entity string) []string {
	h.t.Helper()
	events := h.call("history", "a", map[string]any{"entity": entity, "limit": 1000}).(map[string]any)["events"].([]map[string]any)
	out := []string{}
	for _, e := range events {
		out = append(out, e["action"].(string))
	}
	return out
}

func TestAssignmentProposalValidation(t *testing.T) {
	h := newHarness(t)
	for name, a := range map[string]map[string]any{
		"nothing assigned":        {"task_role": "engineer"},
		"uppercase uuid":          {"issue_uuid": strings.ToUpper(issueOne)},
		"identifier without uuid": {"issue_identifier": "ABC-1", "scope_paths": []any{"internal"}},
		"absolute path":           {"scope_paths": []any{"/etc"}},
		"parent path":             {"scope_paths": []any{"../x"}},
		"unclean path":            {"scope_paths": []any{"internal//bus"}},
		"backslash":               {"scope_paths": []any{`internal\bus`}},
		"inner double star":       {"scope_paths": []any{"internal/**/x.go"}},
		"bad glob":                {"scope_paths": []any{"internal/["}},
		"control role":            {"issue_uuid": issueOne, "task_role": "a\nb"},
		"bad remote":              {"issue_uuid": issueOne, "scope_repo": "no-host"},
	} {
		t.Run(name, func(t *testing.T) {
			h.t = t
			h.fail("assignment_propose", "a", a)
		})
	}
	h.t = t
	many := []any{}
	for n := 0; n < 33; n++ {
		many = append(many, "p"+strings.Repeat("x", n))
	}
	h.fail("assignment_propose", "a", map[string]any{"scope_paths": many})
	// A session never proposes for another.
	h.fail("assignment_propose", "a", map[string]any{"issue_uuid": issueOne, "target_session": "b"})
	if h.live("a") != nil || h.live("b") != nil {
		t.Fatal("refused proposal left an assignment")
	}
}

func TestAssignmentProposeConfirmAndRelease(t *testing.T) {
	h := newHarness(t)
	got := h.propose("a", map[string]any{"issue_uuid": issueOne, "issue_identifier": "SYN-1", "task_role": "engineer", "scope_repo": "https://example.test/org/repo.git", "scope_paths": []any{"internal/bus/**", "docs"}})
	a := got["assignment"].(map[string]any)
	if a["state"] != "proposed" || a["scope_repo"] != "example.test/org/repo" || !reflect.DeepEqual(a["scope_paths"], []string{"docs", "internal/bus/**"}) || len(got["conflicts"].([]map[string]any)) != 0 || got["coordinator_session"] != nil {
		t.Fatalf("%+v", got)
	}
	revision := a["revision"].(string)
	// Nobody may confirm before a coordinator session is designated.
	if e := h.errorOf("assignment_confirm", "a", map[string]any{"revision": revision}); !strings.Contains(e, "designated coordinator") {
		t.Fatal(e)
	}
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	// Another session that is not the coordinator still may not confirm.
	if e := h.errorOf("assignment_confirm", "b", map[string]any{"target_session": "a", "revision": revision}); !strings.Contains(e, "designated coordinator") {
		t.Fatal(e)
	}
	if e := h.errorOf("assignment_confirm", "c", map[string]any{"target_session": "a", "revision": "rev_stale"}); !strings.Contains(e, "current revision") {
		t.Fatal(e)
	}
	h.fail("assignment_confirm", "c", map[string]any{"target_session": "a"})
	confirmed := h.call("assignment_confirm", "c", map[string]any{"target_session": "a", "revision": revision}).(map[string]any)["assignment"].(map[string]any)
	if confirmed["state"] != "confirmed" || confirmed["revision"] == revision || confirmed["confirmed_by_session"] != h.sessionID("c") {
		t.Fatalf("%+v", confirmed)
	}
	if e := h.errorOf("assignment_confirm", "c", map[string]any{"target_session": "a", "revision": confirmed["revision"]}); !strings.Contains(e, "already confirmed") {
		t.Fatal(e)
	}
	// Only the session itself or the coordinator releases, at the current revision.
	h.fail("assignment_release", "b", map[string]any{"target_session": "a", "revision": confirmed["revision"]})
	h.fail("assignment_release", "a", map[string]any{"revision": revision})
	h.call("assignment_release", "a", map[string]any{"revision": confirmed["revision"]})
	if h.live("a") != nil {
		t.Fatal("released assignment still live")
	}
	// The coordinator may release another session's assignment.
	b := h.propose("b", map[string]any{"issue_uuid": issueTwo})["assignment"].(map[string]any)
	h.call("assignment_release", "c", map[string]any{"target_session": "b", "revision": b["revision"]})
	if h.live("b") != nil {
		t.Fatal("coordinator release ignored")
	}
	if got := h.actions("assignment"); !reflect.DeepEqual(got, []string{"proposed", "confirmed", "released", "proposed", "released"}) {
		t.Fatal(got)
	}
}

func TestAssignmentReproposalReplacesTheLiveOne(t *testing.T) {
	h := newHarness(t)
	first := h.propose("a", map[string]any{"issue_uuid": issueOne})["assignment"].(map[string]any)
	second := h.propose("a", map[string]any{"issue_uuid": issueTwo})["assignment"].(map[string]any)
	if second["id"] == first["id"] || h.live("a")["issue_uuid"] != issueTwo {
		t.Fatal("reproposal did not replace")
	}
	var reason string
	if e := h.s.db.QueryRow(`SELECT release_reason FROM session_assignments WHERE id=?`, first["id"]).Scan(&reason); e != nil || reason != "replaced" {
		t.Fatal(reason, e)
	}
}

func TestAssignmentOverlapIsReportedAndRefusedAtConfirm(t *testing.T) {
	h := newHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	a := h.propose("a", map[string]any{"scope_paths": []any{"internal/**"}})["assignment"].(map[string]any)
	h.call("assignment_confirm", "c", map[string]any{"target_session": "a", "revision": a["revision"]})
	got := h.propose("b", map[string]any{"scope_paths": []any{"internal/bus/*.go"}})
	found := got["conflicts"].([]map[string]any)
	if len(found) != 1 || found[0]["session_id"] != h.sessionID("a") || found[0]["state"] != "confirmed" {
		t.Fatalf("%+v", found)
	}
	b := got["assignment"].(map[string]any)
	if e := h.errorOf("assignment_confirm", "c", map[string]any{"target_session": "b", "revision": b["revision"]}); !strings.Contains(e, h.sessionID("a")) || !strings.Contains(e, "allow_overlap") {
		t.Fatal(e)
	}
	if h.live("b")["state"] != "proposed" {
		t.Fatal("refused confirm changed state")
	}
	h.call("assignment_confirm", "c", map[string]any{"target_session": "b", "revision": b["revision"], "allow_overlap": true})
	if got := h.actions("assignment"); got[len(got)-1] != "conflict_overridden" {
		t.Fatal(got)
	}
	// A proposal overlapping only a proposed assignment is still reported, but
	// confirm checks confirmed owners only. Same issue overlaps too.
	h.call("assignment_release", "a", map[string]any{"revision": h.live("a")["revision"]})
	h.call("assignment_release", "b", map[string]any{"revision": h.live("b")["revision"]})
	h.propose("a", map[string]any{"issue_uuid": issueOne})
	c := h.propose("c", map[string]any{"issue_uuid": issueOne})
	if len(c["conflicts"].([]map[string]any)) != 1 {
		t.Fatal(c)
	}
	h.call("assignment_confirm", "c", map[string]any{"revision": c["assignment"].(map[string]any)["revision"]})
	// Different repositories never overlap.
	h.call("assignment_release", "a", map[string]any{"revision": h.live("a")["revision"]})
	h.propose("a", map[string]any{"scope_repo": "example.test/one", "scope_paths": []any{"**"}})
	if got := h.propose("b", map[string]any{"scope_repo": "example.test/two", "scope_paths": []any{"**"}}); len(got["conflicts"].([]map[string]any)) != 0 {
		t.Fatal(got)
	}
}

func TestAssignmentEndsWithItsSession(t *testing.T) {
	h := newHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	a := h.propose("a", map[string]any{"issue_uuid": issueOne, "task_role": "engineer", "scope_paths": []any{"internal"}})["assignment"].(map[string]any)
	h.call("assignment_confirm", "c", map[string]any{"target_session": "a", "revision": a["revision"]})
	h.call("register_session", "d", map[string]any{"instance": "d-instance"})
	retired := h.call("retire_session", "a", map[string]any{"status": "moved", "successor": "d"}).(map[string]any)
	ended := retired["assignment"].(map[string]any)
	if ended["released"] != a["id"] || ended["transferred_to"] == nil {
		t.Fatalf("%+v", ended)
	}
	// The successor inherits it as a proposal for the coordinator to confirm.
	d := h.live("d")
	if d["state"] != "proposed" || d["issue_uuid"] != issueOne || d["task_role"] != "engineer" || d["transferred_from"] != a["id"] || !reflect.DeepEqual(d["scope_paths"], []string{"internal"}) {
		t.Fatalf("%+v", d)
	}
	// A successor with its own live assignment keeps it.
	h.propose("b", map[string]any{"issue_uuid": issueTwo})
	retired = h.call("retire_session", "d", map[string]any{"status": "moved", "successor": "b"}).(map[string]any)
	if retired["assignment"].(map[string]any)["transfer_skipped"] == nil || h.live("b")["issue_uuid"] != issueTwo {
		t.Fatal(retired)
	}
	// Retiring the coordinator clears the designation.
	h.call("retire_session", "c", nil)
	who := h.call("whoami", "b", nil).(map[string]any)
	if who["project"].(map[string]any)["coordinator_session"] != nil {
		t.Fatal("retired coordinator kept its designation")
	}
	if got := h.actions("project"); got[len(got)-1] != "coordinator_set" {
		t.Fatal(got)
	}
}

func TestCoordinatorDesignation(t *testing.T) {
	h := newHarness(t)
	got := h.call("coordinator_set", "a", map[string]any{"target_session": "b"}).(map[string]any)
	if got["coordinator_session"] != h.sessionID("b") {
		t.Fatal(got)
	}
	h.call("retire_session", "c", nil)
	h.fail("coordinator_set", "a", map[string]any{"target_session": "c"})
	h.call("coordinator_set", "a", map[string]any{"target_session": ""})
	if h.call("whoami", "a", nil).(map[string]any)["project"].(map[string]any)["coordinator_session"] != nil {
		t.Fatal("not cleared")
	}
	// Retired sessions cannot act at all.
	h.fail("coordinator_set", "c", map[string]any{"target_session": "a"})
	h.fail("assignment_propose", "c", map[string]any{"issue_uuid": issueOne})
}

func TestListSessionsFindsOwnersByIssueScopeAndState(t *testing.T) {
	h := newHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	a := h.propose("a", map[string]any{"issue_uuid": issueOne, "issue_identifier": "SYN-1", "scope_paths": []any{"internal/bus/**", "docs/*.md"}})["assignment"].(map[string]any)
	h.call("assignment_confirm", "c", map[string]any{"target_session": "a", "revision": a["revision"]})
	h.propose("b", map[string]any{"issue_uuid": issueTwo, "scope_paths": []any{"internal/cli"}})
	names := func(filter map[string]any) []string {
		t.Helper()
		page := h.call("list_sessions", "c", filter).(map[string]any)
		out := []string{}
		for _, row := range page["sessions"].([]map[string]any) {
			out = append(out, row["name"].(string))
		}
		return out
	}
	for _, c := range []struct {
		filter map[string]any
		want   []string
	}{
		{map[string]any{}, []string{"a", "b", "c"}},
		{map[string]any{"issue": issueOne}, []string{"a"}},
		{map[string]any{"issue": "SYN-1"}, []string{"a"}},
		{map[string]any{"scope_path": "internal/bus/store.go"}, []string{"a"}},
		{map[string]any{"scope_path": "internal/cli/cli.go"}, []string{"b"}},
		{map[string]any{"scope_path": "docs/linear.md"}, []string{"a"}},
		{map[string]any{"scope_path": "docs/design/linear.md"}, []string{}},
		{map[string]any{"scope_path": "internal"}, []string{}},
		{map[string]any{"assignment_state": "proposed"}, []string{"b"}},
		{map[string]any{"assignment_state": "confirmed", "issue": issueTwo}, []string{}},
	} {
		if got := names(c.filter); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%v: %v", c.filter, got)
		}
	}
	page := h.call("list_sessions", "c", map[string]any{"limit": 1}).(map[string]any)
	row := page["sessions"].([]map[string]any)[0]
	if row["assignment"].(map[string]any)["state"] != "confirmed" || row["assignment"].(map[string]any)["assignment_stale"] != false || page["has_more"] != true {
		t.Fatal(page)
	}
	// A scope filter pages like any other.
	first := h.call("list_sessions", "c", map[string]any{"scope_path": "internal/bus/x.go", "limit": 1}).(map[string]any)
	if len(first["sessions"].([]map[string]any)) != 1 || first["has_more"] != false {
		t.Fatal(first)
	}
	for _, bad := range []map[string]any{{"assignment_state": "released"}, {"scope_path": "/abs"}, {"scope_path": "../up"}} {
		h.fail("list_sessions", "c", bad)
	}
}

func TestScopeCoverageAndOverlap(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		want          bool
	}{
		{"**", "any/where.go", true},
		{"internal", "internal/bus/store.go", true},
		{"internal/**", "internal/bus/store.go", true},
		{"internal/**", "internalx/a.go", false},
		{"internal/*.go", "internal/a.go", true},
		{"internal/*.go", "internal/bus/a.go", false},
		{"internal/bus", "internal", false},
	} {
		if got := scopeCovers(c.pattern, c.path); got != c.want {
			t.Fatalf("covers(%q,%q)=%v", c.pattern, c.path, got)
		}
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"internal/**", "internal/bus/*.go", true},
		{"**", "docs", true},
		{"docs", "internal", false},
		{"internal/cli", "internal/bus", false},
	} {
		if got := scopeOverlap(c.a, c.b); got != c.want {
			t.Fatalf("overlap(%q,%q)=%v", c.a, c.b, got)
		}
	}
}

func TestAssignmentSchemaFiveMigratesAVersionFourStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`DROP TABLE dispatcher_launcher_tokens; DROP TABLE dispatcher_agent_credentials; DROP TABLE dispatcher_model_keys; DROP TABLE dispatcher_cases; DROP TABLE dispatcher_model_tokens; DROP TABLE dispatcher_releases; DROP TABLE dispatcher_escalations; DROP TABLE dispatcher_decisions; DROP TABLE dispatcher_policies; ALTER TABLE session_assignments DROP COLUMN issue_project_id; ALTER TABLE session_assignments DROP COLUMN issue_labels; DROP TABLE linear_doc_publications; DROP TABLE linear_doc_policies; DROP TABLE project_health; DROP TABLE linear_status_policy; ALTER TABLE assignment_markers DROP COLUMN state_state; ALTER TABLE assignment_markers DROP COLUMN state_policy_revision; ALTER TABLE assignment_markers DROP COLUMN state_target; ALTER TABLE assignment_markers DROP COLUMN state_landed; ALTER TABLE assignment_markers DROP COLUMN state_skip_reason; ALTER TABLE assignment_markers DROP COLUMN outcome; DROP TABLE linear_doc_seeds; DROP TABLE intake_policies; DROP TABLE intake_trusted; DROP TABLE intake_queue; DROP TABLE intake_tokens; DROP TABLE intake_counters; DROP TABLE watch_ticks; DROP TABLE linear_write_unsettled; DROP TABLE linear_write_ledger; DROP TABLE linear_app_tokens; DROP TABLE linear_label_policy; DROP TABLE linear_comment_ledger; DROP TABLE watch_cursors; DROP TABLE watch_events; DROP TABLE watch_lease; DROP TABLE assignment_markers; DROP TABLE principal_subjects; DROP TABLE principal_events; ALTER TABLE workspaces DROP COLUMN allowed_domains; DROP TABLE session_assignments; ALTER TABLE projects DROP COLUMN coordinator_session; PRAGMA user_version=4;`); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var version, tables int
	if e = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); e != nil || version != 17 {
		t.Fatal(version, e)
	}
	if e = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('session_assignments','live_session_assignment')`).Scan(&tables); e != nil || tables != 2 {
		t.Fatal(tables, e)
	}
	// A second live assignment for one session is impossible in storage too.
	var db *sql.DB = s.db
	if _, e = db.Exec(`INSERT INTO workspaces(id,name,mode,retention_days,owner_principal,created_at) VALUES('w','w','local',90,'o','t'); INSERT INTO projects(id,workspace_id,slug,name,created_at) VALUES('p','w','p','p','t'); INSERT INTO sessions(id,project_id,name,instance,owner_principal,role,agent_kind,model,host,account,transport,notes,status,last_seen,created_at,updated_at) VALUES('s','p','s','i','o','','other','','','','{}','','active','t','t','t');`); e != nil {
		t.Fatal(e)
	}
	insert := `INSERT INTO session_assignments(id,project_id,session_id,issue_uuid,revision,state,proposed_by_session,proposed_at) VALUES(?,'p','s',?,?,?,'s','t')`
	if _, e = db.Exec(insert, "asg_1", issueOne, "r1", "released"); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(insert, "asg_2", issueOne, "r2", "proposed"); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(insert, "asg_3", issueTwo, "r3", "confirmed"); e == nil {
		t.Fatal("second live assignment stored")
	}
}
