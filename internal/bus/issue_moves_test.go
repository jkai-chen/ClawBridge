package bus

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

const movedIssue = "e4000000-0000-4000-8000-000000000004"

func moveArgs(op, from, to string) map[string]any {
	return map[string]any{"operation_id": op, "issue_uuid": movedIssue, "from_team": from, "to_team": to, "plan_sha256": strings.Repeat("a", 64)}
}

// movesHarness is the creates harness (lanes "one" writes teamOne, "both"
// writes both; c is the coordinator) on a server that has proven moves.
func movesHarness(t *testing.T) *harness {
	t.Helper()
	h, _ := createsHarness(t)
	h.s.SetIssueMoveProven(true)
	return h
}

// Nothing is reserved until the operator proves moves; then the owner,
// without a session, reserves a move between two bound write teams, a
// repeat returns it, and a reused operation ID with another plan is a
// conflict.
func TestIssueMoveReserveOwner(t *testing.T) {
	h, _ := createsHarness(t)
	if e := h.errorOf("issue_move_reserve", "", moveArgs("m-1", teamOne, teamTwo)); !strings.Contains(e, "issue_move_unproven") {
		t.Fatal("an unproven server reserved:", e)
	}
	h.s.SetIssueMoveProven(true)
	if e := h.errorOf("issue_move_reserve", "", moveArgs("m-1", teamOne, teamOne)); !strings.Contains(e, "already in that team") {
		t.Fatal("same team:", e)
	}
	if e := h.errorOf("issue_move_reserve", "", moveArgs("m-1", teamOne, "e5000000-0000-4000-8000-000000000005")); !strings.Contains(e, "isn't a write team") {
		t.Fatal("an unbound team:", e)
	}
	got := h.call("issue_move_reserve", "", moveArgs("m-1", teamOne, teamTwo)).(map[string]any)
	if got["state"] != "reserved" || got["by"] != "owner" || got["lane"] != "" || got["replay"] != false {
		t.Fatal(got)
	}
	if again := h.call("issue_move_reserve", "", moveArgs("m-1", teamOne, teamTwo)).(map[string]any); again["replay"] != true {
		t.Fatal("a repeat:", again)
	}
	other := moveArgs("m-1", teamOne, teamTwo)
	other["plan_sha256"] = strings.Repeat("b", 64)
	if e := h.errorOf("issue_move_reserve", "", other); !strings.Contains(e, "different Linear write") {
		t.Fatal("op reused:", e)
	}
	// A session can't take the owner's operation ID.
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	if e := h.errorOf("issue_move_reserve", "c", moveArgs("m-1", teamOne, teamTwo)); !strings.Contains(e, "different Linear write") {
		t.Fatal("a session took the owner's operation ID:", e)
	}
}

// N1: a session other than the coordinator, a coordinator with no lane,
// and a coordinator whose lane misses either end are refused; inside its
// lane it reserves, by coordinator with the lane named. A binding without
// lanes takes no coordinator move.
func TestIssueMoveReserveCoordinatorLane(t *testing.T) {
	h := movesHarness(t)
	if e := h.errorOf("issue_move_reserve", "a", moveArgs("m-1", teamOne, teamTwo)); !strings.Contains(e, "designated coordinator") {
		t.Fatal("a non-coordinator session reserved:", e)
	}
	if e := h.errorOf("issue_move_reserve", "c", moveArgs("m-1", teamOne, teamTwo)); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "no lane") {
		t.Fatal("an unpinned coordinator reserved:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "one"})
	if e := h.errorOf("issue_move_reserve", "c", moveArgs("m-1", teamOne, teamTwo)); !strings.Contains(e, "outside_lane: team "+teamTwo) {
		t.Fatal("the target outside the lane:", e)
	}
	if e := h.errorOf("issue_move_reserve", "c", moveArgs("m-1", teamTwo, teamOne)); !strings.Contains(e, "outside_lane: team "+teamTwo) {
		t.Fatal("the source outside the lane:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "c", "lane": "both"})
	got := h.call("issue_move_reserve", "c", moveArgs("m-1", teamTwo, teamOne)).(map[string]any)
	if got["by"] != "coordinator" || got["lane"] != "both" {
		t.Fatal(got)
	}
	// A client can't claim to be the coordinator: the server reads it, and
	// refuses the claim as an argument.
	claimed := moveArgs("m-2", teamTwo, teamOne)
	claimed["coordinator"] = true
	if e := h.errorOf("issue_move_reserve", "a", claimed); e == "" || count(t, h.s.db, "linear_write_reservations") != 1 {
		t.Fatal("a claimed coordinator reserved:", e)
	}
	plain := taskboardArgsV2(1)
	plain["linear_team_ids"] = []string{teamOne, teamTwo}
	h.call("taskboard_set_v2", "", plain)
	if e := h.errorOf("issue_move_reserve", "c", moveArgs("m-3", teamTwo, teamOne)); !strings.Contains(e, "no lanes") {
		t.Fatal("a coordinator without lanes reserved:", e)
	}
	if got := h.call("issue_move_reserve", "", moveArgs("m-3", teamTwo, teamOne)).(map[string]any); got["by"] != "owner" {
		t.Fatal("the owner without lanes:", got)
	}
}

// An accepted settle rewrites the identifier in every store that keeps
// one; a repeat replays, another outcome is a conflict, and only the
// reserving caller settles. History holds IDs only.
func TestIssueMoveSettle(t *testing.T) {
	h := movesHarness(t)
	var pid, sid string
	if e := h.s.db.QueryRow(`SELECT project_id,id FROM sessions WHERE name='a'`).Scan(&pid, &sid); e != nil {
		t.Fatal(e)
	}
	if _, e := h.s.db.Exec(`INSERT INTO session_assignments(id,project_id,session_id,issue_uuid,issue_identifier,revision,state,proposed_by_session,proposed_at) VALUES('as-1',?,?,?,'OLD-1','r1','confirmed',?,'2026-10-28T12:00:00Z')`, pid, sid, movedIssue, sid); e != nil {
		t.Fatal(e)
	}
	if _, e := h.s.db.Exec(`INSERT INTO intake_queue(project_id,issue_uuid,issue_identifier,signals,trigger_cap,cap_set_by,state,created_at,updated_at) VALUES(?,?,'OLD-1','[]','notify','owner','queued','2026-10-28T12:00:00Z','2026-10-28T12:00:00Z')`, pid, movedIssue); e != nil {
		t.Fatal(e)
	}
	h.call("issue_move_reserve", "", moveArgs("m-1", teamOne, teamTwo))
	settle := func(outcome, id string) map[string]any {
		return map[string]any{"operation_id": "m-1", "outcome": outcome, "old_identifier": "OLD-1", "new_identifier": id}
	}
	if e := h.errorOf("issue_move_settle", "a", settle("accepted", "NEW-7")); !strings.Contains(e, "no reservation of this kind") {
		t.Fatal("another caller settled:", e)
	}
	if e := h.errorOf("issue_move_settle", "", settle("accepted", "")); !strings.Contains(e, "new identifier") {
		t.Fatal("accepted without the new identifier:", e)
	}
	if e := h.errorOf("issue_move_settle", "", settle("done", "NEW-7")); !strings.Contains(e, "outcome must be") {
		t.Fatal("an unknown outcome:", e)
	}
	got := h.call("issue_move_settle", "", settle("accepted", "NEW-7")).(map[string]any)
	if got["state"] != "accepted" || got["new_identifier"] != "NEW-7" || got["rows_updated"] != 2 {
		t.Fatal(got)
	}
	for _, table := range issueIdentifierStores {
		var id string
		if e := h.s.db.QueryRow(`SELECT issue_identifier FROM `+table+` WHERE issue_uuid=?`, movedIssue).Scan(&id); e != nil || id != "NEW-7" {
			t.Fatal(table, id, e)
		}
	}
	if again := h.call("issue_move_settle", "", settle("accepted", "NEW-7")).(map[string]any); again["replay"] != true {
		t.Fatal("a repeat:", again)
	}
	if e := h.errorOf("issue_move_settle", "", settle("uncertain", "")); !strings.Contains(e, "already settled as accepted") {
		t.Fatal("another outcome:", e)
	}
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='issue_move' AND action='accepted'`).Scan(&details); e != nil || strings.Contains(details, h.principal()) || !strings.Contains(details, "NEW-7") || !strings.Contains(details, movedIssue) {
		t.Fatal("history:", details, e)
	}
	// A move that never reached Linear reopens on a retry; an uncertain one
	// rewrites nothing.
	h.call("issue_move_reserve", "", moveArgs("m-2", teamOne, teamTwo))
	h.call("issue_move_settle", "", map[string]any{"operation_id": "m-2", "outcome": "not_sent"})
	if got := h.call("issue_move_reserve", "", moveArgs("m-2", teamOne, teamTwo)).(map[string]any); got["state"] != "reserved" || got["replay"] != false {
		t.Fatal("not_sent reopen:", got)
	}
	if got := h.call("issue_move_settle", "", map[string]any{"operation_id": "m-2", "outcome": "uncertain", "new_identifier": "NEW-9"}).(map[string]any); got["rows_updated"] != 0 {
		t.Fatal("an uncertain move rewrote identifiers:", got)
	}
}

// M8: every column that holds an issue's display identifier is in a table
// issue_moved rewrites; a new one fails here until the move handles it.
func TestIssueMoveUpdatesEveryIdentifierStore(t *testing.T) {
	h := newHarness(t)
	rows, e := h.s.db.Query(`SELECT m.name,p.name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type='table' AND p.name LIKE '%identifier%'`)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var table, column string
		if e = rows.Scan(&table, &column); e != nil {
			t.Fatal(e)
		}
		got = append(got, table+"."+column)
	}
	var want []string
	for _, table := range issueIdentifierStores {
		want = append(want, table+".issue_identifier")
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("identifier columns %v, issueIdentifierStores rewrites %v", got, want)
	}
}
