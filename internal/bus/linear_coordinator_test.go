package bus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// domainCoordHarness is the team-adds harness (lanes "one" and "both", project
// set holding addProject, kinds seeded on teamTwo) with the dispatcher on and
// session c named as the default domain's coordinator, pinned to lane "both".
// Session a is another active session of the same principal.
func domainCoordHarness(t *testing.T) *harness {
	t.Helper()
	h, _ := teamAddsHarness(t)
	h.s.SetIssueMoveProven(true)
	h.seedKinds(teamTwo)
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	on, err := h.switchCall("dispatcher_enable", map[string]any{"expected_revision": 0, "confirm": true})
	if err != nil {
		t.Fatal(err)
	}
	edit := map[string]any{"expected_revision": on["revision"], "domain": defaultDomainID, "action": "coordinator", "target_session": "c"}
	if _, err = h.switchCall("dispatcher_domain_edit", edit); err != nil {
		t.Fatal(err)
	}
	return h
}

// domainRevision is the dispatcher policy's current revision.
func (h *harness) domainRevision() int {
	h.t.Helper()
	var n int
	if err := h.s.db.QueryRow(`SELECT revision FROM dispatcher_policies`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// setCoordinator makes the named session the default domain's coordinator.
func (h *harness) setCoordinator(name string) {
	h.t.Helper()
	edit := map[string]any{"expected_revision": h.domainRevision(), "domain": defaultDomainID, "action": "coordinator", "target_session": name}
	if _, err := h.switchCall("dispatcher_domain_edit", edit); err != nil {
		h.t.Fatal(err)
	}
}

// linearCoordinator is isLinearCoordinator for the named session.
func (h *harness) linearCoordinator(name string) bool {
	h.t.Helper()
	ctx := context.Background()
	raw, err := h.s.db.BeginTx(ctx, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer raw.Rollback()
	tx := &transaction{Tx: raw}
	rows, err := query(ctx, tx, `SELECT s.*,p.id AS pid FROM sessions s JOIN projects p ON p.id=s.project_id WHERE s.name=?`, name)
	if err != nil || len(rows) != 1 {
		h.t.Fatal(name, err)
	}
	ok, err := isLinearCoordinator(ctx, tx, str(rows[0], "pid"), rows[0])
	if err != nil {
		h.t.Fatal(err)
	}
	return ok
}

// tryWrites runs one coordinator write as a session and give
// the error text, or "" when it was reserved.
func (h *harness) tryWrites(session, suffix string) (move, add, create string) {
	h.t.Helper()
	try := func(op string, a map[string]any) string {
		_, e := h.s.Execute(context.Background(), h.request(op, session, a))
		if e == nil {
			return ""
		}
		return e.Error()
	}
	return try("issue_move_reserve", moveArgs("m-"+suffix, teamTwo, teamOne)),
		try("project_team_add_reserve", teamAddArgs("t-"+suffix, teamTwo, teamOne)),
		try("project_create_reserve", reserveArgs("c-"+suffix, teamTwo, "Engineering"))
}

// refusedAll fails unless the session's move, team add and create are each
// refused as forbidden by the coordinator test.
func (h *harness) refusedAll(what, session, suffix string) {
	h.t.Helper()
	for kind, op := range map[string]struct {
		name string
		args map[string]any
	}{
		"move":     {"issue_move_reserve", moveArgs("m-"+suffix, teamTwo, teamOne)},
		"team add": {"project_team_add_reserve", teamAddArgs("t-"+suffix, teamTwo, teamOne)},
		"create":   {"project_create_reserve", reserveArgs("c-"+suffix, teamTwo, "Engineering")},
	} {
		_, err := h.s.Execute(context.Background(), h.request(op.name, session, op.args))
		var coded interface{ Code() string }
		if err == nil || !errors.As(err, &coded) || coded.Code() != "forbidden" || !strings.Contains(err.Error(), "domain's coordinator session") {
			h.t.Fatal(what, ": ", kind, " was not refused by the coordinator test: ", err)
		}
	}
}

// While the dispatcher is on, a domain's coordinator session moves, adds a
// team and creates inside its lane, reserved by coordinator with the lane
// named; outside the lane it is refused; another session of the same
// principal and the dispatcher's own session are refused.
func TestDomainCoordinatorKeepsLinearRights(t *testing.T) {
	h := domainCoordHarness(t)
	h.refusedAll("another session of the same principal", "a", "x")
	// The dispatcher's own session holds the seat. Name it as a domain's
	// coordinator, with its own ID and principal: the role=system guard alone
	// must refuse it.
	var raw, sysID, sysPrincipal string
	if e := h.s.db.QueryRow(`SELECT domains FROM dispatcher_policies`).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if e := h.s.db.QueryRow(`SELECT id,owner_principal FROM sessions WHERE role='system' AND name=?`, DispatcherSessionName).Scan(&sysID, &sysPrincipal); e != nil {
		t.Fatal(e)
	}
	// Its owner principal is empty, which the empty-principal guard would
	// refuse; give it one, so only the role=system guard is left.
	sysPrincipal = "forged-owner@example.test"
	var origPrincipal string
	if e := h.s.db.QueryRow(`SELECT owner_principal FROM sessions WHERE id=?`, sysID).Scan(&origPrincipal); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.s.db.Exec(`UPDATE sessions SET owner_principal=? WHERE id=?`, origPrincipal, sysID) })
	if _, e := h.s.db.Exec(`UPDATE sessions SET owner_principal=? WHERE id=?`, sysPrincipal, sysID); e != nil {
		t.Fatal(e)
	}
	var doc DispatcherDomains
	if e := json.Unmarshal([]byte(raw), &doc); e != nil {
		t.Fatal(e)
	}
	doc.Domains[0].CoordinatorSession = &DispatcherSessionRef{ID: sysID, Name: DispatcherSessionName, Principal: sysPrincipal}
	forged, _ := json.Marshal(doc)
	if _, e := h.s.db.Exec(`UPDATE dispatcher_policies SET domains=?`, string(forged)); e != nil {
		t.Fatal(e)
	}
	if h.linearCoordinator(DispatcherSessionName) {
		t.Fatal("the system dispatcher session is a Linear coordinator")
	}
	if _, e := h.s.db.Exec(`UPDATE dispatcher_policies SET domains=?`, raw); e != nil {
		t.Fatal(e)
	}
	if !h.linearCoordinator("c") || h.linearCoordinator("a") {
		t.Fatal("isLinearCoordinator disagrees with the writes")
	}
	move, add, create := h.tryWrites("c", "1")
	if move != "" || add != "" || create != "" {
		t.Fatal("the domain coordinator was refused in its lane:", move, add, create)
	}
	for _, kind := range []string{"issue_move", "project_team_add", "project_create"} {
		var by, lane string
		if e := h.s.db.QueryRow(`SELECT by_role,lane FROM linear_write_reservations WHERE kind=?`, kind).Scan(&by, &lane); e != nil || by != "coordinator" || lane != "both" {
			t.Fatal(kind, by, lane, e)
		}
	}
	// Outside its lane: the lane "one" writes only teamOne.
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "c", "lane": "one"})
	if e := h.errorOf("issue_move_reserve", "c", moveArgs("m-2", teamTwo, teamOne)); !strings.Contains(e, "outside_lane") {
		t.Fatal(e)
	}
	if e := h.errorOf("project_team_add_reserve", "c", teamAddArgs("t-2", teamTwo, teamOne)); !strings.Contains(e, "outside_lane") {
		t.Fatal(e)
	}
	if e := h.errorOf("project_create_reserve", "c", reserveArgs("c-2", teamTwo, "Engineering")); !strings.Contains(e, "outside_lane") {
		t.Fatal(e)
	}
}

// The right follows the domain document: after a coordinator edit the old
// one is refused and the new one accepted; after disable a session that was
// only a domain coordinator is refused and the restored project coordinator
// is accepted.
func TestDomainCoordinatorFollowsEditAndDisable(t *testing.T) {
	h := domainCoordHarness(t)
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "a", "lane": "both"})
	h.refusedAll("a before it was named", "a", "0")
	h.setCoordinator("a")
	h.refusedAll("the old domain coordinator", "c", "1")
	if move, add, create := h.tryWrites("a", "2"); move != "" || add != "" || create != "" {
		t.Fatal("the new domain coordinator was refused:", move, add, create)
	}
	// Disable restores the owner's latest session as the project coordinator.
	off, err := h.switchCall("dispatcher_disable", map[string]any{"expected_revision": h.domainRevision()})
	if err != nil {
		t.Fatal(err)
	}
	restored := off["coordinator"].(string)
	other := "a"
	if restored == other {
		t.Fatal("the restored coordinator is the former domain coordinator; the test cannot tell them apart")
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 2, "kind": "session", "key": restored, "lane": "both"})
	h.refusedAll("a former domain coordinator after disable", other, "3")
	if move, add, create := h.tryWrites(restored, "4"); move != "" || add != "" || create != "" {
		t.Fatal("the restored project coordinator was refused:", restored, move, add, create)
	}
}

// A domain ref whose ID matches but whose principal differs (a session
// re-owned) grants nothing.
func TestDomainCoordinatorPrincipalMustMatch(t *testing.T) {
	h := domainCoordHarness(t)
	var raw string
	if e := h.s.db.QueryRow(`SELECT domains FROM dispatcher_policies`).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var doc DispatcherDomains
	if e := json.Unmarshal([]byte(raw), &doc); e != nil {
		t.Fatal(e)
	}
	doc.Domains[0].CoordinatorSession.Principal = "someone-else@example.test"
	out, _ := json.Marshal(doc)
	if _, e := h.s.db.Exec(`UPDATE dispatcher_policies SET domains=?`, string(out)); e != nil {
		t.Fatal(e)
	}
	h.refusedAll("a re-owned session", "c", "1")
	// An empty principal in the ref never matches, whatever the session.
	doc.Domains[0].CoordinatorSession.Principal = ""
	out, _ = json.Marshal(doc)
	if _, e := h.s.db.Exec(`UPDATE dispatcher_policies SET domains=?`, string(out)); e != nil {
		t.Fatal(e)
	}
	h.refusedAll("a ref with no principal", "c", "2")
	if _, e := h.s.db.Exec(`UPDATE sessions SET owner_principal='' WHERE name='c'`); e != nil {
		t.Fatal(e)
	}
	if h.linearCoordinator("c") {
		t.Fatal("an empty owner matched an empty ref")
	}
}

// An unreadable domains document is an error, never a grant, while the
// dispatcher holds the seat; with the dispatcher off it is not read at all.
func TestDomainCoordinatorUnreadableDocument(t *testing.T) {
	h := domainCoordHarness(t)
	if _, e := h.s.db.Exec(`UPDATE dispatcher_policies SET domains='not json'`); e != nil {
		t.Fatal(e)
	}
	_, err := h.s.Execute(context.Background(), h.request("issue_move_reserve", "c", moveArgs("m-1", teamTwo, teamOne)))
	var coded interface{ Code() string }
	if err == nil || errors.As(err, &coded) && coded.Code() == "forbidden" || !strings.Contains(err.Error(), "unreadable") {
		t.Fatal("not a plain error naming the document:", err)
	}
	// Dispatcher off: a human holds the seat, and the corrupt document is
	// never read.
	h2, _ := createsHarness(t)
	h2.s.SetIssueMoveProven(true)
	h2.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	var pid string
	if e := h2.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	if _, e := h2.s.db.Exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,domains,updated_at,updated_by_principal) VALUES(?,1,0,'not json','2026-01-01T00:00:00Z','x')`, pid); e != nil {
		t.Fatal(e)
	}
	h2.call("issue_move_reserve", "c", moveArgs("m-1", teamTwo, teamOne))
	for session, want := range map[string]bool{"c": true, "a": false} {
		out := h2.call("taskboard_access", session, map[string]any{"ledger": 1, "coordinator_scope": 1}).(map[string]any)
		if got, _ := out["coordinator"].(bool); got != want {
			t.Fatal(session, got, want)
		}
	}
}

// taskboard_access answers coordinator_scope true for the domain
// coordinator only.
func TestDomainCoordinatorTaskboardAccess(t *testing.T) {
	h := domainCoordHarness(t)
	scope := func(session string) bool {
		out := h.call("taskboard_access", session, map[string]any{"ledger": 1, "coordinator_scope": 1}).(map[string]any)
		got, _ := out["coordinator"].(bool)
		return got
	}
	if !scope("c") || scope("a") || scope("b") {
		t.Fatal("coordinator_scope:", scope("c"), scope("a"), scope("b"))
	}
}

// Assignment powers stay with the project's designated coordinator: a
// domain coordinator can't confirm another session's assignment.
func TestDomainCoordinatorKeepsNoAssignmentPowers(t *testing.T) {
	h := domainCoordHarness(t)
	got := h.propose("a", map[string]any{"issue_uuid": issueOne, "task_role": "engineer"})
	revision := got["assignment"].(map[string]any)["revision"].(string)
	e := h.errorOf("assignment_confirm", "c", map[string]any{"target_session": "a", "revision": revision})
	if !strings.Contains(e, "designated coordinator") {
		t.Fatal(e)
	}
}

// The seat must be the dispatcher's own system session: another system
// session in projects.coordinator_session, with the dispatcher enabled,
// grants the domain coordinator nothing.
func TestDomainCoordinatorNeedsTheDispatcherSeat(t *testing.T) {
	h := domainCoordHarness(t)
	if !h.linearCoordinator("c") {
		t.Fatal("the harness domain coordinator holds no rights")
	}
	// The seat's session keeps role=system but is no longer the dispatcher's.
	if res, e := h.s.db.Exec(`UPDATE sessions SET name='other-system' WHERE role='system' AND name=?`, DispatcherSessionName); e != nil {
		t.Fatal(e)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatal("renamed", n, "system sessions")
	}
	var seat string
	if e := h.s.db.QueryRow(`SELECT s.name FROM projects p JOIN sessions s ON s.id=p.coordinator_session`).Scan(&seat); e != nil || seat != "other-system" {
		t.Fatal(seat, e)
	}
	if h.linearCoordinator("c") {
		t.Fatal("a domain coordinator held rights under a non-dispatcher system seat")
	}
	h.refusedAll("a domain coordinator under another system seat", "c", "1")
}

// A corrupt domains document (readDispatcher's errDispatcherUnreadable, a
// plain error rather than a serviceError) leaves taskboard_access answering,
// with coordinator false; the reserve sites still error.
func TestDomainCoordinatorTaskboardAccessSurvivesCorruptDocument(t *testing.T) {
	h := domainCoordHarness(t)
	if _, e := h.s.db.Exec(`UPDATE dispatcher_policies SET domains='not json'`); e != nil {
		t.Fatal(e)
	}
	for _, session := range []string{"c", "a"} {
		out := h.call("taskboard_access", session, map[string]any{"ledger": 1, "coordinator_scope": 1}).(map[string]any)
		if got, _ := out["coordinator"].(bool); got {
			t.Fatal(session, "was told it is a coordinator")
		}
	}
	if e := h.errorOf("issue_move_reserve", "c", moveArgs("m-1", teamTwo, teamOne)); !strings.Contains(e, "unreadable") {
		t.Fatal(e)
	}
}
