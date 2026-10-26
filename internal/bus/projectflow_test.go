package bus

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Project statuses as a workspace lists them: two started, the lower
// position first, and an archived completed one ahead of the live one.
const (
	psBacklog   = "aaaa0001-0000-4000-8000-000000000001"
	psPlanned   = "aaaa0002-0000-4000-8000-000000000002"
	psStarted   = "aaaa0003-0000-4000-8000-000000000003"
	psStarted2  = "aaaa0004-0000-4000-8000-000000000004"
	psDoneOld   = "aaaa0005-0000-4000-8000-000000000005"
	psCompleted = "aaaa0006-0000-4000-8000-000000000006"
	psPaused    = "aaaa0007-0000-4000-8000-000000000007"
	psCanceled  = "aaaa0008-0000-4000-8000-000000000008"
)

var psTypes = map[string]string{psBacklog: "backlog", psPlanned: "planned", psStarted: "started", psStarted2: "started", psDoneOld: "completed", psCompleted: "completed", psPaused: "paused", psCanceled: "canceled"}

func psCatalog() []any {
	return []any{
		map[string]any{"id": psBacklog, "name": "Backlog", "type": "backlog", "position": 0, "archived": false},
		map[string]any{"id": psPlanned, "name": "Planned", "type": "planned", "position": 1, "archived": false},
		map[string]any{"id": psStarted, "name": "In Progress", "type": "started", "position": 3, "archived": false},
		map[string]any{"id": psStarted2, "name": "Kickoff", "type": "started", "position": 2, "archived": false},
		map[string]any{"id": psDoneOld, "name": "Shipped (old)", "type": "completed", "position": 0, "archived": true},
		map[string]any{"id": psCompleted, "name": "Completed", "type": "completed", "position": 5, "archived": false},
		map[string]any{"id": psPaused, "name": "Paused", "type": "paused", "position": 6, "archived": false},
		map[string]any{"id": psCanceled, "name": "Canceled", "type": "canceled", "position": 7, "archived": false},
	}
}

// issues is a project's issue counts: total, completed, canceled, started,
// completed since the project's creation, and whether every page was read.
type issues struct {
	total, completed, canceled, started, since int
	partial                                    bool
}

func flowFacts(status string, n issues) map[string]any {
	return map[string]any{
		"status":     map[string]any{"id": status, "name": "S", "type": psTypes[status], "position": 0, "archived": false},
		"created_at": "2026-01-01T00:00:00Z", "statuses": psCatalog(),
		"total": n.total, "completed": n.completed, "canceled": n.canceled, "started": n.started,
		"completed_since_created": n.since, "complete": !n.partial,
	}
}

type flowHarness struct{ *healthHarness }

// newFlowHarness is a proven server with an enabled status policy whose
// project flow is on, projectOne closed by closes.
func newFlowHarness(t *testing.T, closes string) *flowHarness {
	t.Helper()
	h := &flowHarness{newHealthHarness(t)}
	h.s.SetProjectFlowProven(true)
	policy := map[string]any{"enabled": true, "teams": map[string]any{}, "project_flow": true}
	if closes == "agent" {
		policy["closes"] = map[string]any{projectOne: "agent"}
	}
	// A policy letting agents close is one a person typed (R89-M1).
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": policy, "via": "cli-tty"})
	return h
}

// run makes projectOne's flow run due and reports facts; nil facts report
// a failed read.
func (h *flowHarness) run(facts map[string]any) map[string]any {
	h.t.Helper()
	if _, e := h.s.db.Exec(`UPDATE project_flow_state SET next_flow_at=NULL`); e != nil {
		h.t.Fatal(e)
	}
	runs := h.sys("health_due", map[string]any{"limit": 2})["runs"].([]any)
	found := false
	for _, r := range runs {
		m := r.(map[string]any)
		found = found || m["linear_project"] == projectOne && m["flow"] == true && m["health"] == false
	}
	if !found {
		h.t.Fatal("no flow run due:", runs)
	}
	a := map[string]any{"linear_project": projectOne, "calls": 2}
	if facts == nil {
		a["error"] = "project_unreadable"
	} else {
		a["flow"] = facts
	}
	return h.sys("health_report", a)
}

func (h *flowHarness) settle(seq int, outcome, code string) map[string]any {
	h.t.Helper()
	return h.sys("project_flow_settle", map[string]any{"linear_project": projectOne, "seq": seq, "outcome": outcome, "error": code, "calls": 2})
}

func (h *flowHarness) messages(kind string) []string {
	h.t.Helper()
	rows, e := h.s.db.Query(`SELECT body FROM messages WHERE kind=? ORDER BY sequence`, kind)
	if e != nil {
		h.t.Fatal(e)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var b string
		_ = rows.Scan(&b)
		out = append(out, b)
	}
	return out
}

func (h *flowHarness) moves() []string {
	h.t.Helper()
	rows, e := h.s.db.Query(`SELECT seq,rule,to_type,to_status_id,state FROM project_flow_moves WHERE linear_project_id=? ORDER BY seq`, projectOne)
	if e != nil {
		h.t.Fatal(e)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var seq int
		var rule, to, id, state string
		_ = rows.Scan(&seq, &rule, &to, &id, &state)
		out = append(out, fmt.Sprintf("%d %s %s %s %s", seq, rule, to, id, state))
	}
	return out
}

func moveOf(out map[string]any) map[string]any {
	m, _ := out["move"].(map[string]any)
	return m
}

// L5: project flow is turned on only on a proven server; a policy already
// on stays editable. Existing policies read it as off.
func TestProjectFlowNeedsTheProvenFlag(t *testing.T) {
	h := newHealthHarness(t)
	on := map[string]any{"enabled": true, "teams": map[string]any{}, "project_flow": true}
	if _, e := h.s.Execute(context.Background(), h.request("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": on})); e == nil || !strings.Contains(e.Error(), ProjectFlowUnproven) {
		t.Fatal("project flow turned on unproven:", e)
	}
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": map[string]any{"enabled": true, "teams": map[string]any{}}})
	if view := h.call("status_policy_get", "", nil).(map[string]any); view["project_flow"] != nil || view["project_flow_lines"].([]any)[0].(map[string]any)["project_flow"] != "off" {
		t.Fatal("a policy without project_flow reads it on:", view)
	}
	if runs := h.sys("health_due", map[string]any{"limit": 2})["runs"].([]any); len(runs) != 0 {
		t.Fatal("project flow ran while off:", runs)
	}
	h.s.SetProjectFlowProven(true)
	h.call("status_policy_set", "", map[string]any{"expected_revision": 1, "status_policy": on})
	h.s.SetProjectFlowProven(false)
	on["claim_from_triage"] = true
	h.call("status_policy_set", "", map[string]any{"expected_revision": 2, "status_policy": on})
	if _, e := ParseStatusPolicy(map[string]any{"enabled": true, "project_targets": map[string]any{"started": "Kickoff"}}); e == nil {
		t.Fatal("a target by name accepted")
	}
}

// R1 from backlog and planned to the lowest-position started status; none
// from paused, completed or canceled; L2: completed work older than the
// project starts nothing.
func TestProjectFlowStartsTheProject(t *testing.T) {
	for _, from := range []string{psBacklog, psPlanned} {
		h := newFlowHarness(t, "human")
		m := moveOf(h.run(flowFacts(from, issues{total: 3, started: 1})))
		if m == nil || m["to_status_id"] != psStarted2 || m["rule"] != "R1" || m["to_type"] != "started" {
			t.Fatal(from, m)
		}
	}
	for _, from := range []string{psPaused, psCompleted, psCanceled} {
		h := newFlowHarness(t, "agent")
		if out := h.run(flowFacts(from, issues{total: 3, started: 1})); moveOf(out) != nil || out["flow"] != "left" {
			t.Fatal("R3 broken from", from, out)
		}
	}
	h := newFlowHarness(t, "human")
	if out := h.run(flowFacts(psBacklog, issues{total: 3, completed: 1})); moveOf(out) != nil {
		t.Fatal("work done before the project started it:", out)
	}
	if m := moveOf(h.run(flowFacts(psBacklog, issues{total: 3, completed: 1, since: 1}))); m == nil || m["to_type"] != "started" {
		t.Fatal("work done since the project didn't start it:", m)
	}
}

// R2: all closed with at least one completed completes the project when
// agents close it; an all-canceled project is told once and never moved
// (L3); one open issue or a partial read finishes nothing (L1).
func TestProjectFlowFinishesTheProject(t *testing.T) {
	h := newFlowHarness(t, "agent")
	m := moveOf(h.run(flowFacts(psStarted, issues{total: 4, completed: 3, canceled: 1})))
	if m == nil || m["rule"] != "R2" || m["to_status_id"] != psCompleted {
		t.Fatal("R2 with agents closing:", m)
	}
	for name, n := range map[string]issues{
		"one open":     {total: 4, completed: 3},
		"partial read": {total: 4, completed: 4, partial: true},
	} {
		h := newFlowHarness(t, "agent")
		if out := h.run(flowFacts(psStarted, n)); moveOf(out) != nil || len(h.messages("project-done")) != 0 {
			t.Fatal(name, out)
		}
	}
	h = newFlowHarness(t, "agent")
	for i := 0; i < 2; i++ {
		if out := h.run(flowFacts(psStarted, issues{total: 2, canceled: 2})); moveOf(out) != nil || out["flow"] != "all_canceled" {
			t.Fatal("an all-canceled project moved:", out)
		}
	}
	if n := h.messages("project-done"); len(n) != 1 || !strings.Contains(n[0], "All 2 issues of Linear project "+projectOne+" are canceled") || !strings.Contains(n[0], flowLine) {
		t.Fatal(n)
	}
	// L1: R1 needs only one started issue, so it fires on a partial read.
	h = newFlowHarness(t, "human")
	if m := moveOf(h.run(flowFacts(psPlanned, issues{total: 9, started: 1, partial: true}))); m == nil || m["rule"] != "R1" {
		t.Fatal("R1 on a partial read:", m)
	}
}

// R2 when people close: exactly one notice per done episode; a new issue
// ends the episode, and a second done notifies again.
func TestProjectFlowDoneNoticeWhenPeopleClose(t *testing.T) {
	h := newFlowHarness(t, "human")
	done := flowFacts(psStarted, issues{total: 3, completed: 2, canceled: 1})
	for i := 0; i < 2; i++ {
		if out := h.run(done); moveOf(out) != nil {
			t.Fatal("people close it, but it moved:", out)
		}
	}
	n := h.messages("project-done")
	if len(n) != 1 || !strings.Contains(n[0], "All 3 issues of Linear project "+projectOne+" are closed (2 completed, 1 canceled). Its status is still started. Who closes it is people") || strings.Contains(n[0], "@") {
		t.Fatal(n)
	}
	h.run(flowFacts(psStarted, issues{total: 4, completed: 2, canceled: 1, started: 1}))
	h.run(flowFacts(psStarted, issues{total: 4, completed: 3, canceled: 1}))
	if n = h.messages("project-done"); len(n) != 2 || !strings.Contains(n[1], "All 4 issues") {
		t.Fatal("a second done episode:", n)
	}
}

// A move settles accepted, refused (told once a day) or uncertain; an
// uncertain one blocks another send and is settled by the next read,
// never sent twice.
func TestProjectFlowSettlesMoves(t *testing.T) {
	h := newFlowHarness(t, "human")
	started := flowFacts(psBacklog, issues{total: 2, started: 1})
	m := moveOf(h.run(started))
	if s := h.settle(m["seq"].(int), "accepted", ""); s["state"] != "accepted" {
		t.Fatal(s)
	}
	var seen string
	if e := h.s.db.QueryRow(`SELECT last_seen_type FROM project_flow_state`).Scan(&seen); e != nil || seen != "started" {
		t.Fatal(seen, e)
	}

	h = newFlowHarness(t, "human")
	for i := 0; i < 2; i++ {
		m = moveOf(h.run(started))
		if s := h.settle(m["seq"].(int), "refused", "app_scope"); s["state"] != "refused" {
			t.Fatal(s)
		}
	}
	if n := h.messages("project-flow"); len(n) != 1 || !strings.Contains(n[0], "the ClawBridge app lacks Linear's write scope (app_scope)") {
		t.Fatal("refusal notices:", n)
	}

	h = newFlowHarness(t, "human")
	m = moveOf(h.run(started))
	if s := h.settle(m["seq"].(int), "uncertain", "unavailable"); s["state"] != "pending" {
		t.Fatal(s)
	}
	if out := h.run(started); moveOf(out) != nil || out["flow"] != "pending" {
		t.Fatal("a pending move didn't block another:", out)
	}
	if out := h.run(flowFacts(psStarted2, issues{total: 2, started: 1})); moveOf(out) != nil {
		t.Fatal(out)
	}
	if got := h.moves(); len(got) != 1 || got[0] != "1 R1 started "+psStarted2+" accepted" {
		t.Fatal("the read didn't settle the uncertain move:", got)
	}
	// An hour on, a move the read never shows settles failed, and the next
	// run reads before it sends again.
	h = newFlowHarness(t, "human")
	m = moveOf(h.run(started))
	h.settle(m["seq"].(int), "uncertain", "unavailable")
	if _, e := h.s.db.Exec(`UPDATE project_flow_moves SET sent_at='2000-01-01T00:00:00.000000000Z'`); e != nil {
		t.Fatal(e)
	}
	if again := moveOf(h.run(started)); again == nil || again["seq"] != 2 {
		t.Fatal(again)
	}
	if got := h.moves(); got[0] != "1 R1 started "+psStarted2+" failed" {
		t.Fatal(got)
	}
}

// R4: a person's move back holds the project with one notice; resume is
// the owner's or a lead's, never a session's, and clears the hold.
func TestProjectFlowHoldsAMoveBack(t *testing.T) {
	h := newFlowHarness(t, "agent")
	m := moveOf(h.run(flowFacts(psBacklog, issues{total: 2, started: 1})))
	h.settle(m["seq"].(int), "accepted", "")
	for i := 0; i < 2; i++ {
		if out := h.run(flowFacts(psBacklog, issues{total: 2, started: 1})); moveOf(out) != nil || out["flow"] != "held" {
			t.Fatal("moved after a person moved it back:", out)
		}
	}
	n := h.messages("project-held")
	if len(n) != 1 || !strings.Contains(n[0], "moved back by a person, to backlog") || !strings.Contains(n[0], "project-flow resume --linear-project "+projectOne) {
		t.Fatal(n)
	}
	view := h.call("status_policy_get", "", nil).(map[string]any)
	if line := view["project_flow_lines"].([]any)[0].(map[string]any); !strings.HasPrefix(line["project_flow"].(string), "held (moved back by a person since ") {
		t.Fatal(line)
	}
	if _, e := h.s.Execute(context.Background(), h.request("project_flow_resume", "a", map[string]any{"linear_project": projectOne})); e == nil || !strings.Contains(e.Error(), "session can never") {
		t.Fatal("a session resumed project flow:", e)
	}
	h.call("project_flow_resume", "", map[string]any{"linear_project": projectOne})
	if m = moveOf(h.run(flowFacts(psBacklog, issues{total: 2, started: 1}))); m == nil || m["rule"] != "R1" {
		t.Fatal("resume didn't clear the hold:", m)
	}
	if _, e := h.s.Execute(context.Background(), h.request("project_flow_resume", "", map[string]any{"linear_project": projectTwo})); e == nil || !strings.Contains(e.Error(), "not held") {
		t.Fatal(e)
	}
}

// SF4-M1: any move back holds, whoever made it: a person who reopens a
// completed project is never re-completed by R2.
func TestProjectFlowHoldsAReopenedProject(t *testing.T) {
	h := newFlowHarness(t, "agent")
	if out := h.run(flowFacts(psCompleted, issues{total: 2, completed: 2})); moveOf(out) != nil || out["flow"] != "left" {
		t.Fatal(out)
	}
	if out := h.run(flowFacts(psStarted, issues{total: 2, completed: 2})); moveOf(out) != nil || out["flow"] != "held" {
		t.Fatal("re-completed a reopened project:", out)
	}
	if len(h.messages("project-held")) != 1 {
		t.Fatal(h.messages("project-held"))
	}
}

// The override wins when it is a live status of the type; otherwise
// nothing moves. A failed read is tried again in an hour.
func TestProjectFlowTargets(t *testing.T) {
	for want, override := range map[string]string{psStarted: psStarted, "": psDoneOld} {
		h := newFlowHarness(t, "human")
		h.call("status_policy_set", "", map[string]any{"expected_revision": 1, "status_policy": map[string]any{"enabled": true, "teams": map[string]any{}, "project_flow": true, "project_targets": map[string]any{"started": override}}})
		out := h.run(flowFacts(psBacklog, issues{total: 1, started: 1}))
		if m := moveOf(out); want == "" && (m != nil || out["flow"] != "target_unknown") || want != "" && (m == nil || m["to_status_id"] != want) {
			t.Fatal(override, out)
		}
	}
	h := newFlowHarness(t, "human")
	if out := h.run(nil); out["flow"] != "error" {
		t.Fatal(out)
	}
	var next string
	if e := h.s.db.QueryRow(`SELECT next_flow_at FROM project_flow_state`).Scan(&next); e != nil || next == "" {
		t.Fatal(next, e)
	}
}

// show says, per bound project, follows the work or off, and the last
// move with the rule it followed.
func TestProjectFlowShow(t *testing.T) {
	h := newFlowHarness(t, "agent")
	m := moveOf(h.run(flowFacts(psStarted, issues{total: 1, completed: 1})))
	h.settle(m["seq"].(int), "accepted", "")
	view := h.call("status_policy_get", "", nil).(map[string]any)
	lines := view["project_flow_lines"].([]any)
	one := lines[0].(map[string]any)
	why := one["last_move"].(map[string]any)["why"]
	if len(lines) != 2 || one["project_flow"] != "follows the work" || why != "moved to completed by ClawBridge: all issues closed; the project's close rule is agents (status policy rev 1)" || view["project_flow"] != true {
		t.Fatal(view)
	}
	h.call("status_policy_set", "", map[string]any{"expected_revision": 1, "status_policy": map[string]any{"enabled": true, "teams": map[string]any{}}})
	view = h.call("status_policy_get", "", nil).(map[string]any)
	if view["project_flow_lines"].([]any)[1].(map[string]any)["project_flow"] != "off" {
		t.Fatal(view)
	}
}
