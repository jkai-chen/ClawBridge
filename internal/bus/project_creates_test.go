package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

const createdProject = "e3000000-0000-4000-8000-000000000003"

// createsHarness binds teamOne and teamTwo with lanes ("one" writes
// teamOne; "both" writes both, teamTwo by default), makes c the
// coordinator, and runs the host clock from now.
func createsHarness(t *testing.T) (*harness, *time.Time) {
	t.Helper()
	h := newHarness(t)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	hostClock = func() time.Time { return now }
	t.Cleanup(func() { hostClock = time.Now })
	h.call("taskboard_set_v2", "", taskboardArgsLanes(0))
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	return h, &now
}

// seedKinds sets the project kinds on team and records their labels.
func (h *harness) seedKinds(team string) {
	h.t.Helper()
	h.call("project_kind_set", "", kindSet(0, team, "", "Engineering", "Review"))
	h.call("project_kind_record", "", kindRecord(1, pkEng, pkRev))
}

func reserveArgs(op, team, kind string) map[string]any {
	return map[string]any{"operation_id": op, "team_id": team, "kind": kind, "name_sha256": strings.Repeat("a", 64)}
}

func (h *harness) messagesOf(kind string) int {
	h.t.Helper()
	var n int
	if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE kind=?`, kind).Scan(&n); e != nil {
		h.t.Fatal(e)
	}
	return n
}

// Only the active designated coordinator reserves, in its lane's default
// team, with a seeded kind of that team; a repeat returns the same
// reservation, and there is no cap.
func TestProjectCreateReserveRules(t *testing.T) {
	h, _ := createsHarness(t)
	good := func() map[string]any { return reserveArgs("op-1", teamTwo, "Engineering") }
	if e := h.errorOf("project_create_reserve", "a", good()); !strings.Contains(e, "designated coordinator") {
		t.Fatal("a member's session reserved:", e)
	}
	if e := h.errorOf("project_create_reserve", "", good()); !strings.Contains(e, "coordinator session that makes it") {
		t.Fatal("a sessionless reserve:", e)
	}
	// Unpinned under read_only: no lane, no create.
	if e := h.errorOf("project_create_reserve", "c", good()); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "no lane") {
		t.Fatal("a read-only coordinator reserved:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	if e := h.errorOf("project_create_reserve", "c", reserveArgs("op-1", teamOne, "Engineering")); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "default team "+teamTwo) {
		t.Fatal("a lane's other team:", e)
	}
	if e := h.errorOf("project_create_reserve", "c", good()); !strings.Contains(e, "no project kinds") {
		t.Fatal("without kinds:", e)
	}
	h.call("project_kind_set", "", kindSet(0, teamOne, "", "Engineering", "Review"))
	if e := h.errorOf("project_create_reserve", "c", good()); !strings.Contains(e, "another team") {
		t.Fatal("the kinds' team differs:", e)
	}
	h.call("project_kind_set", "", kindSet(1, teamTwo, "", "Engineering", "Review"))
	if e := h.errorOf("project_create_reserve", "c", good()); !strings.Contains(e, "isn't seeded") {
		t.Fatal("an unseeded kind:", e)
	}
	h.call("project_kind_record", "", kindRecord(2, pkEng, pkRev))
	if e := h.errorOf("project_create_reserve", "c", reserveArgs("op-1", teamTwo, "Nope")); !strings.Contains(e, "one of the project's kinds") {
		t.Fatal("an unknown kind:", e)
	}
	got := h.call("project_create_reserve", "c", good()).(map[string]any)
	if got["state"] != "reserved" || got["kind_label_id"] != pkEng || got["replay"] != false {
		t.Fatal(got)
	}
	if again := h.call("project_create_reserve", "c", good()).(map[string]any); again["replay"] != true || again["kind_label_id"] != pkEng {
		t.Fatal("a repeat:", again)
	}
	for name, other := range map[string]map[string]any{"kind": reserveArgs("op-1", teamTwo, "Review"), "name": reserveArgs("op-1", teamTwo, "Engineering")} {
		if name == "name" {
			other["name_sha256"] = strings.Repeat("b", 64)
		}
		if e := h.errorOf("project_create_reserve", "c", other); !strings.Contains(e, "different Linear write") {
			t.Fatal("op reused, another", name, e)
		}
	}
	// No cap (owner, Q-E).
	for _, op := range []string{"op-2", "op-3", "op-4", "op-5", "op-6", "op-7"} {
		h.call("project_create_reserve", "c", reserveArgs(op, teamTwo, "Review"))
	}
	// History by IDs only.
	owner := h.principal()
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_create' ORDER BY sequence LIMIT 1`).Scan(&details); e != nil || strings.Contains(details, owner) || strings.Contains(details, `"c"`) || strings.Contains(details, "Engineering") {
		t.Fatal("history details:", details, e)
	}
	// A former coordinator is refused; the new one can't take over the
	// old one's operation ID.
	h.call("coordinator_set", "c", map[string]any{"target_session": "a"})
	if e := h.errorOf("project_create_reserve", "c", reserveArgs("op-8", teamTwo, "Review")); !strings.Contains(e, "designated coordinator") {
		t.Fatal("a former coordinator reserved:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "a", "lane": "both"})
	if e := h.errorOf("project_create_reserve", "a", good()); !strings.Contains(e, "different Linear write") {
		t.Fatal("another session took the operation ID:", e)
	}
	// A coordinator session that isn't active is no coordinator, nor is
	// the dispatcher's system session.
	if _, e := h.s.db.Exec(`UPDATE sessions SET status='moved' WHERE name='a'`); e != nil {
		t.Fatal(e)
	}
	var pid, aid string
	if e := h.s.db.QueryRow(`SELECT project_id,id FROM sessions WHERE name='a'`).Scan(&pid, &aid); e != nil {
		t.Fatal(e)
	}
	for name, actor := range map[string]map[string]any{
		"moved":  {"id": aid, "status": "moved", "role": "worker"},
		"system": {"id": aid, "status": "active", "role": "system"},
		"active": {"id": aid, "status": "active", "role": "worker"},
	} {
		tx, e := h.s.operatorTx(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		ok, e := isProjectCoordinator(context.Background(), tx, pid, actor)
		tx.Rollback()
		if e != nil || ok != (name == "active") {
			t.Fatal(name, ok, e)
		}
	}
}

// A binding without lanes: the binding's default team.
func TestProjectCreateWithoutLanes(t *testing.T) {
	h, _ := createsHarness(t)
	h.call("taskboard_set_v2", "", taskboardArgsV2(1))
	h.seedKinds(teamOne)
	if got := h.call("project_create_reserve", "c", reserveArgs("op-1", teamOne, "Review")).(map[string]any); got["kind_label_id"] != pkRev {
		t.Fatal(got)
	}
}

// The settle closes the session's own reservation, tells each of the
// owner's active sessions once, repeats safely, and never changes the
// project; not_sent closes it without telling, and a retry reopens it.
func TestProjectCreateSettle(t *testing.T) {
	h, _ := createsHarness(t)
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	h.seedKinds(teamTwo)
	h.call("project_create_reserve", "c", reserveArgs("op-1", teamTwo, "Engineering"))
	settle := func(project string) map[string]any {
		return map[string]any{"operation_id": "op-1", "outcome": "created", "linear_project_id": project}
	}
	if e := h.errorOf("project_create_settle", "a", settle(createdProject)); !strings.Contains(e, "no reservation of this kind") {
		t.Fatal("another session settled:", e)
	}
	got := h.call("project_create_settle", "c", settle(createdProject)).(map[string]any)
	// a, b and c are the local owner's sessions.
	if got["state"] != "created" || got["linear_project_id"] != createdProject || got["owner_sessions_told"] != 3 || h.messagesOf("project-created") != 3 {
		t.Fatal(got, h.messagesOf("project-created"))
	}
	var body string
	if e := h.s.db.QueryRow(`SELECT body FROM messages WHERE kind='project-created' LIMIT 1`).Scan(&body); e != nil || !strings.Contains(body, createdProject) || !strings.Contains(body, "op-1") || !strings.Contains(body, "No lead") {
		t.Fatal(body, e)
	}
	if again := h.call("project_create_settle", "c", settle(createdProject)).(map[string]any); again["replay"] != true || h.messagesOf("project-created") != 3 {
		t.Fatal("a repeated settle:", again)
	}
	if e := h.errorOf("project_create_settle", "c", settle(pkEng)); !strings.Contains(e, "with another result") {
		t.Fatal("a second project ID:", e)
	}
	if e := h.errorOf("project_create_settle", "c", map[string]any{"operation_id": "op-1", "outcome": "not_sent"}); !strings.Contains(e, "settled as accepted") {
		t.Fatal("not_sent after created:", e)
	}
	h.call("project_create_reserve", "c", reserveArgs("op-2", teamTwo, "Review"))
	if got = h.call("project_create_settle", "c", map[string]any{"operation_id": "op-2", "outcome": "not_sent"}).(map[string]any); got["state"] != "not_sent" || h.messagesOf("project-created") != 3 {
		t.Fatal(got)
	}
	if e := h.errorOf("project_create_settle", "c", map[string]any{"operation_id": "op-2", "outcome": "created", "linear_project_id": createdProject}); !strings.Contains(e, "settled as not_sent") {
		t.Fatal("created after not_sent:", e)
	}
	if got = h.call("project_create_reserve", "c", reserveArgs("op-2", teamTwo, "Review")).(map[string]any); got["state"] != "reserved" || got["settled_at"] != nil {
		t.Fatal("a retry didn't reopen:", got)
	}
	if e := h.errorOf("project_create_settle", "c", map[string]any{"operation_id": "op-2", "outcome": "maybe"}); !strings.Contains(e, "outcome must be") {
		t.Fatal(e)
	}
}

// An unsettled reservation past ten minutes shows in the overview as
// unconfirmed; the overview never sends, the first coordinator call or
// list tells the owner once, and the owner's acknowledgement clears it.
// Rows go 90 days after they close.
func TestProjectCreateStaleNoticeOnce(t *testing.T) {
	h, now := createsHarness(t)
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	h.seedKinds(teamTwo)
	h.call("project_create_reserve", "c", reserveArgs("op-1", teamTwo, "Engineering"))
	unconfirmed := func() []map[string]any {
		return h.overview("")["project_creates"].(map[string]any)["unconfirmed"].([]map[string]any)
	}
	*now = now.Add(9 * time.Minute)
	h.call("project_create_list", "", nil)
	if len(unconfirmed()) != 0 || h.messagesOf("project-create-unconfirmed") != 0 {
		t.Fatal("stale before ten minutes")
	}
	*now = now.Add(2 * time.Minute)
	if u := unconfirmed(); len(u) != 1 || u[0]["operation_id"] != "op-1" || h.messagesOf("project-create-unconfirmed") != 0 {
		t.Fatal("the overview:", u, h.messagesOf("project-create-unconfirmed"))
	}
	h.call("project_create_list", "", nil)
	if h.messagesOf("project-create-unconfirmed") != 3 {
		t.Fatal("the stale notice:", h.messagesOf("project-create-unconfirmed"))
	}
	h.call("project_create_list", "a", nil)
	h.call("project_create_reserve", "c", reserveArgs("op-2", teamTwo, "Review"))
	if h.messagesOf("project-create-unconfirmed") != 3 {
		t.Fatal("the stale notice repeated:", h.messagesOf("project-create-unconfirmed"))
	}
	if e := h.errorOf("project_create_ack", "a", map[string]any{"operation_id": "op-1"}); !strings.Contains(e, "a session can't acknowledge") {
		t.Fatal("a session acknowledged:", e)
	}
	h.call("project_create_ack", "", map[string]any{"operation_id": "op-1"})
	if len(unconfirmed()) != 0 {
		t.Fatal("an acknowledged create still shows")
	}
	if e := h.errorOf("project_create_ack", "", map[string]any{"operation_id": "op-1"}); !strings.Contains(e, "no unacknowledged") {
		t.Fatal(e)
	}
	h.call("project_create_settle", "c", map[string]any{"operation_id": "op-2", "outcome": "created", "linear_project_id": createdProject})
	if created := h.overview("")["project_creates"].(map[string]any)["created"].([]map[string]any); len(created) != 1 || created[0]["linear_project_id"] != createdProject {
		t.Fatal(created)
	}
	// Pruned after 90 days.
	*now = now.AddDate(0, 0, 89)
	h.call("project_create_list", "", nil)
	if n := h.count(`SELECT COUNT(*) FROM linear_write_reservations`); n != 2 {
		t.Fatal("pruned early:", n)
	}
	*now = now.AddDate(0, 0, 2)
	h.call("project_create_list", "", nil)
	if n := h.count(`SELECT COUNT(*) FROM linear_write_reservations`); n != 0 {
		t.Fatal("not pruned:", n)
	}
}

// An acknowledged reservation is never told as stale; a retired session
// of the owner is never told.
func TestProjectCreateAckedAndRetired(t *testing.T) {
	h, now := createsHarness(t)
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	h.seedKinds(teamTwo)
	h.call("project_create_reserve", "c", reserveArgs("op-1", teamTwo, "Engineering"))
	h.call("project_create_ack", "", map[string]any{"operation_id": "op-1"})
	*now = now.Add(11 * time.Minute)
	h.call("project_create_list", "", nil)
	if h.messagesOf("project-create-unconfirmed") != 0 {
		t.Fatal("an acknowledged create was told as stale")
	}
	h.call("retire_session", "b", nil)
	h.call("project_create_reserve", "c", reserveArgs("op-2", teamTwo, "Engineering"))
	if got := h.call("project_create_settle", "c", map[string]any{"operation_id": "op-2", "outcome": "created", "linear_project_id": createdProject}).(map[string]any); got["owner_sessions_told"] != 2 {
		t.Fatal("a retired session was told:", got)
	}
}

// With no owner session to tell, the stale notice waits.
func TestProjectCreateStaleNoticeWaits(t *testing.T) {
	h, now := createsHarness(t)
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	h.seedKinds(teamTwo)
	h.call("project_create_reserve", "c", reserveArgs("op-1", teamTwo, "Engineering"))
	if _, e := h.s.db.Exec(`UPDATE sessions SET owner_principal='other@example.com' WHERE role<>'system'`); e != nil {
		t.Fatal(e)
	}
	*now = now.Add(11 * time.Minute)
	h.call("project_create_list", "", nil)
	if h.messagesOf("project-create-unconfirmed") != 0 || h.count(`SELECT COUNT(*) FROM linear_write_reservations WHERE stale_notified_at IS NOT NULL`) != 0 {
		t.Fatal("claimed with nobody told")
	}
}

func (h *harness) count(q string) int {
	h.t.Helper()
	var n int
	if e := h.s.db.QueryRow(q).Scan(&n); e != nil {
		h.t.Fatal(e)
	}
	return n
}

// taskboard_access names the coordinator only when asked, and only for
// the active designated coordinator session; the parser takes the flag
// only as true, with a session.
func TestTaskboardAccessCoordinator(t *testing.T) {
	h, _ := createsHarness(t)
	ask := func(session string) (bool, bool) {
		out := h.call("taskboard_access", session, map[string]any{"ledger": 1, "coordinator_scope": 1}).(map[string]any)
		access, e := ParseTaskboardAccess(out)
		if e != nil {
			t.Fatal(e)
		}
		_, present := out["coordinator"]
		return access.Coordinator, present
	}
	if is, _ := ask("c"); !is {
		t.Fatal("the coordinator wasn't named")
	}
	if is, present := ask("a"); is || present {
		t.Fatal("a member's session named coordinator")
	}
	if is, present := ask(""); is || present {
		t.Fatal("a sessionless caller named coordinator")
	}
	if out := h.call("taskboard_access", "c", map[string]any{"ledger": 1}).(map[string]any); out["coordinator"] != nil {
		t.Fatal("named without coordinator_scope")
	}
	base := func() map[string]any {
		return map[string]any{"workspace_id": "w", "project_id": "p", "session_id": "s1", "session_name": "a", "can_write": true, "project_role": "member"}
	}
	good := base()
	good["coordinator"] = true
	if a, e := ParseTaskboardAccess(good); e != nil || !a.Coordinator {
		t.Fatal(a, e)
	}
	for name, change := range map[string]func(map[string]any){
		"false":      func(m map[string]any) { m["coordinator"] = false },
		"no session": func(m map[string]any) { m["coordinator"], m["session_id"], m["session_name"] = true, nil, nil },
	} {
		m := base()
		change(m)
		if _, e := ParseTaskboardAccess(m); e == nil {
			t.Fatal(name, "parsed")
		}
	}
}
