package bus

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

const movedIssue = "e4000000-0000-4000-8000-000000000004"

func moveArgs(op, from, to string) map[string]any {
	return map[string]any{"operation_id": op, "issue_uuid": movedIssue, "from_team": from, "to_team": to, "plan_sha256": strings.Repeat("a", 64), "calls": 8}
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

const (
	childOne = "e4000000-0000-4000-8000-000000000011"
	childTwo = "e4000000-0000-4000-8000-000000000012"
)

func familyArgs(op string, children ...any) map[string]any {
	a := moveArgs(op, teamOne, teamTwo)
	a["children"] = children
	return a
}

// A family needs its own proof; then the parent and each child are
// reserved together, each child as its own row under an operation ID
// derived from the family's; a repeat replays all of them.
func TestIssueMoveFamilyReserve(t *testing.T) {
	h := movesHarness(t)
	if e := h.errorOf("issue_move_reserve", "", familyArgs("f-1", childOne)); !strings.Contains(e, "issue_family_move_unproven") {
		t.Fatal("an unproven family reserved:", e)
	}
	h.s.SetIssueFamilyMoveProven(true)
	for name, bad := range map[string][]any{"the parent": {movedIssue}, "twice": {childOne, childOne}, "not a UUID": {"OLD-2"}} {
		if e := h.errorOf("issue_move_reserve", "", familyArgs("f-1", bad...)); e == "" {
			t.Fatal("children", name, "reserved")
		}
	}
	many := make([]any, maxFamilyChildren+1)
	for i := range many {
		many[i] = fmt.Sprintf("e5000000-0000-4000-8000-%012d", i)
	}
	if e := h.errorOf("issue_move_reserve", "", familyArgs("f-1", many...)); !strings.Contains(e, "at most") {
		t.Fatal("too many children:", e)
	}
	got := h.call("issue_move_reserve", "", familyArgs("f-1", childTwo, childOne)).(map[string]any)
	kids, _ := got["children"].([]map[string]any)
	if got["state"] != "reserved" || len(kids) != 2 || kids[0]["issue_uuid"] != childOne || kids[1]["issue_uuid"] != childTwo || count(t, h.s.db, "linear_write_reservations") != 3 {
		t.Fatal(got)
	}
	again := h.call("issue_move_reserve", "", familyArgs("f-1", childOne, childTwo)).(map[string]any)
	if again["replay"] != true || count(t, h.s.db, "linear_write_reservations") != 3 {
		t.Fatal("a repeat:", again)
	}
	if e := h.errorOf("issue_move_reserve", "", familyArgs("f-1", childOne)); !strings.Contains(e, "different Linear write") {
		t.Fatal("the same operation with other children:", e)
	}
	// A long operation ID still gives each child a valid ID of at most 128.
	long := strings.Repeat("x", 128)
	h.call("issue_move_reserve", "", familyArgs(long, childOne))
	var op string
	if e := h.s.db.QueryRow(`SELECT operation_id FROM linear_write_reservations WHERE kind='issue_move_child' AND operation_id NOT LIKE 'f-1%'`).Scan(&op); e != nil || len(op) > 128 || !commentOperationID.MatchString(op) {
		t.Fatal(op, e)
	}
}

// A family settles with every reserved child named once: not_sent for the
// parent leaves them all not_sent; accepted needs every child accepted;
// an accepted child's identifier is rewritten like the parent's.
func TestIssueMoveFamilySettle(t *testing.T) {
	h := movesHarness(t)
	h.s.SetIssueFamilyMoveProven(true)
	var pid string
	if e := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	if _, e := h.s.db.Exec(`INSERT INTO intake_queue(project_id,issue_uuid,issue_identifier,signals,trigger_cap,cap_set_by,state,created_at,updated_at) VALUES(?,?,'OLD-3','[]','notify','owner','queued','2026-10-28T12:00:00Z','2026-10-28T12:00:00Z')`, pid, childOne); e != nil {
		t.Fatal(e)
	}
	h.call("issue_move_reserve", "", familyArgs("f-1", childOne, childTwo))
	child := func(id, outcome, identifier string) map[string]any {
		return map[string]any{"issue_uuid": id, "outcome": outcome, "old_identifier": "OLD-3", "new_identifier": identifier}
	}
	settle := func(outcome string, kids ...any) map[string]any {
		return map[string]any{"operation_id": "f-1", "outcome": outcome, "old_identifier": "OLD-1", "new_identifier": "NEW-1", "children": kids}
	}
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"one missing":     {settle("accepted", child(childOne, "accepted", "NEW-3")), "each of its reserved children once"},
		"named twice":     {settle("accepted", child(childOne, "accepted", "NEW-3"), child(childOne, "accepted", "NEW-3")), "each of its reserved children once"},
		"a stranger":      {settle("accepted", child(childOne, "accepted", "NEW-3"), child(movedIssue, "accepted", "NEW-4")), "each of its reserved children once"},
		"accepted, short": {settle("accepted", child(childOne, "accepted", "NEW-3"), child(childTwo, "uncertain", "")), "only when every child is"},
		"child not_sent":  {settle("uncertain", child(childOne, "accepted", "NEW-3"), child(childTwo, "not_sent", "")), "accepted or uncertain"},
	} {
		if e := h.errorOf("issue_move_settle", "", c.args); !strings.Contains(e, c.want) {
			t.Fatal(name, e)
		}
	}
	got := h.call("issue_move_settle", "", settle("uncertain", child(childOne, "accepted", "NEW-3"), child(childTwo, "uncertain", ""))).(map[string]any)
	kids, _ := got["children"].([]map[string]any)
	if got["state"] != "uncertain" || len(kids) != 2 || got["rows_updated"] != 1 {
		t.Fatal(got)
	}
	var id string
	if e := h.s.db.QueryRow(`SELECT issue_identifier FROM intake_queue WHERE issue_uuid=?`, childOne).Scan(&id); e != nil || id != "NEW-3" {
		t.Fatal(id, e)
	}
	// A family not sent leaves every child not sent, and reopens whole.
	h.call("issue_move_reserve", "", familyArgs("f-2", childOne))
	h.call("issue_move_settle", "", map[string]any{"operation_id": "f-2", "outcome": "not_sent"})
	var state string
	if e := h.s.db.QueryRow(`SELECT state FROM linear_write_reservations WHERE kind='issue_move_child' AND subject=? AND operation_id LIKE 'f-2%'`, childOne).Scan(&state); e != nil || state != "not_sent" {
		t.Fatal(state, e)
	}
	if again := h.call("issue_move_reserve", "", familyArgs("f-2", childOne)).(map[string]any); again["state"] != "reserved" {
		t.Fatal("reopen:", again)
	}
	// A single move's settle can't name children.
	h.call("issue_move_reserve", "", moveArgs("m-1", teamOne, teamTwo))
	if e := h.errorOf("issue_move_settle", "", map[string]any{"operation_id": "m-1", "outcome": "uncertain", "children": []any{}}); !strings.Contains(e, "reserved no children") {
		t.Fatal(e)
	}
}

// A family under an operation ID too long to prefix its children's is
// found again on a repeat and on its settle: the children's IDs come from
// the operation's digest, the same every time.
func TestIssueMoveFamilyLongOperationResumes(t *testing.T) {
	h := movesHarness(t)
	h.s.SetIssueFamilyMoveProven(true)
	long := strings.Repeat("y", 128)
	first := h.call("issue_move_reserve", "", familyArgs(long, childOne, childTwo)).(map[string]any)
	again := h.call("issue_move_reserve", "", familyArgs(long, childOne, childTwo)).(map[string]any)
	if again["replay"] != true || count(t, h.s.db, "linear_write_reservations") != 3 || len(first["children"].([]map[string]any)) != 2 {
		t.Fatal(first, again)
	}
	kid := func(id string) map[string]any {
		return map[string]any{"issue_uuid": id, "outcome": "accepted", "old_identifier": "OLD-3", "new_identifier": "NEW-3"}
	}
	got := h.call("issue_move_settle", "", map[string]any{"operation_id": long, "outcome": "accepted", "new_identifier": "NEW-1", "children": []any{kid(childOne), kid(childTwo)}}).(map[string]any)
	if got["state"] != "accepted" || len(got["children"].([]map[string]any)) != 2 {
		t.Fatal("the long family's children weren't found on settle:", got)
	}
	// Another long operation sharing the first 128 characters' prefix is a
	// different family: none of its rows are the first one's.
	other := strings.Repeat("y", 127) + "z"
	h.call("issue_move_reserve", "", familyArgs(other, childOne))
	if e := h.errorOf("issue_move_settle", "", map[string]any{"operation_id": other, "outcome": "accepted", "new_identifier": "NEW-1", "children": []any{kid(childOne), kid(childTwo)}}); !strings.Contains(e, "each of its reserved children once") {
		t.Fatal("two long families mixed their children:", e)
	}
}

func (h *harness) budgetUsed() int {
	h.t.Helper()
	var n int
	if e := h.s.db.QueryRow(`SELECT COALESCE(SUM(budget_used),0) FROM watch_lease`).Scan(&n); e != nil {
		h.t.Fatal(e)
	}
	return n
}

// O2/P4: a move's estimated calls are counted in the project's hour,
// charged with its reservation; a repeat isn't charged again; a family is
// charged in full. Nobody's move is refused for the budget (FR8 r3), past
// an opt-in cap too.
func TestIssueMoveChargesTheBudget(t *testing.T) {
	h := movesHarness(t)
	h.s.SetIssueFamilyMoveProven(true)
	if e := h.errorOf("issue_move_reserve", "", map[string]any{"operation_id": "m-0", "issue_uuid": movedIssue, "from_team": teamOne, "to_team": teamTwo, "plan_sha256": strings.Repeat("a", 64)}); !strings.Contains(e, "calls must be") {
		t.Fatal("no estimate:", e)
	}
	h.call("comment_watch_budget_set", "", map[string]any{"watch_budget": 20})
	h.call("issue_move_reserve", "", moveArgs("m-1", teamOne, teamTwo))
	if h.budgetUsed() != 8 {
		t.Fatal("charged", h.budgetUsed())
	}
	h.call("issue_move_reserve", "", moveArgs("m-1", teamOne, teamTwo))
	if h.budgetUsed() != 8 {
		t.Fatal("a repeat was charged again:", h.budgetUsed())
	}
	// J1: the client's estimate has a family floor (the write, its
	// read-back, each child's sub-issue read and read-back).
	family := familyArgs("f-1", childOne, childTwo)
	family["calls"] = 5
	if e := h.errorOf("issue_move_reserve", "", family); !strings.Contains(e, "at least 6") {
		t.Fatal("under the family floor:", e)
	}
	if count(t, h.s.db, "linear_write_reservations") != 1 || h.budgetUsed() != 8 {
		t.Fatal("an estimate under the floor left a trace or a charge")
	}
	// FR8 r3: past the project's opt-in cap a session's family and move are
	// reserved and charged, never refused, as a person's is.
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	family["calls"] = 13
	if got := h.call("issue_move_reserve", "c", family).(map[string]any); got["state"] != "reserved" || h.budgetUsed() != 21 || count(t, h.s.db, "linear_write_reservations") != 4 {
		t.Fatal("the family wasn't reserved and charged in full past the cap:", got, h.budgetUsed())
	}
	if got := h.call("issue_move_reserve", "c", moveArgs("m-2", teamOne, teamTwo)).(map[string]any); got["state"] != "reserved" || h.budgetUsed() != 29 {
		t.Fatal("a session was refused past the cap:", got, h.budgetUsed())
	}
	got := h.call("issue_move_reserve", "", moveArgs("m-3", teamOne, teamTwo)).(map[string]any)
	if got["state"] != "reserved" || h.budgetUsed() != 37 || count(t, h.s.db, "linear_write_reservations") != 6 {
		t.Fatal("a person was refused or not charged past the cap:", got, h.budgetUsed())
	}
	// FR8's split: the people's moves and the coordinator session's family.
	if people, agents := h.split(); people != 16 || agents != 21 {
		t.Fatal("moves split", people, agents)
	}
}

// A family's operation acknowledges the family: the parent and each child
// still open, listed in the answer; a child's own operation ID can be
// acknowledged alone. History records the IDs only.
func TestIssueMoveFamilyAcknowledged(t *testing.T) {
	h := movesHarness(t)
	h.s.SetIssueFamilyMoveProven(true)
	h.call("issue_move_reserve", "", familyArgs("f-1", childOne, childTwo))
	h.call("issue_move_reserve", "", familyArgs("f-2", childOne, childTwo))
	got := h.call("project_create_ack", "", map[string]any{"operation_id": "f-1"}).(map[string]any)
	want := []string{"f-1", childOperation("f-1", childOne), childOperation("f-1", childTwo)}
	if !reflect.DeepEqual(got["acknowledged"], want) {
		t.Fatal(got["acknowledged"])
	}
	var open int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM linear_write_reservations WHERE operation_id LIKE 'f-1%' AND acknowledged_at IS NULL`).Scan(&open); err != nil || open != 0 {
		t.Fatal("the family is still open:", open, err)
	}
	kid := childOperation("f-2", childTwo)
	if got = h.call("project_create_ack", "", map[string]any{"operation_id": kid}).(map[string]any); !reflect.DeepEqual(got["acknowledged"], []string{kid}) {
		t.Fatal("a child alone:", got["acknowledged"])
	}
	// The parent then acknowledges what is left of its family.
	if got = h.call("project_create_ack", "", map[string]any{"operation_id": "f-2"}).(map[string]any); !reflect.DeepEqual(got["acknowledged"], []string{"f-2", childOperation("f-2", childOne)}) {
		t.Fatal(got["acknowledged"])
	}
	var details string
	if err := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_create' AND entity_key='f-1' AND action='acknowledged'`).Scan(&details); err != nil || !strings.Contains(details, childOperation("f-1", childOne)) || strings.Contains(details, "@") {
		t.Fatal(details, err)
	}
}
