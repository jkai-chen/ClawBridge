package bus

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const sfCanceled = "22222222-2222-4222-8222-2222222222b6"

type nudgeHarness struct{ *healthHarness }

// newNudgeHarness is a proven server whose status policy moves claims and
// done releases, with project flow on; projectOne and projectTwo are bound.
// Neither Linear project's daily flow run is due.
func newNudgeHarness(t *testing.T) *nudgeHarness {
	t.Helper()
	n := &nudgeHarness{newHealthHarness(t)}
	n.s.SetProjectFlowProven(true)
	policy := sfPolicy(true)
	team := policy["teams"].(map[string]any)[sfTeam].(map[string]any)
	team["workflow"].(map[string]any)[sfCanceled] = "canceled"
	policy["project_flow"] = true
	n.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": policy})
	for _, lp := range []string{projectOne, projectTwo} {
		if _, e := n.s.db.Exec(`INSERT INTO project_flow_state(project_id,linear_project_id,next_flow_at) VALUES(?,?,'2999-01-01T00:00:00.000000000Z')`, n.pid, lp); e != nil {
			t.Fatal(e)
		}
	}
	return n
}

// claim confirms session b on an issue of lp and settles its claim move as
// landed; it returns the assignment.
func (n *nudgeHarness) claim(issue, lp, landed string) string {
	n.t.Helper()
	n.propose("b", map[string]any{"issue_uuid": issue, "issue_identifier": "FIX-2", "issue_project_id": lp})
	n.call("assignment_confirm", "c", map[string]any{"target_session": "b", "revision": n.live("b")["revision"]})
	var id string
	if e := n.s.db.QueryRow(`SELECT id FROM session_assignments WHERE session_id=? AND state='confirmed'`, n.sessionID("b")).Scan(&id); e != nil {
		n.t.Fatal(e)
	}
	n.settle(id, "claim", landed)
	return id
}

// release releases session b's assignment id as done and settles its close
// move as landed.
func (n *nudgeHarness) release(id, landed string) {
	n.t.Helper()
	n.call("assignment_release", "b", map[string]any{"revision": n.live("b")["revision"], "outcome": "done"})
	n.settle(id, "close", landed)
}

func (n *nudgeHarness) settle(id, phase, landed string) {
	n.t.Helper()
	if _, e := n.s.db.Exec(`UPDATE assignment_markers SET next_attempt_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		n.t.Fatal(e)
	}
	if g := n.call("marker_go", "c", map[string]any{"assignment_id": id, "phase": phase, "part": "state"}).(map[string]any); g["action"] != "move" {
		n.t.Fatal(phase, g)
	}
	n.call("marker_settle", "c", map[string]any{"assignment_id": id, "phase": phase, "part": "state", "outcome": "done", "state_target": landed, "state_landed": landed})
}

// asks is each nudge row as "linear_project kind state".
func (n *nudgeHarness) asks() []string {
	n.t.Helper()
	rows, e := n.s.db.Query(`SELECT linear_project_id,kind,state FROM project_flow_nudges ORDER BY asked_at,id`)
	if e != nil {
		n.t.Fatal(e)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var lp, kind, state string
		_ = rows.Scan(&lp, &kind, &state)
		out = append(out, lp+" "+kind+" "+state)
	}
	return out
}

func (n *nudgeHarness) budgetUsed() int {
	n.t.Helper()
	return n.sys("watch_charge", map[string]any{"calls": 0})["budget_used"].(int)
}

func (n *nudgeHarness) nudged() []any {
	n.t.Helper()
	return n.sys("health_due", map[string]any{"limit": 2, "nudges_only": true})["runs"].([]any)
}

// A claim that lands in a started state asks an R1 check of its project.
// The nudge's pass gets it as a flow-only run charged one call; its report
// takes the issue just started as the evidence and moves the project.
func TestFlowNudgeR1ReadsTheProjectOnly(t *testing.T) {
	n := newNudgeHarness(t)
	n.claim(sfIssueNext, projectOne, sfProgress)
	if got := n.asks(); len(got) != 1 || got[0] != projectOne+" r1 asked" {
		t.Fatal(got)
	}
	before := n.budgetUsed()
	runs := n.nudged()
	if len(runs) != 1 || fmt.Sprint(runs[0]) != fmt.Sprint(map[string]any{"linear_project": projectOne, "health": false, "flow": true, "nudge": "r1"}) {
		t.Fatal(runs)
	}
	if spent := n.budgetUsed() - before; spent != 1 {
		t.Fatal("an R1 nudge charged", spent)
	}
	out := n.sys("health_report", map[string]any{"linear_project": projectOne, "calls": 1, "nudge": "r1", "flow": flowFacts(psBacklog, issues{partial: true})})
	if m := moveOf(out); m == nil || m["to_type"] != "started" {
		t.Fatal(out)
	}
	if got := n.asks(); got[0] != projectOne+" r1 done" {
		t.Fatal(got)
	}
	// A report for a nudge nobody handed out is refused.
	if _, e := n.sysErr("health_report", map[string]any{"linear_project": projectOne, "calls": 1, "nudge": "r1", "flow": flowFacts(psBacklog, issues{partial: true})}); !codeIs(e, "conflict", "not handed out") {
		t.Fatal(e)
	}
}

// An R1 nudge never claims every issue was read: it can't finish a project
// or end a done episode with a notice.
func TestFlowNudgeR1NeverFinishes(t *testing.T) {
	n := newNudgeHarness(t)
	n.claim(sfIssueNext, projectOne, sfProgress)
	n.nudged()
	out := n.sys("health_report", map[string]any{"linear_project": projectOne, "calls": 1, "nudge": "r1", "flow": flowFacts(psStarted2, issues{total: 3, completed: 3})})
	if moveOf(out) != nil || out["flow"] != "none" {
		t.Fatal(out)
	}
}

// Only a done release that lands in a completed state asks an R2 check,
// which reads every issue. A release that lands in a started or canceled
// state, or a claim that lands anywhere but started, asks nothing.
func TestFlowNudgeR2OnlyAfterACompletedRelease(t *testing.T) {
	for name, landed := range map[string]string{"started": sfReview, "canceled": sfCanceled} {
		n := newNudgeHarness(t)
		id := n.claim(sfIssueNext, projectOne, sfProgress)
		n.nudged()
		n.release(id, landed)
		if got := n.asks(); len(got) != 1 {
			t.Fatal(name, "release asked:", got)
		}
	}
	n := newNudgeHarness(t)
	n.claim(sfIssueNext, projectOne, sfBacklog)
	if got := n.asks(); len(got) != 0 {
		t.Fatal("a claim that landed in backlog asked:", got)
	}
	n = newNudgeHarness(t)
	id := n.claim(sfIssueNext, projectOne, sfProgress)
	n.nudged()
	n.release(id, sfDone)
	if got := n.asks(); len(got) != 2 || got[1] != projectOne+" r2 asked" {
		t.Fatal(got)
	}
	if _, e := n.s.db.Exec(`UPDATE project_flow_nudges SET granted_at='2000-01-01T00:00:00.000000000Z' WHERE granted_at IS NOT NULL`); e != nil {
		t.Fatal(e)
	}
	before := n.budgetUsed()
	if runs := n.nudged(); len(runs) != 1 || runs[0].(map[string]any)["nudge"] != "r2" {
		t.Fatal(runs)
	}
	if spent := n.budgetUsed() - before; spent != FlowReadCalls {
		t.Fatal("an R2 nudge charged", spent)
	}
}

// One ask waits per Linear project, and an R2 widens a waiting R1. A
// Linear project nudged in the last ten minutes waits; another doesn't.
func TestFlowNudgePerLinearProject(t *testing.T) {
	n := newNudgeHarness(t)
	id := n.claim(sfIssueNext, projectOne, sfProgress)
	n.release(id, sfDone)
	if got := n.asks(); len(got) != 1 || got[0] != projectOne+" r2 asked" {
		t.Fatal(got)
	}
	n.nudged()
	n.claim(watchIssue, projectOne, sfProgress)
	if runs := n.nudged(); len(runs) != 0 {
		t.Fatal("a second nudge within ten minutes:", runs)
	}
	if _, e := n.s.db.Exec(`UPDATE project_flow_nudges SET granted_at=? WHERE state='granted'`, layout(time.Now().Add(-11*time.Minute))); e != nil {
		t.Fatal(e)
	}
	if runs := n.nudged(); len(runs) != 1 {
		t.Fatal("the waiting ask never ran:", runs)
	}
}

// SF4-M2 c: past 20 nudged reads in an hour across the workspace, an ask
// is dropped without a read, and the daily run catches up.
func TestFlowNudgeWorkspaceCap(t *testing.T) {
	n := newNudgeHarness(t)
	recent := layout(time.Now().Add(-30 * time.Minute))
	for i := 0; i < flowNudgesPerHour; i++ {
		if _, e := n.s.db.Exec(`INSERT INTO project_flow_nudges(id,project_id,linear_project_id,kind,asked_at,state,granted_at) VALUES(?,?,?,'r1',?,'done',?)`, newID("fn_"), n.pid, fmt.Sprintf("bbbb%04d-0000-4000-8000-000000000000", i), recent, recent); e != nil {
			t.Fatal(e)
		}
	}
	n.claim(sfIssueNext, projectOne, sfProgress)
	if runs := n.nudged(); len(runs) != 0 {
		t.Fatal("the 21st nudge in an hour read:", runs)
	}
	if got := n.asks(); got[len(got)-1] != projectOne+" r1 dropped" {
		t.Fatal(got)
	}
	// An hour on, the cap has room again.
	if _, e := n.s.db.Exec(`UPDATE project_flow_nudges SET granted_at=? WHERE state='done'`, layout(time.Now().Add(-61*time.Minute))); e != nil {
		t.Fatal(e)
	}
	n.claim(watchIssue, projectTwo, sfProgress)
	if runs := n.nudged(); len(runs) != 1 {
		t.Fatal(runs)
	}
}

// An ask is dropped when a daily run of its Linear project is in hand, or
// after waiting an hour. A nudge's pass hands out no daily run, and leaves
// the ask of a project whose daily run is due waiting for the tick
// (R101-M1). With flow off, nothing asks.
func TestFlowNudgeDroppedAndOff(t *testing.T) {
	n := newNudgeHarness(t)
	n.claim(sfIssueNext, projectOne, sfProgress)
	if _, e := n.s.db.Exec(`UPDATE project_flow_state SET next_flow_at=NULL WHERE linear_project_id=?`, projectOne); e != nil {
		t.Fatal(e)
	}
	if runs := n.nudged(); len(runs) != 0 || n.asks()[0] != projectOne+" r1 asked" {
		t.Fatal("a nudge's pass ran with the daily run due:", runs, n.asks())
	}
	runs := n.sys("health_due", map[string]any{"limit": 2})["runs"].([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["nudge"] != nil {
		t.Fatal(runs)
	}
	if got := n.asks(); got[0] != projectOne+" r1 dropped" {
		t.Fatal("the daily run didn't cover the ask:", got)
	}
	n = newNudgeHarness(t)
	n.claim(sfIssueNext, projectOne, sfProgress)
	if _, e := n.s.db.Exec(`UPDATE project_flow_nudges SET asked_at=?`, layout(time.Now().Add(-61*time.Minute))); e != nil {
		t.Fatal(e)
	}
	if runs := n.nudged(); len(runs) != 0 || n.asks()[0] != projectOne+" r1 dropped" {
		t.Fatal(runs, n.asks())
	}
	n = newNudgeHarness(t)
	n.s.SetProjectFlowProven(false)
	n.claim(sfIssueNext, projectOne, sfProgress)
	if got := n.asks(); len(got) != 0 {
		t.Fatal("asked with flow off:", got)
	}
}

// A nudged run serves project flow alone, even where health is on: health
// is neither charged nor recorded, and a nudge's pass leaves a due health
// run, and the ask with it, to the tick.
func TestFlowNudgeSkipsHealth(t *testing.T) {
	n := newNudgeHarness(t)
	n.enable(projectOne, 0, nil)
	if _, e := n.s.db.Exec(`UPDATE project_health SET next_health_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		t.Fatal(e)
	}
	n.claim(sfIssueNext, projectOne, sfProgress)
	if runs := n.nudged(); len(runs) != 0 || n.asks()[0] != projectOne+" r1 asked" {
		t.Fatal("a nudge's pass ran health:", runs, n.asks())
	}
	if _, e := n.s.db.Exec(`UPDATE project_health SET next_health_at='2999-01-01T00:00:00.000000000Z'`); e != nil {
		t.Fatal(e)
	}
	if runs := n.nudged(); len(runs) != 1 || runs[0].(map[string]any)["nudge"] != "r1" {
		t.Fatal(runs)
	}
	n.sys("health_report", map[string]any{"linear_project": projectOne, "calls": 1, "nudge": "r1", "flow": flowFacts(psBacklog, issues{partial: true})})
	var last any
	if e := n.s.db.QueryRow(`SELECT last_run_at FROM project_health WHERE linear_project_id=?`, projectOne).Scan(&last); e != nil || last != nil {
		t.Fatal("a nudge recorded health:", last, e)
	}
}

func (n *nudgeHarness) flowSlot(lp string) (string, string) {
	n.t.Helper()
	var next, lastError string
	if e := n.s.db.QueryRow(`SELECT COALESCE(next_flow_at,''),last_error FROM project_flow_state WHERE project_id=? AND linear_project_id=?`, n.pid, lp).Scan(&next, &lastError); e != nil {
		n.t.Fatal(e)
	}
	return next, lastError
}

// R101-M1: a nudged report, read or failed, leaves the daily slot and its
// error alone, so it never stands in for the full read.
func TestFlowNudgeKeepsTheDailySlot(t *testing.T) {
	n := newNudgeHarness(t)
	const slot = "2999-01-01T00:00:00.000000000Z"
	if _, e := n.s.db.Exec(`UPDATE project_flow_state SET last_error='earlier' WHERE linear_project_id=?`, projectOne); e != nil {
		t.Fatal(e)
	}
	n.claim(sfIssueNext, projectOne, sfProgress)
	n.nudged()
	n.sys("health_report", map[string]any{"linear_project": projectOne, "calls": 1, "nudge": "r1", "flow": flowFacts(psBacklog, issues{partial: true})})
	if next, lastError := n.flowSlot(projectOne); next != slot || lastError != "earlier" {
		t.Fatal("a nudged read moved the slot:", next, lastError)
	}
	n.claim(watchIssue, projectTwo, sfProgress)
	n.nudged()
	n.sys("health_report", map[string]any{"linear_project": projectTwo, "calls": 1, "nudge": "r1", "error": "timeout"})
	if next, lastError := n.flowSlot(projectTwo); next != slot || lastError != "" {
		t.Fatal("a failed nudged read moved the slot:", next, lastError)
	}
}

// R101-M2: a claim's R1 is dropped when its issue is released out of the
// started states, whether the ask is waiting or already handed out.
func TestFlowNudgeR1DroppedWhenItsIssueIsReleased(t *testing.T) {
	n := newNudgeHarness(t)
	id := n.claim(sfIssueNext, projectOne, sfProgress)
	n.release(id, sfCanceled)
	if runs := n.nudged(); len(runs) != 0 || n.asks()[0] != projectOne+" r1 dropped" {
		t.Fatal("a canceled claim still nudged:", runs, n.asks())
	}
	n = newNudgeHarness(t)
	id = n.claim(sfIssueNext, projectOne, sfProgress)
	n.nudged()
	n.release(id, sfBacklog)
	if _, e := n.sysErr("health_report", map[string]any{"linear_project": projectOne, "calls": 1, "nudge": "r1", "flow": flowFacts(psBacklog, issues{partial: true})}); !codeIs(e, "conflict", "not handed out") {
		t.Fatal("a handed-out R1 outlived its issue's release:", e)
	}
	// Released to a started state, the issue is still the started one.
	n = newNudgeHarness(t)
	id = n.claim(sfIssueNext, projectOne, sfProgress)
	n.release(id, sfReview)
	if got := n.asks(); len(got) != 1 || got[0] != projectOne+" r1 asked" {
		t.Fatal(got)
	}
}

// R101-L1, L4: a grant whose report never came lapses after an hour, so
// no later report answers it; old rows go on a nudge pass too.
func TestFlowNudgeGrantsLapse(t *testing.T) {
	n := newNudgeHarness(t)
	n.claim(sfIssueNext, projectOne, sfProgress)
	n.nudged()
	if _, e := n.s.db.Exec(`UPDATE project_flow_nudges SET granted_at=?`, layout(time.Now().Add(-61*time.Minute))); e != nil {
		t.Fatal(e)
	}
	if _, e := n.sysErr("health_report", map[string]any{"linear_project": projectOne, "calls": 1, "nudge": "r1", "flow": flowFacts(psBacklog, issues{partial: true})}); !codeIs(e, "conflict", "not handed out") {
		t.Fatal("a lapsed grant was answered:", e)
	}
	n.nudged()
	if got := n.asks(); got[0] != projectOne+" r1 dropped" {
		t.Fatal(got)
	}
	if _, e := n.s.db.Exec(`UPDATE project_flow_nudges SET asked_at=?`, layout(time.Now().Add(-3*time.Hour))); e != nil {
		t.Fatal(e)
	}
	n.nudged()
	if got := n.asks(); len(got) != 0 {
		t.Fatal("old rows stayed:", got)
	}
}

// R101-L2: nudged runs have their own allowance, so a tick with no daily
// runs left still serves them, and they don't eat the daily runs' limit.
func TestFlowNudgeOwnAllowance(t *testing.T) {
	n := newNudgeHarness(t)
	n.claim(sfIssueNext, projectOne, sfProgress)
	if runs := n.sys("health_due", map[string]any{"limit": 0})["runs"].([]any); len(runs) != 1 || runs[0].(map[string]any)["nudge"] != "r1" {
		t.Fatal(runs)
	}
	n = newNudgeHarness(t)
	n.claim(sfIssueNext, projectOne, sfProgress)
	if _, e := n.s.db.Exec(`UPDATE project_flow_state SET next_flow_at=NULL WHERE linear_project_id=?`, projectTwo); e != nil {
		t.Fatal(e)
	}
	runs := n.sys("health_due", map[string]any{"limit": 1})["runs"].([]any)
	if len(runs) != 2 || runs[0].(map[string]any)["linear_project"] != projectTwo || runs[1].(map[string]any)["nudge"] != "r1" {
		t.Fatal(runs)
	}
}

// R101-M2: a waiting R1 rests on the newest claim of its Linear project, so
// releasing that issue out of started drops it.
func TestFlowNudgeR1RestsOnTheNewestClaim(t *testing.T) {
	n := newNudgeHarness(t)
	first := n.claim(sfIssueNext, projectOne, sfProgress)
	n.release(first, sfReview)
	second := n.claim(watchIssue, projectOne, sfProgress)
	if got := n.asks(); len(got) != 1 || got[0] != projectOne+" r1 asked" {
		t.Fatal(got)
	}
	n.release(second, sfCanceled)
	if got := n.asks(); got[0] != projectOne+" r1 dropped" {
		t.Fatal("the newest claim's release left the R1:", got)
	}
}

// A nudged run is never forced: a forced run is health's alone (#91), and
// the refusal leaves the nudge to be reported as it is.
func TestFlowNudgeIsNeverForced(t *testing.T) {
	n := newNudgeHarness(t)
	n.enable(projectOne, 0, nil)
	n.claim(sfIssueNext, projectOne, sfProgress)
	if runs := n.nudged(); len(runs) != 1 || runs[0].(map[string]any)["nudge"] != "r1" {
		t.Fatal(runs)
	}
	report := map[string]any{"linear_project": projectOne, "calls": 1, "nudge": "r1", "flow": flowFacts(psBacklog, issues{partial: true}), "forced": true}
	if _, e := n.sysErr("health_report", report); e == nil || !strings.Contains(e.Error(), "never forced") {
		t.Fatal("a forced nudge was taken:", e)
	}
	delete(report, "forced")
	if out := n.sys("health_report", report); out["flow"] == nil {
		t.Fatal("the nudge was lost to the refusal:", out)
	}
}
