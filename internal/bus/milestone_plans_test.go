package bus

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	msMilestone = "f8000000-0000-4000-8000-0000000000a1"
	msCreated   = "f8000000-0000-4000-8000-0000000000c1"
)

var msNameHash = strings.Repeat("c", 64)

// msArgs is a create of a milestone; msUpdateArgs an update of msMilestone.
func msArgs(op string, confirm bool, teams ...string) map[string]any {
	return map[string]any{"operation_id": op, "linear_project": addProject, "team_ids": teams, "op": "create", "fields": []any{"targetDate", "name"}, "name_sha256": msNameHash, "plan_sha256": strings.Repeat("a", 64), "confirm": confirm}
}

func msUpdateArgs(op string, confirm bool, teams ...string) map[string]any {
	a := msArgs(op, confirm, teams...)
	a["op"], a["milestone"] = "update", msMilestone
	delete(a, "name_sha256")
	return a
}

// msHarness is the plans harness on a server that has proven milestone
// plans.
func msHarness(t *testing.T) (*harness, *time.Time) {
	t.Helper()
	h, now := plansHarness(t)
	h.s.SetMilestonePlanProven(true)
	return h, now
}

func msRows(t *testing.T, h *harness) int {
	t.Helper()
	var n int
	if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM linear_write_reservations WHERE kind='milestone_plan'`).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// Unproven, the preview authorizes and records nothing, and a confirm is
// refused; proven, the owner without a session reserves, a repeat returns
// the row, and the same operation ID with another plan is a conflict. The
// arguments are checked, and history has IDs and field names only.
func TestMilestonePlanGateAndOwner(t *testing.T) {
	h, _ := msHarness(t)
	h.s.SetMilestonePlanProven(false)
	preview := h.call("milestone_plan_reserve", "", msArgs("m-1", false, teamOne)).(map[string]any)
	if preview["preview"] != true || preview["by"] != "owner" || preview["proven"] != false || preview["op"] != "create" || !reflect.DeepEqual(preview["fields"], []string{"name", "targetDate"}) {
		t.Fatal(preview)
	}
	// The project-plan gate is another gate.
	if e := h.errorOf("milestone_plan_reserve", "", msArgs("m-1", true, teamOne)); !strings.Contains(e, "milestone_plan_unproven") {
		t.Fatal("an unproven server reserved:", e)
	}
	if n := msRows(t, h); n != 0 {
		t.Fatal("a preview or an unproven confirm recorded", n)
	}
	h.s.SetMilestonePlanProven(true)
	h.s.SetProjectPlanProven(false)
	with := func(k string, v any) map[string]any { a := msArgs("m-1", true, teamOne); a[k] = v; return a }
	upd := func(k string, v any) map[string]any { a := msUpdateArgs("m-1", true, teamOne); a[k] = v; return a }
	for want, args := range map[string]map[string]any{
		"isn't in this binding":      with("linear_project", otherProject),
		"linear_project":             with("linear_project", "Roadmap"),
		"list of UUIDs":              with("team_ids", []any{"Platform"}),
		"team_ids must list":         with("team_ids", []any{}),
		"op must be create or":       with("op", "delete"),
		"fields names only":          with("fields", []any{"description"}),
		"at least one":               with("fields", []any{}),
		"a create sets the name":     with("fields", []any{"targetDate"}),
		"name_sha256 must be":        with("name_sha256", "Beta"),
		"a create has no milestone":  with("milestone", msMilestone),
		"milestone must be a Linear": upd("milestone", "Alpha"),
		"lower-case SHA-256":         with("plan_sha256", "x"),
		"confirm must be":            with("confirm", "yes"),
		"operation_id must be 1–":    with("operation_id", ""),
		"unsupported argument":       with("description", "x"),
	} {
		if e := h.errorOf("milestone_plan_reserve", "", args); !strings.Contains(e, want) {
			t.Fatal(want, e)
		}
	}
	noMilestone := msUpdateArgs("m-1", true, teamOne)
	delete(noMilestone, "milestone")
	if e := h.errorOf("milestone_plan_reserve", "", noMilestone); !strings.Contains(e, "milestone must be a Linear UUID") {
		t.Fatal(e)
	}
	got := h.call("milestone_plan_reserve", "", msArgs("m-1", true, teamOne)).(map[string]any)
	if got["state"] != "reserved" || got["by"] != "owner" || got["lane"] != "" || got["team_id"] != "" || got["linear_project"] != addProject || got["op"] != "create" || got["milestone"] != "" || got["replay"] != false || !reflect.DeepEqual(got["fields"], []string{"name", "targetDate"}) {
		t.Fatal(got)
	}
	if again := h.call("milestone_plan_reserve", "", msArgs("m-1", true, teamOne)).(map[string]any); again["replay"] != true {
		t.Fatal("a repeat:", again)
	}
	other := msArgs("m-1", true, teamOne)
	other["name_sha256"] = strings.Repeat("d", 64)
	for name, args := range map[string]map[string]any{
		"plan":   with("plan_sha256", strings.Repeat("b", 64)),
		"fields": with("fields", []any{"name"}),
		"teams":  with("team_ids", []any{teamTwo}),
		"name":   other,
		"op":     msUpdateArgs("m-1", true, teamOne),
	} {
		if e := h.errorOf("milestone_plan_reserve", "", args); !strings.Contains(e, "different Linear write") {
			t.Fatal("op reused with another", name, e)
		}
	}
	// A project plan can't take a milestone plan's operation ID, nor the
	// other way round.
	h.s.SetProjectPlanProven(true)
	if e := h.errorOf("project_plan_reserve", "", planArgs("m-1", true, teamOne)); !strings.Contains(e, "different Linear write") {
		t.Fatal(e)
	}
	h.call("project_plan_reserve", "", planArgs("pp-1", true, teamOne))
	if e := h.errorOf("milestone_plan_reserve", "", msUpdateArgs("pp-1", true, teamOne)); !strings.Contains(e, "different Linear write") {
		t.Fatal(e)
	}
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='milestone_plan' AND action='reserve'`).Scan(&details); e != nil || !strings.Contains(details, addProject) || !strings.Contains(details, `"fields":["name","targetDate"]`) || !strings.Contains(details, `"op":"create"`) || !strings.Contains(details, `"by":"owner"`) || strings.Contains(details, strings.Repeat("a", 64)) || strings.Contains(details, msNameHash) || strings.Contains(details, h.principal()) {
		t.Fatal("history details:", details, e)
	}
	// The stored row holds the hash of the name, no name, and the detail
	// keys op and fields.
	var subject, detail string
	if e := h.s.db.QueryRow(`SELECT subject,detail FROM linear_write_reservations WHERE operation_id='m-1'`).Scan(&subject, &detail); e != nil || subject != addProject+":"+msNameHash || detail != `{"fields":["name","targetDate"],"op":"create"}` {
		t.Fatal(subject, detail, e)
	}
}

// A team board has no projects: refused, preview too.
func TestMilestonePlanRefusesATeamBoard(t *testing.T) {
	h, _ := createsHarness(t)
	h.s.SetMilestonePlanProven(true)
	for _, confirm := range []bool{false, true} {
		if e := h.errorOf("milestone_plan_reserve", "", msArgs("m-1", confirm, teamOne)); !strings.Contains(e, "a team board has no projects") {
			t.Fatal(e)
		}
	}
}

// Without a session: the owner, a project lead (project_lead), a team's lead
// of a bound write team the project holds (team_lead, naming the team); a
// lead of a team the project doesn't hold, an unbound team's lead, an
// unlinked lead row and a plain member are refused.
func TestMilestonePlanConfirmerWithoutSession(t *testing.T) {
	h, _ := msHarness(t)
	me := h.principal()
	who := func(role string, teams ...string) (string, string, error) {
		tx, pid := ledgerTx(t, h)
		defer tx.Rollback()
		b, _, err := storedBinding(context.Background(), tx, pid)
		if err != nil {
			t.Fatal(err)
		}
		by, _, team, err := h.s.planConfirmer(context.Background(), tx, pid, role, nil, b, teams, true, nil, milestonePlanWords)
		return by, team, err
	}
	if by, team, err := who("owner", teamOne); by != "owner" || team != "" || err != nil {
		t.Fatal(by, team, err)
	}
	if by, team, err := who("lead", teamOne); by != "project_lead" || team != "" || err != nil {
		t.Fatal(by, team, err)
	}
	if _, _, err := who("member", teamOne, teamTwo); err == nil || !strings.Contains(err.Error(), "ask the project owner or a lead") || !strings.Contains(err.Error(), "milestones") {
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
	// A reader is refused outright, and the confirmer for a project plan is
	// the same rule.
	h.setTeamLead(me, teamTwo)
	tx, pid := ledgerTx(t, h)
	defer tx.Rollback()
	b, _, _ := storedBinding(context.Background(), tx, pid)
	pb, _, pt, perr := h.s.projectPlanConfirmer(context.Background(), tx, pid, "member", nil, b, []string{teamOne, teamTwo}, true, nil)
	mb, _, mt, merr := h.s.planConfirmer(context.Background(), tx, pid, "member", nil, b, []string{teamOne, teamTwo}, true, nil, milestonePlanWords)
	if pb != mb || pt != mt || perr != nil || merr != nil {
		t.Fatal("the rules differ:", pb, mb, pt, mt, perr, merr)
	}
}

// The coordinator confirms inside its lane: refused unpinned, and when its
// lane holds none of the project's teams; in lane it reserves by coordinator
// with its lane and the lane's team. Without lanes, outside_lane. A session
// that is neither coordinator nor a lead's is told to ask.
func TestMilestonePlanCoordinatorLane(t *testing.T) {
	h, _ := msHarness(t)
	if e := h.errorOf("milestone_plan_reserve", "a", msArgs("m-1", true, teamOne)); !strings.Contains(e, "ask the project owner or a lead") {
		t.Fatal("another session confirmed:", e)
	}
	if e := h.errorOf("milestone_plan_reserve", "c", msArgs("m-1", true, teamOne)); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "no lane") || !strings.Contains(e, "milestone plan") {
		t.Fatal("an unpinned coordinator confirmed:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "one"})
	if e := h.errorOf("milestone_plan_reserve", "c", msArgs("m-1", true, teamTwo)); !strings.Contains(e, "outside_lane: the project holds no team of lane one") {
		t.Fatal("another lane's project:", e)
	}
	// Both an update and a create (Q2).
	for _, args := range []map[string]any{msArgs("m-1", true, teamOne, teamTwo), msUpdateArgs("m-2", true, teamOne, teamTwo)} {
		got := h.call("milestone_plan_reserve", "c", args).(map[string]any)
		if got["by"] != "coordinator" || got["lane"] != "one" || got["team_id"] != teamOne {
			t.Fatal(got)
		}
	}
	// The owner can't take the coordinator's operation ID.
	if e := h.errorOf("milestone_plan_reserve", "", msArgs("m-1", true, teamOne, teamTwo)); !strings.Contains(e, "different Linear write") {
		t.Fatal("the owner took the coordinator's operation ID:", e)
	}
	a := taskboardArgsV2(2, addProject)
	a["linear_team_ids"] = []string{teamOne, teamTwo}
	h.call("taskboard_set_v2", "", a)
	for _, confirm := range []bool{false, true} {
		if e := h.errorOf("milestone_plan_reserve", "c", msArgs("m-3", confirm, teamOne)); !strings.Contains(e, "outside_lane: this binding has no lanes") {
			t.Fatal("a no-lanes coordinator:", e)
		}
	}
}

// A session whose owning principal leads a team the project holds confirms
// inside a lane that holds that team (team_lead_session, naming the lane and
// the team); not unpinned, not in a lane without the team, not when the lead
// row is another principal's, and not without lanes. The pin and the role are
// re-read at confirm, after a preview.
func TestMilestonePlanTeamLeadSession(t *testing.T) {
	h, _ := msHarness(t)
	me := h.principal()
	h.setTeamLead(me, teamTwo)
	if e := h.errorOf("milestone_plan_reserve", "a", msArgs("m-1", true, teamTwo)); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "no lane") {
		t.Fatal("an unpinned lead session:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "a", "lane": "one"})
	if e := h.errorOf("milestone_plan_reserve", "a", msArgs("m-1", true, teamOne, teamTwo)); !strings.Contains(e, "outside_lane") {
		t.Fatal("a lane without the led team:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "a", "lane": "both"})
	preview := h.call("milestone_plan_reserve", "a", msArgs("m-1", false, teamOne, teamTwo)).(map[string]any)
	if preview["preview"] != true || preview["by"] != "team_lead_session" || preview["lane"] != "both" || preview["team_id"] != teamTwo {
		t.Fatal(preview)
	}
	// The pin moves between the preview and the confirm: refused.
	h.pin("lane_pin_set", map[string]any{"expected_revision": 2, "kind": "session", "key": "a", "lane": "one"})
	if e := h.errorOf("milestone_plan_reserve", "a", msArgs("m-1", true, teamOne, teamTwo)); !strings.Contains(e, "outside_lane") {
		t.Fatal("a pin change wasn't re-checked:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 3, "kind": "session", "key": "a", "lane": "both"})
	// The lead row moves to another principal between preview and confirm.
	h.setTeamLead("someone-else@example.test", teamTwo)
	if e := h.errorOf("milestone_plan_reserve", "a", msArgs("m-1", true, teamOne, teamTwo)); !strings.Contains(e, "ask the project owner or a lead") {
		t.Fatal("a session whose owner leads nothing:", e)
	}
	h.setTeamLead(me, teamTwo)
	got := h.call("milestone_plan_reserve", "a", msUpdateArgs("m-1", true, teamOne, teamTwo)).(map[string]any)
	if got["by"] != "team_lead_session" || got["lane"] != "both" || got["team_id"] != teamTwo {
		t.Fatal(got)
	}
	var row struct{ session, principal string }
	if e := h.s.db.QueryRow(`SELECT session_id,principal FROM linear_write_reservations WHERE operation_id='m-1'`).Scan(&row.session, &row.principal); e != nil || row.session != h.sessionID("a") || row.principal != me {
		t.Fatal(row, e)
	}
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='milestone_plan' AND action='reserve'`).Scan(&details); e != nil || !strings.Contains(details, `"by":"team_lead_session"`) || !strings.Contains(details, `"session_id":"`+h.sessionID("a")) || !strings.Contains(details, `"principal":"`+me) || !strings.Contains(details, `"lane":"both"`) || !strings.Contains(details, `"milestone":"`+msMilestone) {
		t.Fatal(details, e)
	}
	// Without lanes, the lead's session is outside_lane (reviewer (b)).
	a := taskboardArgsV2(2, addProject)
	a["linear_team_ids"] = []string{teamOne, teamTwo}
	h.call("taskboard_set_v2", "", a)
	if e := h.errorOf("milestone_plan_reserve", "a", msArgs("m-2", true, teamTwo)); !strings.Contains(e, "outside_lane: this binding has no lanes") {
		t.Fatal("a no-lanes lead session:", e)
	}
}

// While the dispatcher is on, a domain's coordinator creates and updates
// milestones of a project whose teams are all write teams of its lane,
// judged at confirm on the server's own read of the project's teams
// (project_teams), never the client's team_ids; it waits on the
// project_status_domain proof, and a partial, empty or cross-lane read is
// outside_lane. Its preview judges the client's read and records nothing.
// A session that is not the domain's coordinator is told to ask.
func TestMilestonePlanDomainCoordinator(t *testing.T) {
	h := domainCoordHarness(t) // c: the default domain's coordinator, lane "both"
	h.s.SetMilestonePlanProven(true)
	with := func(op string, confirm bool, server []string, more bool) map[string]any {
		a := msArgs(op, confirm, teamOne, teamTwo)
		a["name_sha256"] = digestOf(op)
		if server != nil {
			a["project_teams"], a["project_teams_more"] = server, more
		}
		return a
	}
	if got := h.call("milestone_plan_reserve", "c", with("d-0", false, nil, false)).(map[string]any); got["by"] != "coordinator" || got["lane"] != "both" || got["preview"] != true {
		t.Fatal("the preview:", got)
	}
	if e := h.errorOf("milestone_plan_reserve", "c", with("d-1", true, []string{teamOne, teamTwo}, false)); !strings.Contains(e, "project_status_domain_unproven") || !strings.Contains(e, "milestone plan") {
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
		if e := h.errorOf("milestone_plan_reserve", "c", with("d-1", true, c.server, c.more)); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "read in full") {
			t.Fatal(name, ": ", e)
		}
	}
	// The client's team_ids say both teams; the server's read adds a team
	// outside the lane: the read decides.
	if e := h.errorOf("milestone_plan_reserve", "c", with("d-1", true, []string{teamOne, unboundTeam}, false)); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "isn't a write team of lane both") {
		t.Fatal("a cross-lane project:", e)
	}
	got := h.call("milestone_plan_reserve", "c", with("d-1", true, []string{teamOne, teamTwo}, false)).(map[string]any)
	if got["by"] != "coordinator" || got["lane"] != "both" {
		t.Fatal(got)
	}
	// A narrower lane: lane "one" writes only teamOne.
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "c", "lane": "one"})
	if e := h.errorOf("milestone_plan_reserve", "c", with("d-2", true, []string{teamOne, teamTwo}, false)); !strings.Contains(e, "isn't a write team of lane one") {
		t.Fatal("a project wider than the lane:", e)
	}
	if got := h.call("milestone_plan_reserve", "c", with("d-3", true, []string{teamOne}, false)).(map[string]any); got["lane"] != "one" {
		t.Fatal(got)
	}
	// Another session of the same principal isn't a coordinator at all.
	if e := h.errorOf("milestone_plan_reserve", "a", with("d-4", true, []string{teamOne}, false)); !strings.Contains(e, "ask the project owner or a lead") {
		t.Fatal(e)
	}
}

// The agent guard: without a session, a client's agent marker is refused,
// even the owner's, preview included; from a session the marker is ordinary.
func TestMilestonePlanAgentGuard(t *testing.T) {
	h, _ := msHarness(t)
	for _, confirm := range []bool{false, true} {
		a := msArgs("m-1", confirm, teamOne)
		a["agent_marker"] = "CLAUDECODE"
		if e := h.errorOf("milestone_plan_reserve", "", a); !strings.Contains(e, "CLAUDECODE says an agent runs this") {
			t.Fatal(e)
		}
	}
	a := msArgs("m-1", true, teamOne)
	a["agent_marker"] = 7
	if e := h.errorOf("milestone_plan_reserve", "", a); !strings.Contains(e, "agent_marker must be") {
		t.Fatal(e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "one"})
	a = msArgs("m-1", true, teamOne)
	a["agent_marker"] = "CLAUDECODE"
	if got := h.call("milestone_plan_reserve", "c", a).(map[string]any); got["by"] != "coordinator" {
		t.Fatal(got)
	}
}

// N1: two creates of one name serialise on the name's hash. A create that is
// reserved, or uncertain and not acknowledged, blocks another operation's
// create of that name; a settled one, another name, an update, and the
// first's own repeat do not.
func TestMilestonePlanSameNameCreatesSerialise(t *testing.T) {
	h, now := msHarness(t)
	named := func(op, hash string) map[string]any {
		a := msArgs(op, true, teamOne)
		a["name_sha256"] = hash
		return a
	}
	h.call("milestone_plan_reserve", "", named("n-1", msNameHash))
	if e := h.errorOf("milestone_plan_reserve", "", named("n-2", msNameHash)); !strings.Contains(e, "another create of a milestone with this name") {
		t.Fatal("a second create of the name went through:", e)
	}
	// The first's own repeat is a replay, not a conflict.
	if again := h.call("milestone_plan_reserve", "", named("n-1", msNameHash)).(map[string]any); again["replay"] != true {
		t.Fatal(again)
	}
	// Another name and an update of any milestone are free.
	h.call("milestone_plan_reserve", "", named("n-3", strings.Repeat("e", 64)))
	h.call("milestone_plan_reserve", "", msUpdateArgs("n-4", true, teamOne))
	// Uncertain, not acknowledged: still blocks.
	h.call("milestone_plan_settle", "", map[string]any{"operation_id": "n-1", "outcome": "uncertain"})
	if e := h.errorOf("milestone_plan_reserve", "", named("n-2", msNameHash)); !strings.Contains(e, "another create of a milestone with this name") {
		t.Fatal("an uncertain create didn't block:", e)
	}
	// Acknowledged by the owner: the name is free again.
	*now = now.Add(time.Minute)
	h.call("project_create_ack", "", map[string]any{"operation_id": "n-1"})
	if got := h.call("milestone_plan_reserve", "", named("n-2", msNameHash)).(map[string]any); got["state"] != "reserved" {
		t.Fatal(got)
	}
	// Settled accepted: the name is free for a third (the client's read of
	// the project's milestones is what refuses a duplicate then).
	h.call("milestone_plan_settle", "", map[string]any{"operation_id": "n-2", "outcome": "accepted", "milestone": msCreated})
	if got := h.call("milestone_plan_reserve", "", named("n-5", msNameHash)).(map[string]any); got["state"] != "reserved" {
		t.Fatal(got)
	}
	// Not sent frees it too.
	h.call("milestone_plan_settle", "", map[string]any{"operation_id": "n-5", "outcome": "not_sent"})
	if got := h.call("milestone_plan_reserve", "", named("n-6", msNameHash)).(map[string]any); got["state"] != "reserved" {
		t.Fatal(got)
	}
	// The name is never stored: only its hash is in the subject.
	var subjects int
	if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM linear_write_reservations WHERE kind='milestone_plan' AND subject=?`, addProject+":"+msNameHash).Scan(&subjects); e != nil || subjects != 4 {
		t.Fatal(subjects, e)
	}
}

// Settle closes the caller's own reservation: accepted (a create with its
// milestone ID; with the names of other changed fields, shown as
// accepted_with_changes), uncertain (open until acknowledged) or not_sent;
// changed names only the allow-list, never a planned field. History has IDs
// and names of fields only.
func TestMilestonePlanSettle(t *testing.T) {
	h, now := msHarness(t)
	h.call("milestone_plan_reserve", "", msArgs("m-1", true, teamOne))
	for _, op := range []string{"m-2", "m-3", "m-4"} {
		h.call("milestone_plan_reserve", "", msUpdateArgs(op, true, teamOne))
	}
	h.call("milestone_plan_reserve", "", func() map[string]any {
		a := msArgs("m-5", true, teamOne)
		a["name_sha256"] = strings.Repeat("e", 64)
		return a
	}())
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "one"})
	if e := h.errorOf("milestone_plan_settle", "c", map[string]any{"operation_id": "m-1", "outcome": "accepted"}); !strings.Contains(e, "no reservation") {
		t.Fatal("a session settled the owner's plan:", e)
	}
	settle := func(op, outcome string, changed ...any) map[string]any {
		return map[string]any{"operation_id": op, "outcome": outcome, "changed": changed}
	}
	for want, args := range map[string]map[string]any{
		"outcome must be":       settle("m-2", "created"),
		"changed names only":    settle("m-2", "accepted", "description"),
		"only fields the plan":  settle("m-2", "accepted", "name"),
		"only an accepted":      settle("m-2", "uncertain", "sortOrder"),
		"must be an array":      {"operation_id": "m-2", "outcome": "accepted", "changed": "sortOrder"},
		"only an accepted plan": {"operation_id": "m-2", "outcome": "uncertain", "milestone": msMilestone},
		"must be the one":       {"operation_id": "m-2", "outcome": "accepted", "milestone": msCreated},
		"milestone must be a":   {"operation_id": "m-2", "outcome": "accepted", "milestone": "Alpha"},
	} {
		if e := h.errorOf("milestone_plan_settle", "", args); !strings.Contains(e, strings.TrimSpace(want)) {
			t.Fatal(want, e)
		}
	}
	// A create settles with the milestone it made.
	got := h.call("milestone_plan_settle", "", map[string]any{"operation_id": "m-1", "outcome": "accepted", "milestone": msCreated}).(map[string]any)
	if got["state"] != "accepted" || got["milestone"] != msCreated || got["op"] != "create" || len(got["changed"].([]string)) != 0 || got["replay"] != false {
		t.Fatal(got)
	}
	if again := h.call("milestone_plan_settle", "", map[string]any{"operation_id": "m-1", "outcome": "accepted", "milestone": msCreated}).(map[string]any); again["replay"] != true {
		t.Fatal("a repeat:", again)
	}
	if e := h.errorOf("milestone_plan_settle", "", map[string]any{"operation_id": "m-1", "outcome": "accepted", "milestone": msMilestone}); !strings.Contains(e, "settled") {
		t.Fatal("another result:", e)
	}
	// An update with other fields changed: its milestone is its subject.
	a := msUpdateArgs("m-6", true, teamOne)
	a["fields"] = []any{"name"}
	h.call("milestone_plan_reserve", "", a)
	got = h.call("milestone_plan_settle", "", settle("m-6", "accepted", "targetDate", "sortOrder", "sortOrder")).(map[string]any)
	if got["state"] != "accepted_with_changes" || !reflect.DeepEqual(got["changed"], []string{"sortOrder", "targetDate"}) || got["milestone"] != msMilestone || got["op"] != "update" || !reflect.DeepEqual(got["fields"], []string{"name"}) {
		t.Fatal(got)
	}
	for name, args := range map[string]map[string]any{"outcome": settle("m-6", "uncertain"), "changed": settle("m-6", "accepted", "targetDate")} {
		if e := h.errorOf("milestone_plan_settle", "", args); !strings.Contains(e, "already settled") {
			t.Fatal(name, e)
		}
	}
	h.call("milestone_plan_settle", "", settle("m-2", "uncertain"))
	h.call("milestone_plan_settle", "", settle("m-3", "not_sent"))
	// An uncertain create has no milestone.
	h.call("milestone_plan_settle", "", settle("m-5", "uncertain"))
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='milestone_plan' AND action='accepted' AND entity_key='m-6'`).Scan(&details); e != nil || !strings.Contains(details, `"changed":["sortOrder","targetDate"]`) || !strings.Contains(details, `"fields":["name"]`) || !strings.Contains(details, `"milestone":"`+msMilestone) {
		t.Fatal(details, e)
	}
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='milestone_plan' AND action='accepted' AND entity_key='m-1'`).Scan(&details); e != nil || !strings.Contains(details, `"milestone":"`+msCreated) || strings.Contains(details, msNameHash) {
		t.Fatal(details, e)
	}
	// The uncertain ones stay open: in the one stale count until acknowledged.
	*now = now.Add(time.Minute)
	u := h.overview("")["project_creates"].(map[string]any)["unconfirmed"].([]map[string]any)
	if len(u) != 2 || u[0]["write"] != milestonePlanKind {
		t.Fatal(u)
	}
	// A session never acknowledges; a clean accepted plan has nothing to
	// acknowledge; the owner acknowledges an uncertain one and one with
	// changes.
	if e := h.errorOf("project_create_ack", "c", map[string]any{"operation_id": "m-2", "instance": "c-instance"}); !strings.Contains(e, "a session can't acknowledge") {
		t.Fatal("a session acknowledged:", e)
	}
	if e := h.errorOf("project_create_ack", "", map[string]any{"operation_id": "m-1"}); !strings.Contains(e, "no unacknowledged") {
		t.Fatal("a clean plan acknowledged:", e)
	}
	for _, op := range []string{"m-2", "m-6", "m-5"} {
		h.call("project_create_ack", "", map[string]any{"operation_id": op})
	}
	if u := h.overview("")["project_creates"].(map[string]any)["unconfirmed"].([]map[string]any); len(u) != 0 {
		t.Fatal("acknowledged:", u)
	}
	// A not_sent plan reopens on the same confirm.
	if again := h.call("milestone_plan_reserve", "", msUpdateArgs("m-3", true, teamOne)).(map[string]any); again["state"] != "reserved" || again["replay"] != false {
		t.Fatal("not_sent didn't reopen:", again)
	}
}

// The acknowledge rule for a milestone plan's row is a project plan's: its
// confirmer acknowledges it when they were a team's lead (in a terminal or
// through a session they own) or a project lead; the session itself and
// another principal don't.
func TestMilestonePlanOwnAck(t *testing.T) {
	row := func(kind, by, principal string) map[string]any {
		return map[string]any{"kind": kind, "by_role": by, "principal": principal}
	}
	const me, other = "me@example.test", "other@example.test"
	for _, c := range []struct {
		row  map[string]any
		role string
		want bool
	}{
		{row(milestonePlanKind, "team_lead_session", me), "member", true},
		{row(milestonePlanKind, "team_lead", me), "member", true},
		{row(milestonePlanKind, "team_lead_session", other), "member", false},
		{row(milestonePlanKind, "team_lead", other), "lead", false},
		{row(milestonePlanKind, "project_lead", me), "lead", true},
		{row(milestonePlanKind, "project_lead", me), "member", false},
		{row(milestonePlanKind, "coordinator", me), "lead", false},
		{row(milestonePlanKind, "lead", me), "lead", true},
		{row(projectTeamAddKind, "team_lead", me), "member", false},
	} {
		if got := ownAck(c.row, c.role, me); got != c.want {
			t.Error(c.row, c.role, got)
		}
	}
}

// The roles are recorded; the kind's detail takes op, fields and changed
// only.
func TestLinearWriteMilestonePlanRoles(t *testing.T) {
	h, _ := createsHarness(t)
	tx, pid := ledgerTx(t, h)
	for i, by := range []string{"project_lead", "team_lead_session", "coordinator", "team_lead", "owner"} {
		w := linearWrite{Kind: milestonePlanKind, Operation: "op-" + by, Principal: "p", ByRole: by, Team: teamTwo, Subject: addProject + ":" + msMilestone, Plan: "plan" + string(rune('0'+i)), Detail: map[string]any{"op": "update", "fields": []string{"name"}}}
		if row, _, err := reserveLinearWrite(context.Background(), tx, pid, w, time.Now()); err != nil || str(row, "by_role") != by {
			t.Fatal(row, err)
		}
	}
	for _, key := range []string{"name", "values", "target_date"} {
		w := linearWrite{Kind: milestonePlanKind, Operation: "op-x", Principal: "p", ByRole: "owner", Subject: addProject, Plan: "plan", Detail: map[string]any{key: "x"}}
		if _, _, err := reserveLinearWrite(context.Background(), tx, pid, w, time.Now()); err == nil {
			t.Fatal("a value key was stored:", key)
		}
	}
}

// The overview counts settled milestone plans, newest first; only an
// accepted plan with changes that is not yet acknowledged is "with_changes";
// a reserved, uncertain or not_sent plan is not counted as planned. The gate
// is shown.
func TestMilestonePlansOverview(t *testing.T) {
	h, now := msHarness(t)
	view := func() map[string]any { return h.overview("")["milestone_plans"].(map[string]any) }
	if v := view(); v["planned"] != 0 || v["with_changes"] != 0 || len(v["recent"].([]map[string]any)) != 0 || v["keep_days"] != linearWriteKeepDays {
		t.Fatal(v)
	}
	for _, op := range []string{"m-1", "m-2", "m-3", "m-4", "m-5"} {
		h.call("milestone_plan_reserve", "", msUpdateArgs(op, true, teamOne))
	}
	if v := view(); v["planned"] != 0 || len(v["recent"].([]map[string]any)) != 0 {
		t.Fatal("reserved rows counted:", v)
	}
	settle := func(op, outcome string, changed ...any) {
		*now = now.Add(time.Minute)
		h.call("milestone_plan_settle", "", map[string]any{"operation_id": op, "outcome": outcome, "changed": changed})
	}
	settle("m-1", "accepted")
	settle("m-2", "accepted", "sortOrder")
	settle("m-3", "accepted", "sortOrder")
	settle("m-4", "uncertain")
	v := view()
	recent := v["recent"].([]map[string]any)
	if v["planned"] != 3 || v["with_changes"] != 2 || len(recent) != 4 {
		t.Fatal(v)
	}
	var order []any
	for _, r := range recent {
		order = append(order, r["operation_id"])
	}
	if !reflect.DeepEqual(order, []any{"m-4", "m-3", "m-2", "m-1"}) {
		t.Fatal("not newest first:", order)
	}
	if recent[1]["state"] != "accepted_with_changes" || !reflect.DeepEqual(recent[1]["changed"], []string{"sortOrder"}) || recent[1]["milestone"] != msMilestone {
		t.Fatal(recent[1])
	}
	h.call("project_create_ack", "", map[string]any{"operation_id": "m-3"})
	if v = view(); v["planned"] != 3 || v["with_changes"] != 1 || v["recent"].([]map[string]any)[1]["acknowledged_at"] == nil {
		t.Fatal("acknowledged:", v)
	}
	// The gate is named in the owner's overview.
	var found bool
	for _, g := range h.overview("")["proven_gates"].([]map[string]any) {
		if g["name"] == "milestone_plan" {
			found = g["on"] == true
		}
	}
	if !found {
		t.Fatal("milestone_plan isn't shown as on")
	}
	h.s.SetMilestonePlanProven(false)
	for _, g := range h.overview("")["proven_gates"].([]map[string]any) {
		if g["name"] == "milestone_plan" && g["on"] != false {
			t.Fatal("still on")
		}
	}
}
