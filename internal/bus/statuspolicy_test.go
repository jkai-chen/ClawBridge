package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	sfTeam      = "11111111-1111-4111-8111-111111111111"
	sfBacklog   = "22222222-2222-4222-8222-2222222222b1"
	sfTodo      = "22222222-2222-4222-8222-2222222222b2"
	sfProgress  = "22222222-2222-4222-8222-2222222222b3"
	sfReview    = "22222222-2222-4222-8222-2222222222b4"
	sfDone      = "22222222-2222-4222-8222-2222222222b5"
	sfIssueNext = "00000000-0000-4000-8000-0000000000f2"
	// A second team and issues for the skip notices.
	sfOtherTeam     = "11111111-1111-4111-8111-111111111112"
	sfOtherProgress = "22222222-2222-4222-8222-2222222222c3"
	sfIssueOther    = "00000000-0000-4000-8000-0000000000f3"
	sfIssueThird    = "00000000-0000-4000-8000-0000000000f4"
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
	// The owner's terminal, as the CLI sends it: closes is never a
	// session's to change (R89-H1).
	return w.call("status_policy_set", "", map[string]any{"expected_revision": rev, "status_policy": policy, "via": "cli-tty"}).(map[string]any)
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

// A release after a claim skipped because the team's flow was off carries
// the claim's skip reason, so the applier may move from the claim's
// from-set; a claim that never skipped carries none.
func TestCloseMoveCarriesTheClaimsSkipReason(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	for _, reason := range []string{"team_off", "team_not_in_policy", "policy_off", "from_state"} {
		w.assign("b", sfIssueNext)
		id := w.assignmentID("b")
		w.markersDue()
		w.markerGo("c", id, "claim", "state")
		w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "skipped", "skip_reason": reason})
		w.call("assignment_release", "b", map[string]any{"revision": w.live("b")["revision"], "outcome": "done"})
		w.markersDue()
		if g := w.markerGo("c", id, "close", "state"); g["action"] != "move" || g["claim_state"] != "skipped" || g["claim_skip_reason"] != reason {
			t.Fatal(reason, g)
		}
	}
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	w.markersDue()
	w.markerGo("c", id, "claim", "state")
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress})
	w.call("assignment_release", "b", map[string]any{"revision": w.live("b")["revision"], "outcome": "done"})
	w.markersDue()
	if g := w.markerGo("c", id, "close", "state"); g["action"] != "move" {
		t.Fatal(g)
	} else if _, ok := g["claim_skip_reason"]; ok {
		t.Fatal("a landed claim carries a skip reason", g)
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

// A bound team the policy lacks is its own skip reason.
func TestStateSettleRecordsATeamNotInThePolicy(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	w.markersDue()
	w.markerGo("c", id, "claim", "state")
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "skipped", "skip_reason": "team_not_in_policy"})
	if s, reason, _ := w.stateRow(id, "claim"); s != "skipped" || reason != "team_not_in_policy" {
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
	// A pass (it sends limit) pays the proof once. marker_due hands rows
	// out without changing them, so the same two rows price both ways.
	before = budget()
	due := w.call("marker_due", "c", w.fenced(map[string]any{"limit": 20})).(map[string]any)
	if rows := due["markers"].([]any); len(rows) != 2 || due["left"] != 0 || due["stopped"] != "" {
		t.Fatal(due)
	}
	if budget()-before != MarkerRowCalls(1, 2)+markerDueCalls+MarkerStateCalls || MarkerRowCalls(1, 2) != markerDueCalls+3 {
		t.Fatal("the pass paid the proof more than once:", budget()-before)
	}
	// R116-M2: a v0.3.4 `linear watch` sends no limit and proves every row,
	// so every row pays the proof.
	before = budget()
	due = w.call("marker_due", "c", w.fenced(map[string]any{})).(map[string]any)
	rows := due["markers"].([]any)
	if len(rows) != 2 || budget()-before != 2*MarkerRowCalls(1, 2)+MarkerStateCalls {
		t.Fatal("a caller without limit paid the proof once:", budget()-before, due)
	}
	for _, r := range rows {
		if c := r.(map[string]any)["charged"].(int); c < MarkerRowCalls(1, 2) {
			t.Fatal("a row without limit was charged without the proof:", c)
		}
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

// skipMarker settles a session's claim move as skipped with reason.
func (w *watchHarness) skipMarker(session, reason string) string {
	w.t.Helper()
	id := w.assignmentID(session)
	w.markersDue()
	if g := w.markerGo("c", id, "claim", "state"); g["action"] != "move" {
		w.t.Fatal(g)
	}
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "skipped", "skip_reason": reason})
	return id
}

// markerNotices are the skip notices a session received, oldest first.
func (w *watchHarness) markerNotices(session string) []string {
	w.t.Helper()
	rows, err := w.s.db.Query(`SELECT m.body FROM messages m JOIN deliveries d ON d.message_id=m.id WHERE m.kind='linear-write' AND d.recipient_id=? AND (m.body LIKE 'Your % marker on %' OR m.body LIKE 'A % marker on %') ORDER BY m.created_at,m.id`, w.sessionID(session))
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, body)
	}
	return out
}

// The summary names the target from the stored policy, else the policy's
// state, and says why for every skip reason; a row that wasn't skipped
// says how its parts stand.
func TestMarkerSummaryForEveryReason(t *testing.T) {
	policy, err := ParseStatusPolicy(sfPolicy(true))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"from_state":         "someone else had moved the issue, so it was left as it is; set it yourself if right",
		"team_unbound":       "the issue's team isn't bound to this project",
		"team_off":           "the status policy doesn't move this team's issues",
		"team_not_in_policy": "the status policy doesn't move this team's issues",
		"target_unknown":     "the target state couldn't be found in Linear; check the status policy",
		"state_unresolved":   "the issue's current state couldn't be read, so it was left as it is; check it in Linear",
		"policy_off":         "the status policy is off",
		"policy_changed":     "the status policy changed after the move was planned",
	}
	if len(want) != len(statusSkipReasons) {
		t.Fatal("a skip reason has no summary test")
	}
	for reason, why := range want {
		claim, close := "In Progress", "In Review"
		if reason == "policy_changed" {
			// The policy now may not target what was planned: no name.
			claim, close = "the policy's state", "the policy's state"
		}
		row := map[string]any{"phase": "claim", "state_state": "skipped", "state_skip_reason": reason}
		if got := markerSummary(row, policy); got != "not moved to "+claim+": "+why {
			t.Errorf("%s: %q", reason, got)
		}
		row["phase"] = "close"
		if got := markerSummary(row, policy); got != "not moved to "+close+": "+why {
			t.Errorf("%s close: %q", reason, got)
		}
		if got := markerSummary(row, StatusPolicy{}); got != "not moved to the policy's state: "+why {
			t.Errorf("%s without a policy: %q", reason, got)
		}
	}
	// A close in a project agents close goes on to done; two teams whose
	// targets differ name none.
	p := sfPolicy(true)
	team := p["teams"].(map[string]any)[sfTeam].(map[string]any)
	team["done"] = map[string]any{"id": sfDone, "name": "Done", "type": "completed"}
	p["closes"] = map[string]any{cpOne: "agent"}
	other := map[string]any{"claim": map[string]any{"id": sfOtherProgress, "name": "Doing", "type": "started"}, "workflow": map[string]any{sfOtherProgress: "started"}}
	agent, err := ParseStatusPolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	close := map[string]any{"phase": "close", "state_state": "skipped", "state_skip_reason": "from_state", "issue_project_id": cpOne}
	if got := markerSummary(close, agent); !strings.HasPrefix(got, "not moved to Done: ") {
		t.Fatal(got)
	}
	p["teams"].(map[string]any)[sfOtherTeam] = other
	two, err := ParseStatusPolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := markerSummary(map[string]any{"phase": "claim", "state_state": "skipped", "state_skip_reason": "from_state"}, two); !strings.HasPrefix(got, "not moved to the policy's state: ") {
		t.Fatal(got)
	}
	// A changed policy names a recorded target only.
	changed := map[string]any{"phase": "claim", "state_state": "skipped", "state_skip_reason": "policy_changed"}
	if got := markerSummary(changed, policy); got != "not moved to the policy's state: the status policy changed after the move was planned" {
		t.Fatal(got)
	}
	changed["state_target"] = sfProgress
	if got := markerSummary(changed, policy); !strings.HasPrefix(got, "not moved to In Progress: ") {
		t.Fatal(got)
	}
	// A recorded target is named by its ID.
	if got := markerSummary(map[string]any{"phase": "claim", "state_state": "skipped", "state_skip_reason": "from_state", "state_target": sfOtherProgress}, two); !strings.HasPrefix(got, "not moved to Doing: ") {
		t.Fatal(got)
	}
	for _, c := range []struct {
		row  map[string]any
		want string
	}{
		{map[string]any{"comment_state": "done", "label_state": "done", "state_state": "done", "state_target": sfProgress, "state_landed": sfProgress}, "comment done, label done, state done (moved)"},
		{map[string]any{"comment_state": "done", "label_state": "no_label", "state_state": "done", "state_target": sfProgress, "state_landed": sfReview}, "comment done, label no_label, state done (another state stood)"},
		{map[string]any{"comment_state": "pending", "label_state": "pending", "state_state": "off"}, "comment pending, label pending, state off"},
	} {
		if got := markerSummary(c.row, policy); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
}

// A skip a person may act on is told once, in the settle that records it:
// to the assignee, by identifier, with the read to run.
func TestStateSkipNoticeOnTheTransitionOnly(t *testing.T) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.skipMarker("b", "from_state")
	want := "Your claim marker on FIX-1 didn't move the issue: not moved to In Progress: someone else had moved the issue, so it was left as it is; set it yourself if right. Details: clawbridge assignment markers --issue FIX-1"
	if got := w.markerNotices("b"); len(got) != 1 || got[0] != want {
		t.Fatalf("%q", got)
	}
	// A replayed settle is refused and sends nothing.
	if _, err := w.s.Execute(context.Background(), w.request("marker_settle", "c", map[string]any{"assignment_id": id, "phase": "claim", "part": "state", "outcome": "skipped", "skip_reason": "from_state"})); err == nil {
		t.Fatal("a replayed settle was accepted")
	}
	if got := w.markerNotices("b"); len(got) != 1 {
		t.Fatalf("replayed: %q", got)
	}
	if got := w.markerNotices("c"); len(got) != 0 {
		t.Fatalf("the coordinator heard of an assignee's skip: %q", got)
	}
	// A configured skip sends nothing.
	w.release("b")
	w.assign("b", sfIssueNext)
	w.skipMarker("b", "team_off")
	if got := w.markerNotices("b"); len(got) != 1 {
		t.Fatalf("a configured skip was told: %q", got)
	}
	// The server's own policy_changed skip is told too, for the release.
	w.markersDue()
	w.markerGo("c", w.assignmentID("b"), "claim", "state")
	w.call("assignment_release", "b", map[string]any{"revision": w.live("b")["revision"], "outcome": "done"})
	w.setStatusPolicy(1, sfPolicy(true))
	w.markersDue()
	if g := w.markerGo("c", w.assignmentID2("b"), "close", "state"); g["reason"] != "policy_changed" {
		t.Fatal(g)
	}
	got := w.markerNotices("b")
	if len(got) != 2 || got[1] != "Your release marker on FIX-1 didn't move the issue: not moved to the policy's state: the status policy changed after the move was planned. Details: clawbridge assignment markers --issue FIX-1" {
		t.Fatalf("%q", got)
	}
	if n := len(w.historyRows("assignment_marker")); n != 2 {
		t.Fatal("audited notices:", n)
	}
}

// A coordinator notice routes by the issue's Linear project's domain. An
// unreadable dispatcher document tells nobody, yet the move still settles
// as skipped, and the audit says notice_error.
func TestStateSkipNoticeRoutesByDomainAndSurvivesAnUnreadableDispatcher(t *testing.T) {
	w := newWatchHarness(t)
	move := skew(t)
	w.call("taskboard_set_v2", "a", taskboardArgsV2(0, projectOne, projectTwo))
	w.call("label_policy_set", "a", map[string]any{"expected_revision": 0, "label_policy": map[string]any{"labels": []any{"payments", "platform"}}})
	w.setStatusPolicy(0, sfPolicy(true))
	var pid string
	if err := w.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	w.dispatcherSessions(pid)
	platform := paymentsDomain(w.principal())
	platform["id"], platform["name"] = "platform", "Platform"
	platform["match"] = map[string]any{"linear_projects": []any{projectTwo}, "labels": []any{"platform"}}
	platform["coordinator_session"] = map[string]any{"name": "b"}
	w.call("dispatcher_domains_set", "a", map[string]any{"expected_revision": 0, "dispatcher_domains": map[string]any{"domains": []any{paymentsDomain(w.principal()), platform}}})
	sql := func(q string, args ...any) {
		t.Helper()
		if _, err := w.s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	var domains string
	if err := w.s.db.QueryRow(`SELECT domains FROM dispatcher_policies WHERE project_id=?`, pid).Scan(&domains); err != nil {
		t.Fatal(err)
	}
	skipGone := func(session, issue string) {
		t.Helper()
		sql(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='c' AND project_id=?) WHERE id=?`, pid, pid)
		w.call("register_session", session, map[string]any{"instance": session + "-instance", "role": "worker"})
		w.propose(session, map[string]any{"issue_uuid": issue, "issue_identifier": "FIX-9", "issue_project_id": projectTwo})
		w.call("assignment_confirm", "c", map[string]any{"target_session": session, "revision": w.live(session)["revision"]})
		id := w.assignmentID(session)
		sql(`UPDATE sessions SET status='retired' WHERE id=?`, w.sessionID(session))
		sql(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='clawbridge-dispatcher' AND project_id=?) WHERE id=?`, pid, pid)
		w.markersDue()
		w.markerGo("c", id, "claim", "state")
		w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "skipped", "skip_reason": "from_state"})
		var state string
		if err := w.s.db.QueryRow(`SELECT state_state FROM assignment_markers WHERE assignment_id=? AND phase='claim'`, id).Scan(&state); err != nil || state != "skipped" {
			t.Fatal(state, err)
		}
	}
	sent := func() int {
		t.Helper()
		var n int
		if err := w.s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE kind='linear-write' AND body LIKE '% marker on FIX-9 %'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sql(`UPDATE dispatcher_policies SET domains='not json' WHERE project_id=?`, pid)
	skipGone("d", sfIssueNext)
	audits := w.historyRows("assignment_marker")
	if sent() != 0 || len(audits) != 1 || !strings.Contains(str(audits[0], "details"), `"notice_error":true`) || !strings.Contains(str(audits[0], "details"), `"coordinator":false`) {
		t.Fatal(sent(), audits)
	}
	sql(`UPDATE dispatcher_policies SET domains=? WHERE project_id=?`, domains, pid)
	move(time.Minute)
	skipGone("e", sfIssueOther)
	if b, c := w.markerNotices("b"), w.markerNotices("c"); len(b) != 1 || len(c) != 0 || !strings.HasPrefix(b[0], "A claim marker on FIX-9 didn't move the issue") {
		t.Fatalf("platform's coordinator %q, payments' %q", b, c)
	}
	errs := 0
	for _, a := range w.historyRows("assignment_marker") {
		if strings.Contains(str(a, "details"), "notice_error") {
			errs++
		}
	}
	if audits = w.historyRows("assignment_marker"); len(audits) != 2 || errs != 1 {
		t.Fatal(audits)
	}
}

// assignmentID2 is a session's newest assignment, released or not.
func (w *watchHarness) assignmentID2(session string) string {
	w.t.Helper()
	var id string
	if e := w.s.db.QueryRow(`SELECT id FROM session_assignments WHERE session_id=? ORDER BY proposed_at DESC,id DESC LIMIT 1`, w.sessionID(session)).Scan(&id); e != nil {
		w.t.Fatal(e)
	}
	return id
}

// Without an active assignee the coordinator is told, at most once in ten
// minutes per project; the UUID names an issue with no identifier.
func TestStateSkipNoticeFallsBackToTheCoordinator(t *testing.T) {
	w := newWatchHarness(t)
	move := skew(t)
	w.setStatusPolicy(0, sfPolicy(true))
	skipGone := func(session, issue, reason string) {
		t.Helper()
		w.call("register_session", session, map[string]any{"instance": session + "-instance", "role": "worker"})
		w.propose(session, map[string]any{"issue_uuid": issue})
		w.call("assignment_confirm", "c", map[string]any{"target_session": session, "revision": w.live(session)["revision"]})
		id := w.assignmentID(session)
		if _, err := w.s.db.Exec(`UPDATE sessions SET status='retired' WHERE id=?`, w.sessionID(session)); err != nil {
			t.Fatal(err)
		}
		w.markersDue()
		w.markerGo("c", id, "claim", "state")
		w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "skipped", "skip_reason": reason})
	}
	skipGone("d", sfIssueNext, "target_unknown")
	want := "A claim marker on " + sfIssueNext + " didn't move the issue: not moved to In Progress: the target state couldn't be found in Linear; check the status policy. Details: clawbridge assignment markers --issue " + sfIssueNext
	if got := w.markerNotices("c"); len(got) != 1 || got[0] != want {
		t.Fatalf("%q", got)
	}
	// A second within ten minutes is recorded, not sent; past them it is.
	move(5 * time.Minute)
	skipGone("e", sfIssueOther, "state_unresolved")
	if got := w.markerNotices("c"); len(got) != 1 {
		t.Fatalf("the rate limit didn't hold: %q", got)
	}
	move(6 * time.Minute)
	skipGone("f", sfIssueThird, "from_state")
	if got := w.markerNotices("c"); len(got) != 2 || !strings.Contains(got[1], sfIssueThird) {
		t.Fatalf("%q", got)
	}
	audits := w.historyRows("assignment_marker")
	if len(audits) != 3 {
		t.Fatal("every skip is audited:", len(audits))
	}
}
