package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// dsBegin and dsClaim are a set's begin and claim arguments to addProject,
// the project of the domain coordinator harness. The claim carries the
// server's own read of the project's teams.
func dsBegin(op string) map[string]any {
	return map[string]any{"linear_project": addProject, "set": true, "to_type": "started", "operation_id": op}
}

func dsClaim(op string, teams []string, more bool) map[string]any {
	a := dsBegin(op)
	delete(a, "set")
	a["expected_status_id"], a["from_type"], a["to_status_id"] = psPlanned, "planned", psStarted
	if teams != nil {
		a["project_teams"] = teams
	}
	if more {
		a["project_teams_more"] = true
	}
	return a
}

// statusDomainHarness is the domain coordinator harness with the operator's
// proof that the app sees a project's private teams, which a domain
// coordinator's status set needs.
func statusDomainHarness(t *testing.T) *harness {
	t.Helper()
	h := domainCoordHarness(t)
	h.s.SetProjectStatusDomainProven(true)
	return h
}

// statusClaimed claims a set and settles it refused at once, so the next
// claim of the project isn't held by a move in flight.
func (h *harness) statusClaimed(session string, a map[string]any) {
	h.t.Helper()
	got := psOK(h, "project_status_claim", session, a)
	settle := map[string]any{"linear_project": addProject, "to_type": "started", "operation_id": a["operation_id"], "expected_status_id": psPlanned, "seq": got["seq"], "outcome": "refused", "error": "app_scope"}
	psOK(h, "project_status_settle", session, settle)
}

func (h *harness) statusRefused(what, op, session string, a map[string]any, want string) {
	h.t.Helper()
	_, e := psStep(h, op, session, a)
	if code(e) != "forbidden" || !strings.Contains(e.Error(), want) {
		h.t.Fatal(what, ": ", op, " was not refused with ", want, ": ", e)
	}
}

// A domain coordinator sets a project's status while the dispatcher is on,
// when every team the project carries is a write team of its own lane. The
// lane bound is the claim's, before any Linear write; a begin lets it
// through, and a preview stays open to every session.
func TestDomainCoordinatorSetsStatusInItsLane(t *testing.T) {
	h := statusDomainHarness(t)
	psOK(h, "project_status_begin", "a", map[string]any{"linear_project": addProject})
	psOK(h, "project_status_begin", "c", dsBegin("op-1"))
	h.statusClaimed("c", dsClaim("op-1", []string{teamOne, teamTwo}, false))
	// Upper-case team IDs from the read compare lower-cased.
	h.statusClaimed("c", dsClaim("op-upper", []string{strings.ToUpper(teamOne)}, false))
	for what, c := range map[string]struct {
		teams []string
		more  bool
		want  string
	}{
		"cross-lane":     {[]string{teamOne, unboundTeam}, false, "outside_lane: the project carries team " + unboundTeam},
		"a list past 50": {[]string{teamOne}, true, "weren't read in full"},
		"no teams":       {[]string{}, false, "weren't read in full"},
		"no list":        {nil, false, "weren't read in full"},
	} {
		op := "op-" + strings.ReplaceAll(what, " ", "-")
		h.statusRefused(what, "project_status_claim", "c", dsClaim(op, c.teams, c.more), c.want)
		h.statusRefused(what, "project_status_claim", "c", dsClaim(op, c.teams, c.more), "lane both")
	}
	// Nothing was recorded for the refused claims.
	if n := count(t, h.s.db, "linear_write_ledger"); n != 2 {
		t.Fatal("ledger rows:", n)
	}
	// A lane that doesn't hold the project's teams.
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "c", "lane": "one"})
	h.statusRefused("lane one", "project_status_claim", "c", dsClaim("op-one", []string{teamOne, teamTwo}, false), "outside_lane")
	h.statusClaimed("c", dsClaim("op-one-ok", []string{teamOne}, false))
}

// Other sessions, the system session and a principal mismatch are refused
// at begin; people close where the policy says human, the domain
// coordinator included.
func TestDomainCoordinatorStatusRefusals(t *testing.T) {
	h := statusDomainHarness(t)
	for _, who := range []string{"a", "b"} {
		h.statusRefused(who, "project_status_begin", who, dsBegin("op-x"), "a domain's coordinator session (for a project whose teams are all in its lane)")
		h.statusRefused(who, "project_status_claim", who, dsClaim("op-x", []string{teamOne}, false), "designated coordinator session")
	}
	closing := dsBegin("op-close")
	closing["to_type"] = "completed"
	h.statusRefused("completed", "project_status_begin", "c", closing, "people close Linear project")
	// The dispatcher's own session, named as a domain's coordinator.
	var raw, sysID string
	if e := h.s.db.QueryRow(`SELECT domains FROM dispatcher_policies`).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if e := h.s.db.QueryRow(`SELECT id FROM sessions WHERE role='system' AND name=?`, DispatcherSessionName).Scan(&sysID); e != nil {
		t.Fatal(e)
	}
	var sysOwner string
	if e := h.s.db.QueryRow(`SELECT owner_principal FROM sessions WHERE id=?`, sysID).Scan(&sysOwner); e != nil {
		t.Fatal(e)
	}
	if _, e := h.s.db.Exec(`UPDATE sessions SET owner_principal='forged-owner@example.test' WHERE id=?`, sysID); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.s.db.Exec(`UPDATE sessions SET owner_principal=? WHERE id=?`, sysOwner, sysID) })
	var doc DispatcherDomains
	if e := json.Unmarshal([]byte(raw), &doc); e != nil {
		t.Fatal(e)
	}
	doc.Domains[0].CoordinatorSession = &DispatcherSessionRef{ID: sysID, Name: DispatcherSessionName, Principal: "forged-owner@example.test"}
	forged, _ := json.Marshal(doc)
	if _, e := h.s.db.Exec(`UPDATE dispatcher_policies SET domains=?`, string(forged)); e != nil {
		t.Fatal(e)
	}
	if h.linearCoordinator(DispatcherSessionName) {
		t.Fatal("the system session is a Linear coordinator")
	}
	if _, e := h.s.db.Exec(`UPDATE dispatcher_policies SET domains=?`, raw); e != nil {
		t.Fatal(e)
	}
	// A re-owned session.
	var doc2 DispatcherDomains
	_ = json.Unmarshal([]byte(raw), &doc2)
	doc2.Domains[0].CoordinatorSession.Principal = "someone-else@example.test"
	out, _ := json.Marshal(doc2)
	if _, e := h.s.db.Exec(`UPDATE dispatcher_policies SET domains=?`, string(out)); e != nil {
		t.Fatal(e)
	}
	h.statusRefused("principal mismatch", "project_status_begin", "c", dsBegin("op-p"), "domain's coordinator session")
	if _, e := h.s.db.Exec(`UPDATE dispatcher_policies SET domains=?`, raw); e != nil {
		t.Fatal(e)
	}
	psOK(h, "project_status_begin", "c", dsBegin("op-p"))
}

// The right follows the domain document: after a coordinator edit the old
// one is refused; after disable a session that was only a domain
// coordinator is refused and the restored project coordinator passes,
// unbounded by teams.
func TestDomainCoordinatorStatusFollowsEditAndDisable(t *testing.T) {
	h := statusDomainHarness(t)
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "a", "lane": "both"})
	h.statusRefused("a before it was named", "project_status_begin", "a", dsBegin("op-0"), "domain's coordinator session")
	h.setCoordinator("a")
	h.statusRefused("the old coordinator", "project_status_begin", "c", dsBegin("op-1"), "domain's coordinator session")
	psOK(h, "project_status_begin", "a", dsBegin("op-2"))
	off, err := h.switchCall("dispatcher_disable", map[string]any{"expected_revision": h.domainRevision()})
	if err != nil {
		t.Fatal(err)
	}
	restored := off["coordinator"].(string)
	if restored == "a" {
		t.Fatal("the restored coordinator is the former domain coordinator")
	}
	h.statusRefused("a former domain coordinator after disable", "project_status_begin", "a", dsBegin("op-3"), "designated coordinator session")
	psOK(h, "project_status_begin", restored, dsBegin("op-4"))
	// The project coordinator's set is unaffected by the teams arguments.
	psOK(h, "project_status_claim", restored, dsClaim("op-4", []string{teamOne, unboundTeam}, true))
}

// The seat's holder sets unbounded, the dispatcher's system session
// included: neither begin nor a cross-lane or partial claim is refused by the
// setter. (A request never addresses a system session, so the setter is
// called directly, as the bus does with the actor row.)
func TestDispatcherSeatSetsStatusUnbounded(t *testing.T) {
	h := statusDomainHarness(t)
	ctx := context.Background()
	raw, err := h.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Rollback()
	tx := &transaction{Tx: raw}
	actor, err := one(ctx, tx, `SELECT * FROM sessions WHERE role='system' AND name=?`, DispatcherSessionName)
	if err != nil {
		t.Fatal(err)
	}
	p, err := one(ctx, tx, `SELECT * FROM projects WHERE id=?`, str(actor, "project_id"))
	if err != nil || p["coordinator_session"] != actor["id"] {
		t.Fatal("the dispatcher is not in the seat:", err)
	}
	pid := str(p, "id")
	for op, a := range map[string]map[string]any{
		"project_status_begin": dsBegin("op-seat"),
		"project_status_claim": dsClaim("op-seat", []string{teamOne, unboundTeam}, true),
	} {
		if e := h.s.projectStatusSetter(ctx, tx, pid, p, "", actor, op, a); e != nil {
			t.Fatal(op, e)
		}
	}
	// A domain coordinator in the same state is bounded.
	c, err := one(ctx, tx, `SELECT * FROM sessions WHERE name='c'`)
	if err != nil {
		t.Fatal(err)
	}
	if e := h.s.projectStatusSetter(ctx, tx, pid, p, "", c, "project_status_claim", dsClaim("op-seat", []string{teamOne, unboundTeam}, false)); e == nil || !strings.Contains(e.Error(), "outside_lane") {
		t.Fatal(e)
	}
}

// A claim by a domain coordinator is outside_lane when the binding has no
// lanes, and when the session's lane is no longer one of the binding's.
func TestDomainCoordinatorStatusNeedsALane(t *testing.T) {
	h := statusDomainHarness(t)
	if _, e := h.s.db.Exec(`UPDATE lane_pins SET lane='gone' WHERE pin_key='c'`); e != nil {
		t.Fatal(e)
	}
	psOK(h, "project_status_begin", "c", dsBegin("op-1"))
	h.statusRefused("a pin to a lane the binding lacks", "project_status_claim", "c", dsClaim("op-1", []string{teamOne}, false), "outside_lane: the coordinator session has no lane")
	if _, e := h.s.db.Exec(`UPDATE lane_pins SET lane='both' WHERE pin_key='c'`); e != nil {
		t.Fatal(e)
	}
	h.statusClaimed("c", dsClaim("op-2", []string{teamOne}, false))
	var rev int
	if e := h.s.db.QueryRow(`SELECT revision FROM taskboard_bindings`).Scan(&rev); e != nil {
		t.Fatal(e)
	}
	h.call("taskboard_set_v2", "", taskboardArgsV2(rev, addProject))
	h.statusRefused("a binding without lanes", "project_status_claim", "c", dsClaim("op-3", []string{teamOne}, false), "outside_lane: this binding has no lanes")
}

// While the operator hasn't proven that the app sees a project's private
// teams, a domain coordinator's set is refused at begin and at the claim,
// before any Linear write; the seat's, the owner's and a lead's sets and a
// plain read are unaffected.
func TestDomainCoordinatorStatusWaitsForTheProbe(t *testing.T) {
	h := domainCoordHarness(t)
	psOK(h, "project_status_begin", "c", map[string]any{"linear_project": addProject})
	h.statusRefused("begin", "project_status_begin", "c", dsBegin("op-1"), "project_status_domain_unproven")
	h.statusRefused("claim", "project_status_claim", "c", dsClaim("op-1", []string{teamOne}, false), "project_status_domain_unproven")
	if n := count(t, h.s.db, "linear_write_ledger"); n != 0 {
		t.Fatal("ledger rows:", n)
	}
	ctx := context.Background()
	raw, err := h.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Rollback()
	tx := &transaction{Tx: raw}
	actor, err := one(ctx, tx, `SELECT * FROM sessions WHERE role='system' AND name=?`, DispatcherSessionName)
	if err != nil {
		t.Fatal(err)
	}
	p, err := one(ctx, tx, `SELECT * FROM projects WHERE id=?`, str(actor, "project_id"))
	if err != nil || p["coordinator_session"] != actor["id"] {
		t.Fatal("the dispatcher is not in the seat:", err)
	}
	pid := str(p, "id")
	for _, op := range []string{"project_status_begin", "project_status_claim"} {
		a := dsBegin("op-seat")
		if op == "project_status_claim" {
			a = dsClaim("op-seat", []string{teamOne, unboundTeam}, true)
		}
		if e := h.s.projectStatusSetter(ctx, tx, pid, p, "", actor, op, a); e != nil {
			t.Fatal("seat", op, e)
		}
		for _, role := range []string{"owner", "lead"} {
			if e := h.s.projectStatusSetter(ctx, tx, pid, p, role, nil, op, a); e != nil {
				t.Fatal(role, op, e)
			}
		}
	}
	raw.Rollback()
	h.s.SetProjectStatusDomainProven(true)
	psOK(h, "project_status_begin", "c", dsBegin("op-2"))
}
