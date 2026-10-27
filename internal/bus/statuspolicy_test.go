package bus

import (
	"context"
	"strings"
	"testing"
)

const (
	sfTeam      = "11111111-1111-4111-8111-111111111111"
	sfBacklog   = "22222222-2222-4222-8222-2222222222b1"
	sfTodo      = "22222222-2222-4222-8222-2222222222b2"
	sfProgress  = "22222222-2222-4222-8222-2222222222b3"
	sfReview    = "22222222-2222-4222-8222-2222222222b4"
	sfDone      = "22222222-2222-4222-8222-2222222222b5"
	sfIssueNext = "00000000-0000-4000-8000-0000000000f2"
)

func sfPolicy(enabled bool) map[string]any {
	return map[string]any{"enabled": enabled, "teams": map[string]any{sfTeam: map[string]any{
		"create":       map[string]any{"id": sfTodo, "name": "Todo", "type": "unstarted"},
		"claim":        map[string]any{"id": sfProgress, "name": "In Progress", "type": "started"},
		"release_done": map[string]any{"id": sfReview, "name": "In Review", "type": "started"},
		"workflow":     map[string]any{sfBacklog: "backlog", sfTodo: "unstarted", sfProgress: "started", sfReview: "started", sfDone: "completed"},
	}}}
}

func (w *watchHarness) setStatusPolicy(rev int, policy map[string]any) map[string]any {
	w.t.Helper()
	return w.call("status_policy_set", "c", map[string]any{"expected_revision": rev, "status_policy": policy}).(map[string]any)
}

func (w *watchHarness) stateRow(id, phase string) (state, reason string, revision int) {
	w.t.Helper()
	if e := w.s.db.QueryRow(`SELECT state_state,state_skip_reason,state_policy_revision FROM assignment_markers WHERE assignment_id=? AND phase=?`, id, phase).Scan(&state, &reason, &revision); e != nil {
		w.t.Fatal(phase, e)
	}
	return
}

func TestStatusPolicyRulesAndRevisions(t *testing.T) {
	w := newWatchHarness(t)
	got := w.call("status_policy_get", "a", map[string]any{}).(map[string]any)
	if got["revision"] != int64(0) || got["enabled"] != false {
		t.Fatal("default policy", got)
	}
	bad := func(edit func(team map[string]any)) map[string]any {
		p := sfPolicy(true)
		edit(p["teams"].(map[string]any)[sfTeam].(map[string]any))
		return p
	}
	for name, p := range map[string]map[string]any{
		"claim completed": bad(func(t map[string]any) { t["claim"] = map[string]any{"id": sfDone, "name": "Done", "type": "completed"} }),
		"create started": bad(func(t map[string]any) {
			t["create"] = map[string]any{"id": sfProgress, "name": "In Progress", "type": "started"}
		}),
		"type mismatch": bad(func(t map[string]any) {
			t["claim"] = map[string]any{"id": sfProgress, "name": "In Progress", "type": "unstarted"}
		}),
		"not in workflow":  bad(func(t map[string]any) { delete(t["workflow"].(map[string]any), sfReview) }),
		"release no claim": bad(func(t map[string]any) { delete(t, "claim") }),
		"release = claim":  bad(func(t map[string]any) { t["release_done"] = t["claim"] }),
		"mention in name":  bad(func(t map[string]any) { t["claim"].(map[string]any)["name"] = "@ops" }),
		"unknown key":      bad(func(t map[string]any) { t["extra"] = true }),
	} {
		if _, err := w.s.Execute(context.Background(), w.request("status_policy_set", "c", map[string]any{"expected_revision": 0, "status_policy": p})); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	set := w.setStatusPolicy(0, sfPolicy(true))
	if set["revision"] != int64(1) || set["enabled"] != true {
		t.Fatal(set)
	}
	// A member may read it; only the owner may set it, at its revision.
	if _, err := w.s.Execute(context.Background(), w.request("status_policy_set", "c", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(false)})); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatal("stale revision", err)
	}
	var n int
	if e := w.s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE entity='linear_status_policy' AND action='set'`).Scan(&n); e != nil || n != 1 {
		t.Fatal("audit", n, e)
	}
}

// A2: done is a completed state and needs a claim; closes maps canonical
// project UUIDs to agent or human; the view shows closes; a policy whose
// teams move to done only still gives a release a state part.
func TestStatusPolicyDoneAndCloses(t *testing.T) {
	const project = "3333aaaa-3333-4333-8333-333333333333"
	with := func(edit func(p, team map[string]any)) map[string]any {
		p := sfPolicy(true)
		team := p["teams"].(map[string]any)[sfTeam].(map[string]any)
		team["done"] = map[string]any{"id": sfDone, "name": "Done", "type": "completed"}
		p["closes"] = map[string]any{project: "agent"}
		edit(p, team)
		return p
	}
	for name, p := range map[string]map[string]any{
		"done started":     with(func(_, t map[string]any) { t["done"] = t["release_done"] }),
		"done no claim":    with(func(_, t map[string]any) { delete(t, "claim"); delete(t, "release_done") }),
		"closes bad value": with(func(p, _ map[string]any) { p["closes"] = map[string]any{project: "bot"} }),
		"closes not uuid":  with(func(p, _ map[string]any) { p["closes"] = map[string]any{"FIX": "agent"} }),
		"closes upper":     with(func(p, _ map[string]any) { p["closes"] = map[string]any{strings.ToUpper(project): "agent"} }),
	} {
		if _, err := ParseStatusPolicy(p); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	w := newWatchHarness(t)
	set := w.setStatusPolicy(0, with(func(_, t map[string]any) { delete(t, "release_done") }))
	if set["closes"].(map[string]string)[project] != "agent" {
		t.Fatal(set)
	}
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	w.call("assignment_release", "b", map[string]any{"revision": w.live("b")["revision"], "outcome": "done"})
	if s, _, _ := w.stateRow(id, "close"); s != "pending" {
		t.Fatal("a done-only policy gave the release no state part:", s)
	}
}

// The claim row freezes the policy revision; the state part is handed out
// only while the policy is enabled at that revision (SF-M1).
func TestStateMoveFreezesThePolicyRevision(t *testing.T) {
	w := newWatchHarness(t)
	old := w.assignmentID("a")
	if s, _, _ := w.stateRow(old, "claim"); s != "off" {
		t.Fatal("a claim before any policy has a state part:", s)
	}
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	if s, _, rev := w.stateRow(id, "claim"); s != "pending" || rev != 1 {
		t.Fatal("claim state part", s, rev)
	}
	w.markersDue()
	g := w.markerGo("c", id, "claim", "state")
	if g["action"] != "move" || g["kind"] != "claim" || g["policy_revision"] != int64(1) {
		t.Fatal(g)
	}
	if s, _, _ := w.stateRow(id, "claim"); s != "in_flight" {
		t.Fatal("not in flight:", s)
	}
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress})
	if s, _, _ := w.stateRow(id, "claim"); s != "done" {
		t.Fatal(s)
	}
	// A revision bump between the release and its apply skips the move.
	w.call("assignment_release", "b", map[string]any{"revision": w.live("b")["revision"], "outcome": "done"})
	if s, _, rev := w.stateRow(id, "close"); s != "pending" || rev != 1 {
		t.Fatal("close state part", s, rev)
	}
	w.setStatusPolicy(1, sfPolicy(true))
	w.markersDue()
	if g = w.markerGo("c", id, "close", "state"); g["action"] != "none" || g["reason"] != "policy_changed" {
		t.Fatal(g)
	}
	if s, reason, _ := w.stateRow(id, "close"); s != "skipped" || reason != "policy_changed" {
		t.Fatal(s, reason)
	}
}

func TestStateMoveSkipsADisabledPolicyAndWaitsForTheClaim(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	w.markersDue()
	// The claim move is handed out but not settled: the close waits.
	w.markerGo("c", id, "claim", "state")
	w.call("assignment_release", "c", map[string]any{"target_session": "b", "revision": w.live("b")["revision"], "outcome": "done"})
	w.markersDue()
	if g := w.markerGo("c", id, "close", "state"); g["action"] != "wait" {
		t.Fatal("the close did not wait for the claim:", g)
	}
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "skipped", "skip_reason": "from_state"})
	w.setStatusPolicy(1, sfPolicy(false))
	w.markersDue()
	if g := w.markerGo("c", id, "close", "state"); g["action"] != "none" || g["reason"] != "policy_off" {
		t.Fatal(g)
	}
}

// A claim move never handed out is superseded once its close exists; a
// release without done never moves.
func TestStateMoveSupersededAndOnlyOnDone(t *testing.T) {
	w := newWatchHarness(t)
	// An enabled policy without a claim target moves nothing at a claim.
	createOnly := sfPolicy(true)
	team := createOnly["teams"].(map[string]any)[sfTeam].(map[string]any)
	delete(team, "claim")
	delete(team, "release_done")
	w.setStatusPolicy(0, createOnly)
	w.assign("b", sfIssueNext)
	if s, _, _ := w.stateRow(w.assignmentID("b"), "claim"); s != "off" {
		t.Fatal("a claim without a claim target has a state part:", s)
	}
	w.release("b")
	w.setStatusPolicy(1, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	w.release("b")
	if s, _, _ := w.stateRow(id, "close"); s != "off" {
		t.Fatal("a release without done has a state part:", s)
	}
	w.markersDue()
	if g := w.markerGo("c", id, "claim", "state"); g["action"] != "none" || g["state"] != "superseded" {
		t.Fatal(g)
	}
	w.assign("b", sfIssueNext)
	if _, err := w.s.Execute(context.Background(), w.request("assignment_release", "b", map[string]any{"revision": w.live("b")["revision"], "outcome": "finished"})); err == nil {
		t.Fatal("an unknown outcome was accepted")
	}
}

// A1: a release that overtakes its claim's move supersedes it, and the
// close's move carries how the claim settled and the revision it froze, so
// the applier may move from the claim's from-set under the same policy only.
func TestCloseMoveCarriesTheClaimsOutcome(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	race := func(bump bool) map[string]any {
		w.assign("b", sfIssueNext)
		id := w.assignmentID("b")
		if bump {
			w.setStatusPolicy(int(w.call("status_policy_get", "a", map[string]any{}).(map[string]any)["revision"].(int64)), sfPolicy(true))
		}
		w.call("assignment_release", "b", map[string]any{"revision": w.live("b")["revision"], "outcome": "done"})
		w.markersDue()
		if g := w.markerGo("c", id, "claim", "state"); g["state"] != "superseded" {
			t.Fatal("claim not superseded", g)
		}
		return w.markerGo("c", id, "close", "state")
	}
	if g := race(false); g["action"] != "move" || g["claim_state"] != "superseded" || g["claim_policy_revision"] != int64(1) || g["policy_revision"] != int64(1) {
		t.Fatal("the race", g)
	}
	// A1-L1: the policy changed between the confirm and the release.
	if g := race(true); g["action"] != "move" || g["claim_state"] != "superseded" || g["claim_policy_revision"] != int64(1) || g["policy_revision"] != int64(2) {
		t.Fatal("policy changed", g)
	}
	// A claim that landed hands its close "done".
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	w.markersDue()
	w.markerGo("c", id, "claim", "state")
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress})
	w.call("assignment_release", "b", map[string]any{"revision": w.live("b")["revision"], "outcome": "done"})
	w.markersDue()
	if g := w.markerGo("c", id, "close", "state"); g["action"] != "move" || g["claim_state"] != "done" {
		t.Fatal("claim landed", g)
	}
}

func TestStateSettleRules(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	w.markersDue()
	if _, err := w.s.Execute(context.Background(), w.request("marker_settle", "c", map[string]any{"assignment_id": id, "phase": "claim", "part": "state", "outcome": "done", "state_target": sfProgress, "state_landed": sfProgress})); err == nil {
		t.Fatal("a part never handed out settled")
	}
	w.markerGo("c", id, "claim", "state")
	for _, a := range []map[string]any{
		{"outcome": "done", "state_target": sfProgress},
		{"outcome": "skipped", "skip_reason": "because"},
		{"outcome": "no_label"},
	} {
		a["assignment_id"], a["phase"], a["part"] = id, "claim", "state"
		if _, err := w.s.Execute(context.Background(), w.request("marker_settle", "c", a)); err == nil {
			t.Fatal("accepted", a)
		}
	}
	// Uncertain stays in flight, retried later; done records what landed.
	if r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "uncertain"}); r["state"] != "in_flight" {
		t.Fatal(r)
	}
	w.markersDue()
	if g := w.markerGo("c", id, "claim", "state"); g["action"] != "move" {
		t.Fatal("an uncertain move is not retried:", g)
	}
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfReview})
	var landed string
	if e := w.s.db.QueryRow(`SELECT state_landed FROM assignment_markers WHERE assignment_id=? AND phase='claim'`, id).Scan(&landed); e != nil || landed != sfReview {
		t.Fatal(landed, e)
	}
}

// A row with a live state part costs more of the watch budget.
func TestMarkerDueChargesTheStatePart(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	w.markersDue()
	before := w.call("comment_watch_lease", "c", w.fenced(map[string]any{"action": "renew"})).(map[string]any)["budget_used"].(int)
	due := w.call("marker_due", "c", w.fenced(map[string]any{})).(map[string]any)["markers"].([]any)
	after := w.call("comment_watch_lease", "c", w.fenced(map[string]any{"action": "renew"})).(map[string]any)["budget_used"].(int)
	if len(due) != 2 || after-before != MarkerRowCalls(0, 0)*2+MarkerStateCalls {
		t.Fatal(len(due), before, after)
	}
}

// A state the applier could not resolve to its ID is its own skip reason.
func TestStateSettleRecordsAnUnresolvedState(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	w.markersDue()
	w.markerGo("c", id, "claim", "state")
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "skipped", "skip_reason": "state_unresolved"})
	if s, reason, _ := w.stateRow(id, "claim"); s != "skipped" || reason != "state_unresolved" {
		t.Fatal(s, reason)
	}
}

// A marker row is charged by the binding it must prove: one team and two
// projects add three to each row.
// A pass proves the binding once: the first row of a marker_due answer
// pays the proof, a later row only its own calls. limit caps the rows, and
// the answer says how many due rows it left, and why.
func TestMarkerDueChargesTheBindingProofOncePerPass(t *testing.T) {
	w := newWatchHarness(t)
	w.call("taskboard_set_v2", "a", taskboardArgsV2(0, projectOne, projectTwo))
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	w.markersDue()
	budget := func() int {
		return w.call("comment_watch_lease", "c", w.fenced(map[string]any{"action": "renew"})).(map[string]any)["budget_used"].(int)
	}
	before := budget()
	one := w.call("marker_due", "c", w.fenced(map[string]any{"limit": 1})).(map[string]any)
	if len(one["markers"].([]any)) != 1 || one["left"] != 1 || one["stopped"] != "limit" {
		t.Fatal(one)
	}
	first := one["markers"].([]any)[0].(map[string]any)["charged"].(int)
	if first < MarkerRowCalls(1, 2) || budget()-before != first {
		t.Fatal("the first row did not pay the proof:", first, MarkerRowCalls(1, 2))
	}
	before = budget()
	due := w.call("marker_due", "c", w.fenced(map[string]any{})).(map[string]any)
	rows := due["markers"].([]any)
	if len(rows) != 2 || due["left"] != 0 || due["stopped"] != "" {
		t.Fatal(due)
	}
	if budget()-before != MarkerRowCalls(1, 2)+markerDueCalls+MarkerStateCalls || MarkerRowCalls(1, 2) != markerDueCalls+3 {
		t.Fatal("the pass paid the proof more than once:", budget()-before)
	}
	if _, err := w.s.Execute(context.Background(), w.request("marker_due", "c", w.fenced(map[string]any{"limit": 21}))); err == nil {
		t.Fatal("a limit above 20 was taken")
	}
}

// The server watcher reconciles each applied row with what it spent: the
// unused charge comes back, an overspend is charged, a charge above the
// row's most is capped, and only the system session may report.
func TestMarkerSpentReconcilesTheRowsCharge(t *testing.T) {
	h := newHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0, projectOne, projectTwo))
	w := &watchHarness{harness: h}
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	var pid string
	if e := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	sys := func(op string, a map[string]any) (map[string]any, error) {
		r, e := h.s.ExecuteSystem(context.Background(), pid, Request{Operation: op, Args: a})
		m, _ := r.Data.(map[string]any)
		return m, e
	}
	l, _ := sys("comment_watch_lease", map[string]any{"action": "acquire"})
	fence := func(a map[string]any) map[string]any {
		a["lease_token"], a["generation"] = l["lease_token"], l["generation"]
		return a
	}
	w.markersDue()
	due, err := sys("marker_due", fence(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	row := due["markers"].([]any)[0].(map[string]any)
	charged := row["charged"].(int)
	if charged != MarkerRowCalls(1, 2) {
		t.Fatal("charge", charged)
	}
	used := func() int {
		r, _ := sys("watch_charge", fence(map[string]any{"calls": 0}))
		return r["budget_used"].(int)
	}
	before := used()
	spend := func(calls, charged int) error {
		_, e := sys("marker_spent", fence(map[string]any{"assignment_id": id, "phase": "claim", "calls": calls, "charged": charged}))
		return e
	}
	if err = spend(4, charged); err != nil || used() != before-(charged-4) {
		t.Fatal("refund", err, before, used())
	}
	before = used()
	if err = spend(charged+5, charged); err != nil || used() != before+5 {
		t.Fatal("overspend", err, before, used())
	}
	// A claimed charge above the row's most refunds no more than the most.
	if _, err = sys("watch_charge", fence(map[string]any{"calls": 100})); err != nil {
		t.Fatal(err)
	}
	before = used()
	if err = spend(0, 999); err != nil || used() != before-(MarkerRowCalls(1, 2)+MarkerStateCalls) {
		t.Fatal("cap", err, before, used())
	}
	if _, err = sys("marker_spent", fence(map[string]any{"assignment_id": "asg_nope", "phase": "claim", "calls": 1, "charged": 1})); err == nil {
		t.Fatal("an unknown row was reconciled")
	}
	// A member's session cannot report: it would refund the app's budget.
	if _, err = h.s.Execute(context.Background(), h.request("marker_spent", "c", fence(map[string]any{"assignment_id": id, "phase": "claim", "calls": 0, "charged": charged}))); err == nil {
		t.Fatal("a member reconciled a marker")
	}
}

// A member's client watcher holds its own lease; it still cannot report a
// row's spend, so it can never refund the budget.
func TestMarkerSpentIsRefusedToALeaseHoldingMember(t *testing.T) {
	w := newWatchHarness(t)
	id := w.assignmentID("a")
	if _, err := w.s.Execute(context.Background(), w.request("marker_spent", "c", w.fenced(map[string]any{"assignment_id": id, "phase": "claim", "calls": 0, "charged": 8}))); err == nil {
		t.Fatal("a member's watcher reconciled a marker")
	}
}
