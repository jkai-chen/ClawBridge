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
	w.setStatusPolicy(0, sfPolicy(true))
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
	if len(due) != 2 || after-before != markerDueCalls*2+markerStateCalls {
		t.Fatal(len(due), before, after)
	}
}
