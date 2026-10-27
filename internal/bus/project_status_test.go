package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// unboundProject is a Linear project of the same workspace that this
// project's binding doesn't hold.
const unboundProject = "33333333-3333-4333-8333-333333333333"

// psStep runs a project status step as the server's AppWriter does.
func psStep(h *harness, op, session string, a map[string]any) (map[string]any, error) {
	copied := map[string]any{}
	for k, v := range a {
		copied[k] = v
	}
	r, e := h.s.Execute(WithAppStep(context.Background()), h.request(op, session, copied))
	m, _ := r.Data.(map[string]any)
	return m, e
}

func psOK(h *harness, op, session string, a map[string]any) map[string]any {
	h.t.Helper()
	m, e := psStep(h, op, session, a)
	if e != nil {
		h.t.Fatal(op, e)
	}
	return m
}

// psSet is a set's arguments to projectOne, with its claim fields.
func psSet(to, opID string) map[string]any {
	return map[string]any{"linear_project": projectOne, "to_type": to, "operation_id": opID, "expected_status_id": psPlanned}
}

func psClaim(to, opID, status string) map[string]any {
	a := psSet(to, opID)
	a["from_type"], a["to_status_id"] = "planned", status
	return a
}

func psSettle(to, opID string, seq int, outcome, code string) map[string]any {
	a := psSet(to, opID)
	a["seq"], a["outcome"], a["error"] = seq, outcome, code
	return a
}

func (h *healthHarness) psBudget() int {
	h.t.Helper()
	var n int
	if e := h.s.db.QueryRow(`SELECT budget_used FROM watch_lease`).Scan(&n); e != nil {
		h.t.Fatal(e)
	}
	return n
}

func (h *healthHarness) sessionID(name string) string {
	h.t.Helper()
	var id string
	if e := h.s.db.QueryRow(`SELECT id FROM sessions WHERE name=?`, name).Scan(&id); e != nil {
		h.t.Fatal(e)
	}
	return id
}

// The steps are the server's: a client calling them is refused.
func TestProjectStatusIsTheServersStep(t *testing.T) {
	h := newHealthHarness(t)
	for op, a := range map[string]map[string]any{
		"project_status_begin":  {"linear_project": projectOne},
		"project_status_claim":  psClaim("started", "op-1", psStarted),
		"project_status_settle": psSettle("started", "op-1", 1, "accepted", ""),
	} {
		_, e := h.s.Execute(context.Background(), h.request(op, "c", a))
		if code(e) != "forbidden" || !strings.Contains(e.Error(), "server runs as its ClawBridge app") {
			t.Fatal(op, e)
		}
	}
	if n := count(t, h.s.db, "linear_write_ledger"); n != 0 {
		t.Fatal("a refused step wrote", n)
	}
}

// Any session reads; only the designated coordinator session, or a person
// without a session, sets. A retired session does neither.
func TestProjectStatusWhoSets(t *testing.T) {
	h := newHealthHarness(t)
	read := psOK(h.harness, "project_status_begin", "a", map[string]any{"linear_project": projectOne})
	if read["flow"] != "off" || read["agent_closes"] != false {
		t.Fatal(read)
	}
	if _, e := psStep(h.harness, "project_status_begin", "a", map[string]any{"linear_project": projectOne, "set": true, "to_type": "started", "operation_id": "op-1"}); code(e) != "forbidden" || !strings.Contains(e.Error(), "designated coordinator session") {
		t.Fatal(e)
	}
	if _, e := psStep(h.harness, "project_status_claim", "a", psClaim("started", "op-1", psStarted)); code(e) != "forbidden" {
		t.Fatal("a claim from another session:", e)
	}
	psOK(h.harness, "project_status_begin", "c", map[string]any{"linear_project": projectOne, "set": true, "to_type": "started", "operation_id": "op-1"})
	psOK(h.harness, "project_status_begin", "", map[string]any{"linear_project": projectOne, "set": true, "to_type": "started", "operation_id": "op-1"})
	if _, e := h.s.db.Exec(`UPDATE sessions SET status='retired' WHERE name='b'`); e != nil {
		t.Fatal(e)
	}
	if _, e := psStep(h.harness, "project_status_begin", "b", map[string]any{"linear_project": projectOne}); e == nil {
		t.Fatal("a retired session read")
	}
	if _, e := psStep(h.harness, "project_status_begin", "c", map[string]any{"linear_project": projectOne, "set": true, "to_type": "paused", "operation_id": "op-2"}); e == nil {
		t.Fatal("a set to paused")
	}
}

// M1: a Linear project of the same workspace that this project's binding
// doesn't hold is refused at every step before any charge or record.
func TestProjectStatusNeedsTheBinding(t *testing.T) {
	h := newHealthHarness(t)
	before := h.psBudget()
	for _, op := range []string{"project_status_begin", "project_status_claim"} {
		a := psClaim("started", "op-1", psStarted)
		a["linear_project"] = unboundProject
		if op == "project_status_begin" {
			a = map[string]any{"linear_project": unboundProject}
		}
		if _, e := psStep(h.harness, op, "c", a); code(e) != "forbidden" || !strings.Contains(e.Error(), "not in this project's binding") {
			t.Fatal(op, e)
		}
	}
	if h.psBudget() != before || count(t, h.s.db, "linear_write_ledger") != 0 || count(t, h.s.db, "project_flow_moves") != 0 {
		t.Fatal("a refused step charged or recorded")
	}
	// The binding is read in the claim's own transaction: dropped after a
	// begin, the claim is refused.
	psOK(h.harness, "project_status_begin", "c", map[string]any{"linear_project": projectOne, "set": true, "to_type": "started", "operation_id": "op-2"})
	h.call("taskboard_set_v2", "a", taskboardArgsV2(1, projectTwo))
	if _, e := psStep(h.harness, "project_status_claim", "c", psClaim("started", "op-2", psStarted)); code(e) != "forbidden" {
		t.Fatal("a claim after the binding dropped the project:", e)
	}
}

// The project's close rule decides who sets completed or canceled: where
// people close, no session does, the coordinator's included.
func TestProjectStatusFollowsWhoCloses(t *testing.T) {
	for _, closes := range []string{"human", "agent"} {
		h := newFlowHarness(t, closes)
		for _, to := range []string{"completed", "canceled"} {
			a := map[string]any{"linear_project": projectOne, "set": true, "to_type": to, "operation_id": "op-" + to}
			_, e := psStep(h.harness, "project_status_begin", "c", a)
			if closes == "agent" && e != nil {
				t.Fatal(closes, to, e)
			}
			if closes == "human" && (code(e) != "forbidden" || !strings.Contains(e.Error(), "people close Linear project") || !strings.Contains(e.Error(), "no session")) {
				t.Fatal(closes, to, e)
			}
			claim, e := psStep(h.harness, "project_status_claim", "c", psClaim(to, "op-"+to, psCompleted))
			if (e == nil) != (closes == "agent") {
				t.Fatal("claim", closes, to, e)
			}
			if e == nil {
				psOK(h.harness, "project_status_settle", "c", psSettle(to, "op-"+to, claim["seq"].(int), "refused", "app_scope"))
			}
			// A person always may.
			psOK(h.harness, "project_status_begin", "", a)
		}
		if _, e := psStep(h.harness, "project_status_begin", "c", map[string]any{"linear_project": projectOne, "set": true, "to_type": "started", "operation_id": "op-s"}); e != nil {
			t.Fatal("started is the coordinator's either way:", e)
		}
	}
}

// A read is charged its calls up front, a set its read, write and
// read-back; past the hour's budget, rate_limited and nothing charged.
func TestProjectStatusChargesTheWatchBudget(t *testing.T) {
	h := newHealthHarness(t)
	before := h.psBudget()
	psOK(h.harness, "project_status_begin", "a", map[string]any{"linear_project": projectOne})
	if h.psBudget() != before+ProjectStatusReadCalls {
		t.Fatal(h.psBudget(), before)
	}
	preview := map[string]any{"linear_project": projectOne, "set": true, "to_type": "started", "operation_id": "op-1"}
	psOK(h.harness, "project_status_begin", "c", preview)
	if h.psBudget() != before+2*ProjectStatusReadCalls {
		t.Fatal("a preview is charged its read only:", h.psBudget())
	}
	preview["confirm"] = true
	psOK(h.harness, "project_status_begin", "c", preview)
	if h.psBudget() != before+3*ProjectStatusReadCalls+ProjectStatusSetCalls {
		t.Fatal(h.psBudget())
	}
	h.call("comment_watch_budget_set", "", map[string]any{"watch_budget": h.psBudget() + ProjectStatusReadCalls - 1})
	used := h.psBudget()
	if _, e := psStep(h.harness, "project_status_begin", "a", map[string]any{"linear_project": projectOne}); code(e) != "rate_limited" || !strings.Contains(e.Error(), "budget for the hour is spent") {
		t.Fatal(e)
	}
	if h.psBudget() != used {
		t.Fatal("a refused read was charged")
	}
}

// A project no watcher ever ran for still pays: an expired lease row
// carries the hour's count, and a watcher takes it over with its charge.
func TestProjectStatusChargesWithoutAWatcher(t *testing.T) {
	h := newHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0, projectOne, projectTwo))
	psOK(h, "project_status_begin", "a", map[string]any{"linear_project": projectOne})
	var used, gen int
	var expires string
	if e := h.s.db.QueryRow(`SELECT budget_used,generation,expires_at FROM watch_lease`).Scan(&used, &gen, &expires); e != nil || used != ProjectStatusReadCalls || gen != 0 || expires != "" {
		t.Fatal(used, gen, expires, e)
	}
	var pid string
	_ = h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid)
	r, e := h.s.ExecuteSystem(context.Background(), pid, Request{Operation: "comment_watch_lease", Args: map[string]any{"action": "acquire"}})
	if e != nil || r.Data.(map[string]any)["budget_used"] != ProjectStatusReadCalls {
		t.Fatal(r.Data, e)
	}
}

// A set end to end with flow on: the claim records the move, holds flow
// and is audited before anything is sent; flow then moves nothing; the
// settle records the outcome once, naming the coordinator's session; the
// same operation_id answers from the record and costs nothing.
func TestProjectStatusClaimHoldsFlowAndSettles(t *testing.T) {
	h := newFlowHarness(t, "human")
	claim := psOK(h.harness, "project_status_claim", "c", psClaim("started", "op-1", psStarted))
	if claim["seq"] != 1 || claim["flow"] != "held" {
		t.Fatal(claim)
	}
	if got := h.moves(); len(got) != 1 || got[0] != "1 manual started "+psStarted+" pending" {
		t.Fatal(got)
	}
	// M3: flow sees the pending move and the hold, and sends nothing.
	out := h.run(flowFacts(psPlanned, issues{total: 2, started: 1}))
	if moveOf(out) != nil || len(h.moves()) != 1 {
		t.Fatal("flow moved during a set:", out, h.moves())
	}
	settled := psOK(h.harness, "project_status_settle", "c", psSettle("started", "op-1", 1, "accepted", ""))
	if settled["state"] != "accepted" || settled["flow"] != "held" {
		t.Fatal(settled)
	}
	if got := h.moves(); got[0] != "1 manual started "+psStarted+" accepted" {
		t.Fatal(got)
	}
	var seen, session, details string
	_ = h.s.db.QueryRow(`SELECT last_seen_type FROM project_flow_state`).Scan(&seen)
	if e := h.s.db.QueryRow(`SELECT changed_by_session,details FROM history WHERE entity='project_status' AND action='set'`).Scan(&session, &details); e != nil || session != h.sessionID("c") {
		t.Fatal(session, e)
	}
	if seen != "started" || !strings.Contains(details, `"outcome":"accepted"`) || !strings.Contains(details, `"operation_id":"op-1"`) {
		t.Fatal(seen, details)
	}
	before := h.psBudget()
	replay := psOK(h.harness, "project_status_begin", "c", map[string]any{"linear_project": projectOne, "set": true, "to_type": "started", "operation_id": "op-1", "expected_status_id": psPlanned})
	if rec, _ := replay["recorded"].(map[string]any); rec["state"] != "accepted" || h.psBudget() != before {
		t.Fatal(replay, h.psBudget(), before)
	}
	if _, e := psStep(h.harness, "project_status_settle", "c", psSettle("started", "op-1", 1, "accepted", "")); code(e) != "conflict" {
		t.Fatal("a second settle:", e)
	}
	// status-policy show says the last move was set by hand.
	view := h.call("status_policy_get", "", nil).(map[string]any)
	raw, _ := json.Marshal(view)
	if !strings.Contains(string(raw), "set by hand") {
		t.Fatal(string(raw))
	}
	// Flow stays held until project-flow resume.
	if out = h.run(flowFacts(psStarted, issues{total: 2, completed: 2})); moveOf(out) != nil {
		t.Fatal(out)
	}
}

// M3: a flow move in flight refuses a set, and only one move is ever
// pending; an hour later the unconfirmed move is failed, never re-sent.
func TestProjectStatusWaitsForAFlowMoveInFlight(t *testing.T) {
	h := newFlowHarness(t, "human")
	if move := moveOf(h.run(flowFacts(psPlanned, issues{total: 2, started: 1}))); move == nil {
		t.Fatal("no flow move")
	}
	if _, e := psStep(h.harness, "project_status_claim", "c", psClaim("backlog", "op-1", psBacklog)); code(e) != "conflict" || !strings.Contains(e.Error(), "in flight") {
		t.Fatal(e)
	}
	if got := h.moves(); len(got) != 1 || count(t, h.s.db, "linear_write_ledger") != 0 {
		t.Fatal(got)
	}
	if _, e := h.s.db.Exec(`UPDATE project_flow_moves SET sent_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		t.Fatal(e)
	}
	psOK(h.harness, "project_status_claim", "c", psClaim("backlog", "op-1", psBacklog))
	if got := h.moves(); len(got) != 2 || !strings.HasSuffix(got[0], " failed") || !strings.HasSuffix(got[1], " pending") {
		t.Fatal(got)
	}
}

// M4: an operation_id is one request of one caller. Another request under
// it is refused; one still sending is in progress; one never settled turns
// uncertain and is never claimed again; another caller's is its own.
func TestProjectStatusLedger(t *testing.T) {
	h := newHealthHarness(t)
	psOK(h.harness, "project_status_claim", "c", psClaim("started", "op-1", psStarted))
	other := psClaim("planned", "op-1", psPlanned)
	if _, e := psStep(h.harness, "project_status_claim", "c", other); e == nil || !strings.Contains(e.Error(), "different project status request") {
		t.Fatal(e)
	}
	if _, e := psStep(h.harness, "project_status_begin", "c", map[string]any{"linear_project": projectOne, "set": true, "to_type": "started", "operation_id": "op-1", "expected_status_id": psPlanned}); code(e) != "in_progress" {
		t.Fatal(e)
	}
	if _, e := h.s.db.Exec(`UPDATE linear_write_ledger SET record=replace(record,substr(record,instr(record,'"at":"')+6,30),'2000-01-01T00:00:00.000000000Z')`); e != nil {
		t.Fatal(e)
	}
	again := psOK(h.harness, "project_status_claim", "c", psClaim("started", "op-1", psStarted))
	if rec, _ := again["recorded"].(map[string]any); rec["state"] != "uncertain" || rec["error"] != "unsettled" {
		t.Fatal(again)
	}
	if count(t, h.s.db, "project_flow_moves") != 1 {
		t.Fatal("an unsettled set was claimed again")
	}
	// The person's op-1 is not the coordinator's.
	if mine := psOK(h.harness, "project_status_begin", "", map[string]any{"linear_project": projectOne, "set": true, "to_type": "planned", "operation_id": "op-1"}); mine["recorded"] != nil {
		t.Fatal(mine)
	}
}

// A refused set reached nothing: it lifts the hold its claim put on. An
// uncertain one keeps it, fails its move and answers uncertain for good.
func TestProjectStatusRefusedAndUncertain(t *testing.T) {
	h := newFlowHarness(t, "human")
	psOK(h.harness, "project_status_claim", "c", psClaim("started", "op-1", psStarted))
	for _, bad := range []map[string]any{
		psSettle("started", "op-1", 1, "accepted", "app_scope"),
		psSettle("started", "op-1", 1, "refused", ""),
		psSettle("started", "op-1", 1, "refused", "Linear said: no"),
		psSettle("started", "op-1", 1, "gone", "x"),
		psSettle("started", "op-1", 2, "refused", "app_scope"),
	} {
		if _, e := psStep(h.harness, "project_status_settle", "c", bad); e == nil {
			t.Fatal("accepted", bad)
		}
	}
	if out := psOK(h.harness, "project_status_settle", "c", psSettle("started", "op-1", 1, "refused", "app_scope")); out["flow"] != "on" {
		t.Fatal("a refused set kept the hold:", out)
	}
	var seen string
	if e := h.s.db.QueryRow(`SELECT last_seen_type FROM project_flow_state`).Scan(&seen); e != nil || seen != "" {
		t.Fatal("a refused set counted as seen:", seen, e)
	}
	psOK(h.harness, "project_status_claim", "c", psClaim("started", "op-2", psStarted))
	if out := psOK(h.harness, "project_status_settle", "c", psSettle("started", "op-2", 2, "uncertain", "read_back_mismatch")); out["flow"] != "held" {
		t.Fatal(out)
	}
	if got := h.moves(); got[1] != "2 manual started "+psStarted+" failed" {
		t.Fatal(got)
	}
	replay := psOK(h.harness, "project_status_claim", "c", psClaim("started", "op-2", psStarted))
	if rec, _ := replay["recorded"].(map[string]any); rec["state"] != "uncertain" || rec["error"] != "read_back_mismatch" {
		t.Fatal(replay)
	}
	// M6: the audit holds types, IDs and codes, never a status name.
	rows, _ := h.s.db.Query(`SELECT details FROM history WHERE entity='project_status'`)
	defer rows.Close()
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		var m map[string]any
		if json.Unmarshal([]byte(d), &m) != nil || m["name"] != nil {
			t.Fatal(d)
		}
	}
}
