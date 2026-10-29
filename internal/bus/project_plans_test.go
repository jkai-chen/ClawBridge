package bus

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const planLeadUser = "d2000000-0000-4000-8000-0000000000d2"

func planArgs(op string, confirm bool, teams ...string) map[string]any {
	return map[string]any{"operation_id": op, "linear_project": addProject, "team_ids": teams, "fields": []any{"targetDate", "priority"}, "plan_sha256": strings.Repeat("a", 64), "confirm": confirm}
}

// plansHarness is the creates harness (lanes "one" writes teamOne, "both"
// writes both; c is the coordinator) with a project set holding
// addProject, on a server that has proven project plans.
func plansHarness(t *testing.T) (*harness, *time.Time) {
	t.Helper()
	h, now := createsHarness(t)
	a := taskboardArgsLanes(1)
	a["mode"], a["linear_project_ids"] = "project_set", []string{addProject}
	h.call("taskboard_set_v2", "", a)
	h.s.SetProjectPlanProven(true)
	return h, now
}

// setTeamLead stores team roles in which linked (a principal, or "" for
// none) leads team.
func (h *harness) setTeamLead(linked, team string) {
	h.t.Helper()
	m := TeamRoles{Teams: []TeamRoleTeam{{TeamID: team, Leads: []string{planLeadUser}}}, People: []TeamRolePerson{{LinearUserID: planLeadUser, Role: "lead", Teams: []string{team}, Principal: linked}}}
	raw, _ := json.Marshal(m)
	if _, e := h.s.db.Exec(`DELETE FROM team_role_mapping`); e != nil {
		h.t.Fatal(e)
	}
	if _, e := h.s.db.Exec(`INSERT INTO team_role_mapping(project_id,revision,mapping,updated_by,updated_at) VALUES(?,1,?,?,?)`, h.projectID(), string(raw), h.principal(), "2026-10-29T00:00:00Z"); e != nil {
		h.t.Fatal(e)
	}
}

func planRows(t *testing.T, h *harness) int {
	t.Helper()
	var n int
	if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM linear_write_reservations WHERE kind='project_plan'`).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// Unproven, the preview authorizes and records nothing, and a confirm is
// refused; proven, the owner without a session reserves, a repeat returns
// the row, and the same operation ID with another plan is a conflict.
// The arguments are checked, and history has IDs and field names only.
func TestProjectPlanGateAndOwner(t *testing.T) {
	h, _ := plansHarness(t)
	h.s.SetProjectPlanProven(false)
	preview := h.call("project_plan_reserve", "", planArgs("p-1", false, teamOne)).(map[string]any)
	if preview["preview"] != true || preview["by"] != "owner" || preview["proven"] != false || !reflect.DeepEqual(preview["fields"], []string{"priority", "targetDate"}) {
		t.Fatal(preview)
	}
	if e := h.errorOf("project_plan_reserve", "", planArgs("p-1", true, teamOne)); !strings.Contains(e, "project_plan_unproven") {
		t.Fatal("an unproven server reserved:", e)
	}
	if n := planRows(t, h); n != 0 {
		t.Fatal("a preview or an unproven confirm recorded", n)
	}
	h.s.SetProjectPlanProven(true)
	with := func(k string, v any) map[string]any { a := planArgs("p-1", true, teamOne); a[k] = v; return a }
	for want, args := range map[string]map[string]any{
		"isn't in this binding":   with("linear_project", otherProject),
		"linear_project":          with("linear_project", "Roadmap"),
		"list of UUIDs":           with("team_ids", []any{"Platform"}),
		"team_ids must list":      with("team_ids", []any{}),
		"fields names only":       with("fields", []any{"dueDate"}),
		"at least one":            with("fields", []any{}),
		"lower-case SHA-256":      with("plan_sha256", "x"),
		"confirm must be":         with("confirm", "yes"),
		"operation_id must be 1–": with("operation_id", ""),
	} {
		if e := h.errorOf("project_plan_reserve", "", args); !strings.Contains(e, want) {
			t.Fatal(want, e)
		}
	}
	got := h.call("project_plan_reserve", "", planArgs("p-1", true, teamOne)).(map[string]any)
	if got["state"] != "reserved" || got["by"] != "owner" || got["lane"] != "" || got["team_id"] != "" || got["linear_project"] != addProject || got["replay"] != false || !reflect.DeepEqual(got["fields"], []string{"priority", "targetDate"}) {
		t.Fatal(got)
	}
	if again := h.call("project_plan_reserve", "", planArgs("p-1", true, teamOne)).(map[string]any); again["replay"] != true {
		t.Fatal("a repeat:", again)
	}
	for name, args := range map[string]map[string]any{
		"plan":   with("plan_sha256", strings.Repeat("b", 64)),
		"fields": with("fields", []any{"startDate"}),
		"teams":  with("team_ids", []any{teamTwo}),
	} {
		if e := h.errorOf("project_plan_reserve", "", args); !strings.Contains(e, "different Linear write") {
			t.Fatal("op reused with another", name, e)
		}
	}
	// A team add can't take a plan's operation ID.
	if e := h.errorOf("project_team_add_reserve", "", teamAddArgs("p-1", teamTwo, teamOne)); !strings.Contains(e, "different Linear write") && !strings.Contains(e, "unproven") {
		t.Fatal(e)
	}
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_plan' AND action='reserve'`).Scan(&details); e != nil || !strings.Contains(details, addProject) || !strings.Contains(details, `"fields":["priority","targetDate"]`) || !strings.Contains(details, `"by":"owner"`) || strings.Contains(details, strings.Repeat("a", 64)) || strings.Contains(details, h.principal()) {
		t.Fatal("history details:", details, e)
	}
}

// A team board has no projects: refused, preview too.
func TestProjectPlanRefusesATeamBoard(t *testing.T) {
	h, _ := createsHarness(t)
	h.s.SetProjectPlanProven(true)
	for _, confirm := range []bool{false, true} {
		if e := h.errorOf("project_plan_reserve", "", planArgs("p-1", confirm, teamOne)); !strings.Contains(e, "a team board has no projects") {
			t.Fatal(e)
		}
	}
}

// Without a session: the owner, a project lead (project_lead), a team's
// lead of a bound write team the project holds (team_lead, naming the
// team); a lead of a team the project doesn't hold, an unbound team's
// lead, an unlinked lead row and a plain member are refused. Only the
// owner can be the harness's principal, so the confirmer is read directly.
func TestProjectPlanConfirmerWithoutSession(t *testing.T) {
	h, _ := plansHarness(t)
	me := h.principal()
	who := func(role string, teams ...string) (string, string, error) {
		tx, pid := ledgerTx(t, h)
		defer tx.Rollback()
		b, _, err := storedBinding(context.Background(), tx, pid)
		if err != nil {
			t.Fatal(err)
		}
		by, _, team, err := h.s.projectPlanConfirmer(context.Background(), tx, pid, role, nil, b, teams, true, nil)
		return by, team, err
	}
	if by, team, err := who("owner", teamOne); by != "owner" || team != "" || err != nil {
		t.Fatal(by, team, err)
	}
	if by, team, err := who("lead", teamOne); by != "project_lead" || team != "" || err != nil {
		t.Fatal(by, team, err)
	}
	if _, _, err := who("member", teamOne, teamTwo); err == nil || !strings.Contains(err.Error(), "ask the project owner or a lead") {
		t.Fatal("a member with no roles:", err)
	}
	h.setTeamLead(me, teamTwo)
	if by, team, err := who("member", teamOne, teamTwo); by != "team_lead" || team != teamTwo || err != nil {
		t.Fatal(by, team, err)
	}
	if _, _, err := who("member", teamOne); err == nil {
		t.Fatal("the lead of a team the project doesn't hold confirmed")
	}
	h.setTeamLead(me, unboundTeam)
	if _, _, err := who("member", unboundTeam); err == nil {
		t.Fatal("an unbound team's lead confirmed")
	}
	h.setTeamLead("", teamTwo)
	if _, _, err := who("member", teamTwo); err == nil {
		t.Fatal("an unlinked lead row confirmed")
	}
}

// The coordinator confirms inside its lane: refused unpinned, and when its
// lane holds none of the project's teams; in lane it reserves by
// coordinator with its lane and the lane's team. Without lanes,
// outside_lane. A session that is neither coordinator nor a lead's is
// told to ask.
func TestProjectPlanCoordinatorLane(t *testing.T) {
	h, _ := plansHarness(t)
	if e := h.errorOf("project_plan_reserve", "a", planArgs("p-1", true, teamOne)); !strings.Contains(e, "ask the project owner or a lead") {
		t.Fatal("another session confirmed:", e)
	}
	if e := h.errorOf("project_plan_reserve", "c", planArgs("p-1", true, teamOne)); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "no lane") {
		t.Fatal("an unpinned coordinator confirmed:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "one"})
	if e := h.errorOf("project_plan_reserve", "c", planArgs("p-1", true, teamTwo)); !strings.Contains(e, "outside_lane: the project holds no team of lane one") {
		t.Fatal("another lane's project:", e)
	}
	got := h.call("project_plan_reserve", "c", planArgs("p-1", true, teamOne, teamTwo)).(map[string]any)
	if got["by"] != "coordinator" || got["lane"] != "one" || got["team_id"] != teamOne {
		t.Fatal(got)
	}
	// The owner can't take the coordinator's operation ID.
	if e := h.errorOf("project_plan_reserve", "", planArgs("p-1", true, teamOne, teamTwo)); !strings.Contains(e, "different Linear write") {
		t.Fatal("the owner took the coordinator's operation ID:", e)
	}
	a := taskboardArgsV2(2, addProject)
	a["linear_team_ids"] = []string{teamOne, teamTwo}
	h.call("taskboard_set_v2", "", a)
	for _, confirm := range []bool{false, true} {
		if e := h.errorOf("project_plan_reserve", "c", planArgs("p-2", confirm, teamOne)); !strings.Contains(e, "outside_lane: this binding has no lanes") {
			t.Fatal("a no-lanes coordinator:", e)
		}
	}
}

// A session whose owning principal leads a team the project holds
// confirms inside a lane that holds that team (team_lead_session, naming
// the lane and the team); not unpinned, not in a lane without the team,
// not when the lead row is another principal's, and not without lanes.
// The pin and the role are re-read at confirm, after a preview.
func TestProjectPlanTeamLeadSession(t *testing.T) {
	h, _ := plansHarness(t)
	me := h.principal()
	h.setTeamLead(me, teamTwo)
	if e := h.errorOf("project_plan_reserve", "a", planArgs("p-1", true, teamTwo)); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "no lane") {
		t.Fatal("an unpinned lead session:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "a", "lane": "one"})
	if e := h.errorOf("project_plan_reserve", "a", planArgs("p-1", true, teamOne, teamTwo)); !strings.Contains(e, "outside_lane") {
		t.Fatal("a lane without the led team:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "a", "lane": "both"})
	preview := h.call("project_plan_reserve", "a", planArgs("p-1", false, teamOne, teamTwo)).(map[string]any)
	if preview["preview"] != true || preview["by"] != "team_lead_session" || preview["lane"] != "both" || preview["team_id"] != teamTwo {
		t.Fatal(preview)
	}
	// The pin moves between the preview and the confirm: refused.
	h.pin("lane_pin_set", map[string]any{"expected_revision": 2, "kind": "session", "key": "a", "lane": "one"})
	if e := h.errorOf("project_plan_reserve", "a", planArgs("p-1", true, teamOne, teamTwo)); !strings.Contains(e, "outside_lane") {
		t.Fatal("a pin change wasn't re-checked:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 3, "kind": "session", "key": "a", "lane": "both"})
	// The lead row moves to another principal between preview and confirm.
	h.setTeamLead("someone-else@example.test", teamTwo)
	if e := h.errorOf("project_plan_reserve", "a", planArgs("p-1", true, teamOne, teamTwo)); !strings.Contains(e, "ask the project owner or a lead") {
		t.Fatal("a session whose owner leads nothing:", e)
	}
	h.setTeamLead(me, teamTwo)
	got := h.call("project_plan_reserve", "a", planArgs("p-1", true, teamOne, teamTwo)).(map[string]any)
	if got["by"] != "team_lead_session" || got["lane"] != "both" || got["team_id"] != teamTwo {
		t.Fatal(got)
	}
	var row struct{ session, principal string }
	if e := h.s.db.QueryRow(`SELECT session_id,principal FROM linear_write_reservations WHERE operation_id='p-1'`).Scan(&row.session, &row.principal); e != nil || row.session != h.sessionID("a") || row.principal != me {
		t.Fatal(row, e)
	}
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_plan' AND action='reserve'`).Scan(&details); e != nil || !strings.Contains(details, `"by":"team_lead_session"`) || !strings.Contains(details, `"session_id":"`+h.sessionID("a")) || !strings.Contains(details, `"principal":"`+me) || !strings.Contains(details, `"lane":"both"`) {
		t.Fatal(details, e)
	}
	a := taskboardArgsV2(2, addProject)
	a["linear_team_ids"] = []string{teamOne, teamTwo}
	h.call("taskboard_set_v2", "", a)
	if e := h.errorOf("project_plan_reserve", "a", planArgs("p-2", true, teamTwo)); !strings.Contains(e, "outside_lane: this binding has no lanes") {
		t.Fatal("a no-lanes lead session:", e)
	}
}

// The agent guard: without a session, a client's agent marker is refused,
// even the owner's, preview included; from a session the marker is
// ordinary.
func TestProjectPlanAgentGuard(t *testing.T) {
	h, _ := plansHarness(t)
	for _, confirm := range []bool{false, true} {
		a := planArgs("p-1", confirm, teamOne)
		a["agent_marker"] = "CLAUDECODE"
		if e := h.errorOf("project_plan_reserve", "", a); !strings.Contains(e, "CLAUDECODE says an agent runs this") {
			t.Fatal(e)
		}
	}
	a := planArgs("p-1", true, teamOne)
	a["agent_marker"] = 7
	if e := h.errorOf("project_plan_reserve", "", a); !strings.Contains(e, "agent_marker must be") {
		t.Fatal(e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "one"})
	a = planArgs("p-1", true, teamOne)
	a["agent_marker"] = "CLAUDECODE"
	if got := h.call("project_plan_reserve", "c", a).(map[string]any); got["by"] != "coordinator" {
		t.Fatal(got)
	}
}

// Settle closes the caller's own reservation: accepted, accepted with the
// names of other changed fields (shown as accepted_with_changes),
// uncertain (open until acknowledged) or not_sent; changed names only the
// allow-list, never a planned field. History has names only.
func TestProjectPlanSettle(t *testing.T) {
	h, now := plansHarness(t)
	for _, op := range []string{"p-1", "p-2", "p-3", "p-4"} {
		h.call("project_plan_reserve", "", planArgs(op, true, teamOne))
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "one"})
	if e := h.errorOf("project_plan_settle", "c", map[string]any{"operation_id": "p-1", "outcome": "accepted"}); !strings.Contains(e, "no reservation") {
		t.Fatal("a session settled the owner's plan:", e)
	}
	settle := func(op, outcome string, changed ...any) map[string]any {
		return map[string]any{"operation_id": op, "outcome": outcome, "changed": changed}
	}
	for want, args := range map[string]map[string]any{
		"outcome must be":     settle("p-1", "created"),
		"changed names only":  settle("p-1", "accepted", "description"),
		"changed names only ": settle("p-1", "accepted", "priority"),
		"only an accepted":    settle("p-1", "uncertain", "lead"),
		"must be an array":    {"operation_id": "p-1", "outcome": "accepted", "changed": "lead"},
	} {
		if e := h.errorOf("project_plan_settle", "", args); !strings.Contains(e, strings.TrimSpace(want)) {
			t.Fatal(want, e)
		}
	}
	if got := h.call("project_plan_settle", "", settle("p-1", "accepted")).(map[string]any); got["state"] != "accepted" || len(got["changed"].([]string)) != 0 {
		t.Fatal(got)
	}
	got := h.call("project_plan_settle", "", settle("p-2", "accepted", "targetDateResolution", "lead", "lead")).(map[string]any)
	if got["state"] != "accepted_with_changes" || !reflect.DeepEqual(got["changed"], []string{"lead", "targetDateResolution"}) || got["replay"] != false || !reflect.DeepEqual(got["fields"], []string{"priority", "targetDate"}) {
		t.Fatal(got)
	}
	if again := h.call("project_plan_settle", "", settle("p-2", "accepted", "lead", "targetDateResolution")).(map[string]any); again["replay"] != true {
		t.Fatal("a repeat:", again)
	}
	for name, args := range map[string]map[string]any{"outcome": settle("p-2", "uncertain"), "changed": settle("p-2", "accepted", "lead")} {
		if e := h.errorOf("project_plan_settle", "", args); !strings.Contains(e, "already settled") {
			t.Fatal(name, e)
		}
	}
	h.call("project_plan_settle", "", settle("p-3", "uncertain"))
	h.call("project_plan_settle", "", settle("p-4", "not_sent"))
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_plan' AND action='accepted' AND entity_key='p-2'`).Scan(&details); e != nil || !strings.Contains(details, `"changed":["lead","targetDateResolution"]`) || !strings.Contains(details, `"fields":["priority","targetDate"]`) {
		t.Fatal(details, e)
	}
	// The uncertain plan stays open: in the one stale count until acknowledged.
	*now = now.Add(time.Minute)
	u := h.overview("")["project_creates"].(map[string]any)["unconfirmed"].([]map[string]any)
	if len(u) != 1 || u[0]["write"] != projectPlanKind || u[0]["subject"] != addProject {
		t.Fatal(u)
	}
	// A session never acknowledges; a clean accepted plan has nothing to
	// acknowledge; the owner acknowledges an uncertain one and one with
	// changes.
	if e := h.errorOf("project_create_ack", "c", map[string]any{"operation_id": "p-3", "instance": "c-instance"}); !strings.Contains(e, "a session can't acknowledge") {
		t.Fatal("a session acknowledged:", e)
	}
	if e := h.errorOf("project_create_ack", "", map[string]any{"operation_id": "p-1"}); !strings.Contains(e, "no unacknowledged") {
		t.Fatal("a clean plan acknowledged:", e)
	}
	for _, op := range []string{"p-2", "p-3"} {
		h.call("project_create_ack", "", map[string]any{"operation_id": op})
	}
	if u := h.overview("")["project_creates"].(map[string]any)["unconfirmed"].([]map[string]any); len(u) != 0 {
		t.Fatal("acknowledged:", u)
	}
	// A not_sent plan reopens on the same confirm.
	if again := h.call("project_plan_reserve", "", planArgs("p-4", true, teamOne)).(map[string]any); again["state"] != "reserved" || again["replay"] != false {
		t.Fatal("not_sent didn't reopen:", again)
	}
}

// The acknowledge rule for a project plan's row: its confirmer acknowledges
// it when they were a team's lead (in a terminal or through a session they
// own) or a project lead; another principal doesn't; a team add's
// team_lead row and every other existing case stay the owner's.
func TestProjectPlanOwnAck(t *testing.T) {
	row := func(kind, by, principal string) map[string]any {
		return map[string]any{"kind": kind, "by_role": by, "principal": principal}
	}
	const me, other = "me@example.test", "other@example.test"
	for _, c := range []struct {
		row  map[string]any
		role string
		want bool
	}{
		{row(projectPlanKind, "team_lead_session", me), "member", true},
		{row(projectPlanKind, "team_lead", me), "member", true},
		{row(projectPlanKind, "team_lead_session", other), "member", false},
		{row(projectPlanKind, "team_lead", other), "lead", false},
		{row(projectPlanKind, "project_lead", me), "lead", true},
		{row(projectPlanKind, "project_lead", me), "member", false},
		{row(projectPlanKind, "coordinator", me), "lead", false},
		{row(projectTeamAddKind, "team_lead", me), "member", false},
		{row(projectTeamAddKind, "lead", me), "lead", true},
		{row(projectTeamAddKind, "lead", me), "member", false},
		{row(issueMoveKind, "coordinator", me), "lead", false},
	} {
		if got := ownAck(c.row, c.role, me); got != c.want {
			t.Error(c.row, c.role, got)
		}
	}
}

// The new roles are recorded; the kind's detail takes fields and changed
// only.
func TestLinearWriteProjectPlanRoles(t *testing.T) {
	h, _ := createsHarness(t)
	tx, pid := ledgerTx(t, h)
	for i, by := range []string{"project_lead", "team_lead_session"} {
		w := linearWrite{Kind: projectPlanKind, Operation: "op-" + by, Principal: "p", ByRole: by, Team: teamTwo, Subject: addProject, Plan: "plan" + string(rune('0'+i)), Detail: map[string]any{"fields": []string{"priority"}}}
		if row, _, err := reserveLinearWrite(context.Background(), tx, pid, w, time.Now()); err != nil || str(row, "by_role") != by {
			t.Fatal(row, err)
		}
	}
	w := linearWrite{Kind: projectPlanKind, Operation: "op-x", Principal: "p", ByRole: "owner", Subject: addProject, Plan: "plan", Detail: map[string]any{"values": "x"}}
	if _, _, err := reserveLinearWrite(context.Background(), tx, pid, w, time.Now()); err == nil {
		t.Fatal("a value key was stored")
	}
}

// The overview counts settled plans, newest first; only an accepted plan
// with changes that is not yet acknowledged is "with_changes"; a reserved,
// uncertain or not_sent plan is not counted as planned.
func TestProjectPlansOverview(t *testing.T) {
	h, now := plansHarness(t)
	view := func() map[string]any { return h.overview("")["project_plans"].(map[string]any) }
	if v := view(); v["planned"] != 0 || v["with_changes"] != 0 || len(v["recent"].([]map[string]any)) != 0 || v["keep_days"] != linearWriteKeepDays {
		t.Fatal(v)
	}
	for _, op := range []string{"p-1", "p-2", "p-3", "p-4", "p-5"} {
		h.call("project_plan_reserve", "", planArgs(op, true, teamOne))
	}
	// Reserved only: nothing is counted or listed.
	if v := view(); v["planned"] != 0 || len(v["recent"].([]map[string]any)) != 0 {
		t.Fatal("reserved rows counted:", v)
	}
	settle := func(op, outcome string, changed ...any) {
		*now = now.Add(time.Minute)
		h.call("project_plan_settle", "", map[string]any{"operation_id": op, "outcome": outcome, "changed": changed})
	}
	settle("p-1", "accepted")
	settle("p-2", "accepted", "lead")
	settle("p-3", "accepted", "members", "lead")
	settle("p-4", "uncertain")
	v := view()
	recent := v["recent"].([]map[string]any)
	if v["planned"] != 3 || v["with_changes"] != 2 || len(recent) != 4 {
		t.Fatal(v)
	}
	var order []any
	for _, r := range recent {
		order = append(order, r["operation_id"])
	}
	if !reflect.DeepEqual(order, []any{"p-4", "p-3", "p-2", "p-1"}) {
		t.Fatal("not newest first:", order)
	}
	if recent[1]["state"] != "accepted_with_changes" || !reflect.DeepEqual(recent[1]["changed"], []string{"lead", "members"}) || !reflect.DeepEqual(recent[1]["fields"], []string{"priority", "targetDate"}) {
		t.Fatal(recent[1])
	}
	// An acknowledged plan stays planned and listed, and stops counting.
	h.call("project_create_ack", "", map[string]any{"operation_id": "p-3"})
	v = view()
	if v["planned"] != 3 || v["with_changes"] != 1 || len(v["recent"].([]map[string]any)) != 4 {
		t.Fatal("acknowledged:", v)
	}
	if v["recent"].([]map[string]any)[1]["acknowledged_at"] == nil {
		t.Fatal("acknowledged_at missing:", v["recent"])
	}
}

// While the dispatcher is on, a domain's coordinator plans a project whose
// teams are all write teams of its lane, judged at confirm on the server's
// own read of the project's teams (project_teams), never the client's
// team_ids; it waits on the project_status_domain proof, and a partial,
// empty or cross-lane read is outside_lane. Its preview judges the client's
// read and records nothing.
func TestProjectPlanDomainCoordinator(t *testing.T) {
	h := domainCoordHarness(t) // c: the default domain's coordinator, lane "both"
	h.s.SetProjectPlanProven(true)
	with := func(op string, confirm bool, server []string, more bool) map[string]any {
		a := planArgs(op, confirm, teamOne, teamTwo)
		if server != nil {
			a["project_teams"], a["project_teams_more"] = server, more
		}
		return a
	}
	if got := h.call("project_plan_reserve", "c", with("d-0", false, nil, false)).(map[string]any); got["by"] != "coordinator" || got["lane"] != "both" || got["preview"] != true {
		t.Fatal("the preview:", got)
	}
	if e := h.errorOf("project_plan_reserve", "c", with("d-1", true, []string{teamOne, teamTwo}, false)); !strings.Contains(e, "project_status_domain_unproven") {
		t.Fatal("the domain gate off:", e)
	}
	h.s.SetProjectStatusDomainProven(true)
	for name, c := range map[string]struct {
		server []string
		more   bool
	}{
		"no server read (only the client's team_ids)": {nil, false},
		"a partial read": {[]string{teamOne}, true},
		"an empty read":  {[]string{}, false},
	} {
		if e := h.errorOf("project_plan_reserve", "c", with("d-1", true, c.server, c.more)); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "read in full") {
			t.Fatal(name, ": ", e)
		}
	}
	// The client's team_ids say both teams; the server's read adds a team
	// outside the lane: the read decides.
	if e := h.errorOf("project_plan_reserve", "c", with("d-1", true, []string{teamOne, unboundTeam}, false)); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "isn't a write team of lane both") {
		t.Fatal("a cross-lane project:", e)
	}
	got := h.call("project_plan_reserve", "c", with("d-1", true, []string{teamOne, teamTwo}, false)).(map[string]any)
	if got["by"] != "coordinator" || got["lane"] != "both" {
		t.Fatal(got)
	}
	// A narrower lane: lane "one" writes only teamOne.
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "c", "lane": "one"})
	if e := h.errorOf("project_plan_reserve", "c", with("d-2", true, []string{teamOne, teamTwo}, false)); !strings.Contains(e, "isn't a write team of lane one") {
		t.Fatal("a project wider than the lane:", e)
	}
	if got := h.call("project_plan_reserve", "c", with("d-3", true, []string{teamOne}, false)).(map[string]any); got["lane"] != "one" {
		t.Fatal(got)
	}
	// Another session of the same principal isn't a coordinator at all.
	if e := h.errorOf("project_plan_reserve", "a", with("d-4", true, []string{teamOne}, false)); !strings.Contains(e, "ask the project owner or a lead") {
		t.Fatal(e)
	}
}
