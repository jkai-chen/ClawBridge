package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func (h *harness) overview(session string) map[string]any {
	h.t.Helper()
	return h.call("project_overview", session, nil).(map[string]any)
}

// The counts are an aggregate over every session, whatever the row cap;
// they classify as the Sessions tab does; the rows carry the coordinator
// and each live assignment's identifier, never its text.
func TestProjectOverviewCountsEverySession(t *testing.T) {
	h := newHarness(t)
	h.call("coordinator_set", "b", map[string]any{"target_session": "b"})
	l := h.propose("a", map[string]any{"issue_uuid": issueOne, "issue_identifier": "SYN-1", "task_role": "implementer"})["assignment"].(map[string]any)
	h.call("assignment_confirm", "b", map[string]any{"target_session": "a", "revision": l["revision"]})
	h.propose("c", map[string]any{"issue_uuid": issueTwo})
	for i := 0; i < 205; i++ {
		name := fmt.Sprintf("extra-%03d", i)
		h.call("register_session", name, map[string]any{"instance": name + "-instance", "role": "worker"})
	}
	h.call("retire_session", "extra-000", nil)
	h.call("retire_session", "extra-001", nil)
	// A released assignment counts for nothing.
	gone := h.propose("extra-003", map[string]any{"issue_uuid": "0c0c0c0c-0c0c-4c0c-8c0c-0c0c0c0c0c0c"})["assignment"].(map[string]any)
	h.call("assignment_release", "extra-003", map[string]any{"target_session": "extra-003", "revision": gone["revision"]})
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	// One retired two days ago (archived), one 23 hours ago (not yet), one
	// active but silent (stale).
	if _, err := h.s.db.Exec(`UPDATE sessions SET updated_at=? WHERE name='extra-000'`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.db.Exec(`UPDATE sessions SET updated_at=? WHERE name='extra-001'`, time.Now().Add(-23*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.db.Exec(`UPDATE sessions SET last_seen=? WHERE name='extra-002'`, old); err != nil {
		t.Fatal(err)
	}
	// a and b were registered first; seen most recently, they sort first.
	soon := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := h.s.db.Exec(`UPDATE sessions SET last_seen=? WHERE name IN ('a','b')`, soon); err != nil {
		t.Fatal(err)
	}
	got := h.overview("")
	counts := got["counts"].(map[string]any)
	want := map[string]any{"total": 208, "active": 206, "stale": 1, "retired": 2, "moved": 0, "archived": 1, "current": 207,
		"assignments_confirmed": 1, "assignments_proposed": 1}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("counts[%s] = %v, want %v: %v", k, counts[k], v, counts)
		}
	}
	rows := got["sessions"].([]map[string]any)
	if len(rows) != overviewSessions || got["sessions_truncated"] != true {
		t.Fatalf("%d rows, truncated %v", len(rows), got["sessions_truncated"])
	}
	for i, row := range rows {
		if row["status"] != "active" && i < len(rows)-2 {
			t.Fatalf("an inactive row before active ones at %d: %v", i, row["name"])
		}
	}
	coord := got["coordinator"].(map[string]any)
	if coord["session_id"] != h.sessionID("b") || coord["name"] != "b" || coord["status"] != "active" {
		t.Fatal(coord)
	}
	byName := map[string]map[string]any{}
	for _, row := range rows {
		byName[row["name"].(string)] = row
	}
	if a := byName["a"]; a == nil || a["coordinator"] != false || a["assignment"].(map[string]any)["issue_identifier"] != "SYN-1" ||
		a["assignment"].(map[string]any)["state"] != "confirmed" {
		t.Fatalf("a: %v", byName["a"])
	}
	if byName["b"]["coordinator"] != true || byName["b"]["assignment"] != nil {
		t.Fatalf("b: %v", byName["b"])
	}
	// With a session, the same answer; an instance without a session is refused.
	if h.overview("a")["counts"].(map[string]any)["total"] != 208 {
		t.Fatal("with a session")
	}
	if _, err := h.s.Execute(context.Background(), Request{Operation: "project_overview", Workspace: "w", Project: "p", Args: map[string]any{"instance": "a-instance"}}); err == nil {
		t.Fatal("instance without session accepted")
	}
}

// Exactly the cap is not truncated.
func TestProjectOverviewAtTheCap(t *testing.T) {
	h := newHarness(t)
	total := asInt(h.overview("")["counts"].(map[string]any)["total"])
	for i := total; i < overviewSessions; i++ {
		name := fmt.Sprintf("extra-%03d", i)
		h.call("register_session", name, map[string]any{"instance": name + "-instance", "role": "worker"})
	}
	got := h.overview("")
	if len(got["sessions"].([]map[string]any)) != overviewSessions || got["sessions_truncated"] != false {
		t.Fatalf("%d rows, truncated %v", len(got["sessions"].([]map[string]any)), got["sessions_truncated"])
	}
}

// A coordinator past the row cap is still named.
func TestProjectOverviewCoordinatorPastTheCap(t *testing.T) {
	h := newHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	for i := 0; i < overviewSessions; i++ {
		name := fmt.Sprintf("extra-%03d", i)
		h.call("register_session", name, map[string]any{"instance": name + "-instance", "role": "worker"})
	}
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := h.s.db.Exec(`UPDATE sessions SET last_seen=? WHERE name='c'`, old); err != nil {
		t.Fatal(err)
	}
	got := h.overview("")
	for _, row := range got["sessions"].([]map[string]any) {
		if row["name"] == "c" {
			t.Fatal("c is in the capped rows; the test needs it past the cap")
		}
	}
	if c, _ := got["coordinator"].(map[string]any); c == nil || c["name"] != "c" {
		t.Fatalf("coordinator: %v", got["coordinator"])
	}
}

// Nothing a reader can't already read: no instance token, transport,
// notes, notification settings or context bookkeeping; a binding that
// can't be read is a flag, with no error text.
func TestProjectOverviewShowsNoSecrets(t *testing.T) {
	h := newHarness(t)
	h.call("update_session", "a", map[string]any{"notes": "note-sentinel", "transport": map[string]any{"kind": "tmux", "target": "transport-sentinel"}})
	raw, err := json.Marshal(h.overview(""))
	if err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{"-instance", `"instance"`, "transport", "note-sentinel", "notification_policy", "context_backstop_at",
		"context_notified_at", "context_handover_at", "context_warn_tokens", "lease_token", "generation", "sha256", "secret"} {
		if strings.Contains(string(raw), never) {
			t.Fatalf("the overview shows %q: %s", never, raw)
		}
	}
	if got := h.overview("")["linear"].(map[string]any); got["configured"] != false || len(got) != 1 {
		t.Fatalf("unlinked: %v", got)
	}
	if _, err = h.s.db.Exec(`INSERT INTO taskboard_bindings(project_id,revision,mapping,updated_at,mapping_version) VALUES((SELECT id FROM projects WHERE slug='p'),1,'not json','2026-10-27T00:00:00Z',2)`); err != nil {
		t.Fatal(err)
	}
	got := h.overview("")["linear"].(map[string]any)
	if got["configured"] != true || got["unreadable"] != true || len(got) != 2 {
		t.Fatalf("unreadable: %v", got)
	}
}

// A linked board shows its teams, its projects and who closes each.
func TestProjectOverviewLinearSummary(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne, cpTwo})
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)})
	if _, err := h.closesSet(1, cpTwo, "agent", "cli-tty"); err != nil {
		t.Fatal(err)
	}
	// Health shows for a project whose health posts are on, not one whose
	// are off.
	for id, on := range map[string]int{cpOne: 1, cpTwo: 0} {
		if _, err := h.s.db.Exec(`INSERT INTO project_health(project_id,linear_project_id,enabled,policy,revision,last_health,next_health_at,updated_at,updated_by_principal) VALUES((SELECT id FROM projects WHERE slug='p'),?,?,'{}',1,'on_track','2026-10-27T00:00:00Z','2026-10-27T00:00:00Z','owner@example.test')`, id, on); err != nil {
			t.Fatal(err)
		}
	}
	got := h.overview("")["linear"].(map[string]any)
	teams := got["teams"].([]map[string]any)
	projects := got["projects"].([]map[string]any)
	if got["configured"] != true || got["revision"] != int64(1) || len(teams) != 1 || teams[0]["id"] != sfTeam || teams[0]["default"] != true ||
		len(projects) != 2 || projects[0]["closes"] != "human" || projects[1]["closes"] != "agent" || projects[0]["name"] != "" ||
		projects[0]["health"] != "on_track" || projects[1]["health"] != nil {
		t.Fatalf("%v", got)
	}
}

func TestPublicRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:token@example.test/org/repo.git": "https://example.test/org/repo.git",
		"https://example.test/org/repo.git":            "https://example.test/org/repo.git",
		"ssh://git@example.test/org/repo.git":          "ssh://example.test/org/repo.git",
		"git@example.test:org/repo.git":                "git@example.test:org/repo.git",
		"":                                             "",
	} {
		if got := publicRemote(in); got != want {
			t.Fatalf("%q: %q", in, got)
		}
	}
}

// The owner's overview lists every PROVEN gate by name, in a fixed order,
// with whether it is on, and nothing else of the server's settings.
func TestProjectOverviewShowsProvenGates(t *testing.T) {
	h := newHarness(t)
	h.s.SetIssueMoveProven(true)
	h.s.SetProjectTeamAddProven(true)
	gates := h.overview("")["proven_gates"].([]map[string]any)
	want := []string{"project_flow off", "issue_subscribers off", "issue_move on", "issue_family_move off", "project_team_add on"}
	if len(gates) != len(want) {
		t.Fatal(gates)
	}
	for i, g := range gates {
		state := "off"
		if g["on"] == true {
			state = "on"
		}
		if len(g) != 2 || fmt.Sprint(g["name"], " ", state) != want[i] {
			t.Fatal(i, g)
		}
	}
}
