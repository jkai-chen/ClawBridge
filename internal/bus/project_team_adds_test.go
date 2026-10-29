package bus

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	addProject   = "e6000000-0000-4000-8000-000000000006"
	otherProject = "e7000000-0000-4000-8000-000000000007"
	unboundTeam  = "e8000000-0000-4000-8000-000000000008"
)

func teamAddArgs(op, team string, current ...string) map[string]any {
	return map[string]any{"operation_id": op, "linear_project": addProject, "team_id": team, "project_teams": current, "plan_sha256": strings.Repeat("a", 64)}
}

// teamAddsHarness is the creates harness (lanes "one" writes teamOne,
// "both" writes both; c is the coordinator) with a project set holding
// addProject, on a server that has proven team adds.
func teamAddsHarness(t *testing.T) (*harness, *time.Time) {
	t.Helper()
	h, now := createsHarness(t)
	a := taskboardArgsLanes(1)
	a["mode"], a["linear_project_ids"] = "project_set", []string{addProject}
	h.call("taskboard_set_v2", "", a)
	h.s.SetProjectTeamAddProven(true)
	return h, now
}

// Nothing is reserved until the operator proves team adds; then the
// owner, without a session, reserves adding a bound write team to a
// project of the set, a repeat returns it, and a reused operation ID with
// another plan is a conflict.
func TestProjectTeamAddReserveOwner(t *testing.T) {
	h, _ := teamAddsHarness(t)
	h.s.SetProjectTeamAddProven(false)
	if e := h.errorOf("project_team_add_reserve", "", teamAddArgs("t-1", teamTwo, teamOne)); !strings.Contains(e, "project_team_add_unproven") {
		t.Fatal("an unproven server reserved:", e)
	}
	h.s.SetProjectTeamAddProven(true)
	for want, args := range map[string]map[string]any{
		"already in the project": teamAddArgs("t-1", teamTwo, teamOne, teamTwo),
		"isn't a write team":     teamAddArgs("t-1", unboundTeam, teamOne),
		"isn't in this binding": func() map[string]any {
			a := teamAddArgs("t-1", teamTwo, teamOne)
			a["linear_project"] = otherProject
			return a
		}(),
		"list of UUIDs": func() map[string]any {
			a := teamAddArgs("t-1", teamTwo)
			a["project_teams"] = []any{"Platform"}
			return a
		}(),
		"team_id":                 func() map[string]any { a := teamAddArgs("t-1", teamTwo, teamOne); a["team_id"] = "Platform"; return a }(),
		"lower-case SHA-256":      func() map[string]any { a := teamAddArgs("t-1", teamTwo, teamOne); a["plan_sha256"] = "x"; return a }(),
		"operation_id must be 1–": teamAddArgs("", teamTwo, teamOne),
	} {
		if e := h.errorOf("project_team_add_reserve", "", args); !strings.Contains(e, want) {
			t.Fatal(want, e)
		}
	}
	got := h.call("project_team_add_reserve", "", teamAddArgs("t-1", teamTwo, teamOne)).(map[string]any)
	if got["state"] != "reserved" || got["by"] != "owner" || got["lane"] != "" || got["linear_project"] != addProject || got["team_id"] != teamTwo || got["replay"] != false {
		t.Fatal(got)
	}
	if again := h.call("project_team_add_reserve", "", teamAddArgs("t-1", teamTwo, teamOne)).(map[string]any); again["replay"] != true {
		t.Fatal("a repeat:", again)
	}
	other := teamAddArgs("t-1", teamTwo, teamOne)
	other["plan_sha256"] = strings.Repeat("b", 64)
	if e := h.errorOf("project_team_add_reserve", "", other); !strings.Contains(e, "different Linear write") {
		t.Fatal("op reused:", e)
	}
	// The project's teams are part of the plan.
	if e := h.errorOf("project_team_add_reserve", "", teamAddArgs("t-1", teamTwo)); !strings.Contains(e, "different Linear write") {
		t.Fatal("op reused with other teams:", e)
	}
	// History by IDs only.
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_team_add' ORDER BY sequence LIMIT 1`).Scan(&details); e != nil || !strings.Contains(details, addProject) || strings.Contains(details, h.principal()) {
		t.Fatal("history details:", details, e)
	}
}

// A team board has no projects: refused.
func TestProjectTeamAddRefusesATeamBoard(t *testing.T) {
	h, _ := createsHarness(t)
	h.s.SetProjectTeamAddProven(true)
	if e := h.errorOf("project_team_add_reserve", "", teamAddArgs("t-1", teamTwo, teamOne)); !strings.Contains(e, "a team board has no projects") {
		t.Fatal(e)
	}
}

// A session other than the coordinator, a coordinator with no lane, one
// whose lane misses the team, and one whose lane holds none of the
// project's teams (R2) are refused; inside its lane it reserves, by
// coordinator with the lane named. A binding without lanes takes no
// coordinator add.
func TestProjectTeamAddCoordinatorLane(t *testing.T) {
	h, _ := teamAddsHarness(t)
	if e := h.errorOf("project_team_add_reserve", "a", teamAddArgs("t-1", teamOne, teamTwo)); !strings.Contains(e, "designated coordinator") {
		t.Fatal("a non-coordinator session reserved:", e)
	}
	if e := h.errorOf("project_team_add_reserve", "c", teamAddArgs("t-1", teamOne, teamTwo)); !strings.Contains(e, "outside_lane") || !strings.Contains(e, "no lane") {
		t.Fatal("an unpinned coordinator reserved:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "one"})
	if e := h.errorOf("project_team_add_reserve", "c", teamAddArgs("t-1", teamTwo, teamOne)); !strings.Contains(e, "outside_lane: team "+teamTwo) {
		t.Fatal("a team outside the lane:", e)
	}
	// R2: lane "one" adding its own team to a project only lane "both"'s
	// teamTwo holds pulls that project into lane "one".
	if e := h.errorOf("project_team_add_reserve", "c", teamAddArgs("t-1", teamOne, teamTwo)); !strings.Contains(e, "outside_lane: the project holds no write team of lane one") {
		t.Fatal("another lane's project:", e)
	}
	if e := h.errorOf("project_team_add_reserve", "c", teamAddArgs("t-1", teamOne)); !strings.Contains(e, "holds no write team") {
		t.Fatal("a project with no teams read:", e)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "c", "lane": "both"})
	got := h.call("project_team_add_reserve", "c", teamAddArgs("t-1", teamOne, teamTwo)).(map[string]any)
	if got["by"] != "coordinator" || got["lane"] != "both" {
		t.Fatal(got)
	}
	// The owner can't take the coordinator's operation ID.
	if e := h.errorOf("project_team_add_reserve", "", teamAddArgs("t-1", teamOne, teamTwo)); !strings.Contains(e, "different Linear write") {
		t.Fatal("the owner took the coordinator's operation ID:", e)
	}
	// Without lanes, a coordinator adds no team.
	a := taskboardArgsV2(2, addProject)
	a["linear_team_ids"] = []string{teamOne, teamTwo}
	h.call("taskboard_set_v2", "", a)
	if e := h.errorOf("project_team_add_reserve", "c", teamAddArgs("t-2", teamOne, teamTwo)); !strings.Contains(e, "no lanes") {
		t.Fatal("a no-lanes coordinator reserved:", e)
	}
}

// Settle closes the caller's own reservation: accepted, accepted with the
// names of other changed fields (shown as accepted_with_changes), uncertain
// or not_sent; a repeat is a replay, a different outcome a conflict, and
// changed names only listed fields. The list and the overview count adds
// and those with changes; history has field names only.
func TestProjectTeamAddSettle(t *testing.T) {
	h, _ := teamAddsHarness(t)
	for _, op := range []string{"t-1", "t-2", "t-3", "t-4"} {
		h.call("project_team_add_reserve", "", teamAddArgs(op, teamTwo, teamOne))
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	if e := h.errorOf("project_team_add_settle", "c", map[string]any{"operation_id": "t-1", "outcome": "accepted"}); !strings.Contains(e, "no reservation") {
		t.Fatal("a session settled the owner's add:", e)
	}
	settle := func(op, outcome string, changed ...any) map[string]any {
		return map[string]any{"operation_id": op, "outcome": outcome, "changed": changed}
	}
	for want, args := range map[string]map[string]any{
		"outcome must be":    settle("t-1", "created"),
		"changed names only": settle("t-1", "accepted", "description"),
		"only an accepted":   settle("t-1", "uncertain", "lead"),
		"array of strings":   {"operation_id": "t-1", "outcome": "accepted", "changed": "lead"},
	} {
		if e := h.errorOf("project_team_add_settle", "", args); !strings.Contains(e, want) {
			t.Fatal(want, e)
		}
	}
	if got := h.call("project_team_add_settle", "", settle("t-1", "accepted")).(map[string]any); got["state"] != "accepted" || len(got["changed"].([]string)) != 0 {
		t.Fatal(got)
	}
	got := h.call("project_team_add_settle", "", settle("t-2", "accepted", "members", "lead", "members")).(map[string]any)
	if got["state"] != "accepted_with_changes" || !reflect.DeepEqual(got["changed"], []string{"lead", "members"}) || got["replay"] != false {
		t.Fatal(got)
	}
	if again := h.call("project_team_add_settle", "", settle("t-2", "accepted", "lead", "members")).(map[string]any); again["replay"] != true {
		t.Fatal("a repeat:", again)
	}
	for name, args := range map[string]map[string]any{"outcome": settle("t-2", "uncertain"), "changed": settle("t-2", "accepted", "lead")} {
		if e := h.errorOf("project_team_add_settle", "", args); !strings.Contains(e, "conflict") && !strings.Contains(e, "already settled") {
			t.Fatal(name, e)
		}
	}
	h.call("project_team_add_settle", "", settle("t-3", "uncertain"))
	h.call("project_team_add_settle", "", settle("t-4", "not_sent"))
	list := h.call("project_team_add_list", "", nil).(map[string]any)
	if list["added"] != 2 || list["with_changes"] != 1 || len(list["recent"].([]map[string]any)) != 4 {
		t.Fatal(list)
	}
	if adds := h.overview("")["project_team_adds"].(map[string]any); adds["added"] != 2 || adds["with_changes"] != 1 {
		t.Fatal(adds)
	}
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_team_add' AND action='accepted' AND entity_key='t-2'`).Scan(&details); e != nil || !strings.Contains(details, `"changed":["lead","members"]`) {
		t.Fatal(details, e)
	}
	// The owner acknowledges an add with changes (linear writes --ack): it
	// stops counting, still listed; a clean accepted add has nothing to
	// acknowledge.
	if e := h.errorOf("project_create_ack", "", map[string]any{"operation_id": "t-1"}); !strings.Contains(e, "no unacknowledged") {
		t.Fatal("a clean add acknowledged:", e)
	}
	h.call("project_create_ack", "", map[string]any{"operation_id": "t-2"})
	if list := h.call("project_team_add_list", "", nil).(map[string]any); list["added"] != 2 || list["with_changes"] != 0 || list["recent"].([]map[string]any)[0]["acknowledged_at"] == nil && list["recent"].([]map[string]any)[1]["acknowledged_at"] == nil {
		t.Fatal("acknowledged:", list)
	}
	if e := h.errorOf("project_create_ack", "", map[string]any{"operation_id": "t-2"}); !strings.Contains(e, "no unacknowledged") {
		t.Fatal("acknowledged twice:", e)
	}
	// A not_sent add reopens on the same confirm.
	if again := h.call("project_team_add_reserve", "", teamAddArgs("t-4", teamTwo, teamOne)).(map[string]any); again["state"] != "reserved" || again["replay"] != false {
		t.Fatal("not_sent didn't reopen:", again)
	}
}

// A reservation left unsettled is in the one stale count, as a
// project_team_add write naming the project.
func TestProjectTeamAddStale(t *testing.T) {
	h, now := teamAddsHarness(t)
	h.call("project_team_add_reserve", "", teamAddArgs("t-1", teamTwo, teamOne))
	*now = now.Add(11 * time.Minute)
	u := h.overview("")["project_creates"].(map[string]any)["unconfirmed"].([]map[string]any)
	if len(u) != 1 || u[0]["write"] != projectTeamAddKind || u[0]["subject"] != addProject || u[0]["team_id"] != teamTwo {
		t.Fatal(u)
	}
}

// F2: an accepted add names the fields a read left out (lead, members),
// stored with the add and in history; only those two, and only on an
// accepted add.
func TestProjectTeamAddUnverified(t *testing.T) {
	h, _ := teamAddsHarness(t)
	h.call("project_team_add_reserve", "", teamAddArgs("t-1", teamTwo, teamOne))
	h.call("project_team_add_reserve", "", teamAddArgs("t-2", teamTwo, teamOne))
	for want, args := range map[string]map[string]any{
		"unverified names only": {"operation_id": "t-1", "outcome": "accepted", "unverified": []any{"status"}},
		"only an accepted":      {"operation_id": "t-1", "outcome": "uncertain", "unverified": []any{"members"}},
	} {
		if e := h.errorOf("project_team_add_settle", "", args); !strings.Contains(e, want) {
			t.Fatal(want, e)
		}
	}
	got := h.call("project_team_add_settle", "", map[string]any{"operation_id": "t-1", "outcome": "accepted", "unverified": []any{"members", "lead"}}).(map[string]any)
	if got["state"] != "accepted" || !reflect.DeepEqual(got["unverified"], []string{"lead", "members"}) {
		t.Fatal(got)
	}
	if e := h.errorOf("project_team_add_settle", "", map[string]any{"operation_id": "t-1", "outcome": "accepted"}); !strings.Contains(e, "already settled") {
		t.Fatal("a settle with other unverified fields:", e)
	}
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_team_add' AND action='accepted' AND entity_key='t-1'`).Scan(&details); e != nil || !strings.Contains(details, `"unverified":["lead","members"]`) {
		t.Fatal(details, e)
	}
}

// team_lead is a recorded role (FR10 R3); a role nobody writes still isn't.
func TestLinearWriteTeamLeadRole(t *testing.T) {
	h, _ := createsHarness(t)
	tx, pid := ledgerTx(t, h)
	w := linearWrite{Kind: projectTeamAddKind, Operation: "op-1", Principal: "p", ByRole: "team_lead", Team: teamTwo, Subject: addProject, Plan: "plan"}
	if row, _, err := reserveLinearWrite(context.Background(), tx, pid, w, time.Now()); err != nil || str(row, "by_role") != "team_lead" {
		t.Fatal(row, err)
	}
	w.Operation, w.ByRole = "op-2", "team_engineer"
	if _, _, err := reserveLinearWrite(context.Background(), tx, pid, w, time.Now()); err == nil {
		t.Fatal("an unknown role was reserved")
	}
}

// R3: without a session, a member who leads the added team in the team
// roles (a lead row the owner linked to them) reserves it as team_lead;
// a team they don't lead, or a lead row not linked to them, is refused.
// The role can only be tested here as the harness's own principal, which
// is the owner; so the test reads the adder directly with role member.
func TestProjectTeamAddTeamRolesLead(t *testing.T) {
	h, _ := teamAddsHarness(t)
	me := h.principal()
	const leadUser = "d1000000-0000-4000-8000-0000000000d1"
	mapping := func(linked string, team string) {
		h.t.Helper()
		m := TeamRoles{Teams: []TeamRoleTeam{{TeamID: team, Leads: []string{leadUser}}}, People: []TeamRolePerson{{LinearUserID: leadUser, Role: "lead", Teams: []string{team}, Principal: linked}}}
		raw, _ := json.Marshal(m)
		if _, e := h.s.db.Exec(`DELETE FROM team_role_mapping`); e != nil {
			t.Fatal(e)
		}
		if _, e := h.s.db.Exec(`INSERT INTO team_role_mapping(project_id,revision,mapping,updated_by,updated_at) VALUES(?,1,?,?,?)`, h.projectID(), string(raw), me, "2026-10-29T00:00:00Z"); e != nil {
			t.Fatal(e)
		}
	}
	adder := func() (string, error) {
		tx, pid := ledgerTx(t, h)
		defer tx.Rollback()
		b, _, err := storedBinding(context.Background(), tx, pid)
		if err != nil {
			t.Fatal(err)
		}
		by, _, err := h.s.projectTeamAdder(context.Background(), tx, pid, "member", nil, b, teamTwo, []string{teamOne})
		return by, err
	}
	if _, err := adder(); err == nil || !strings.Contains(err.Error(), "team's own lead") {
		t.Fatal("a member with no roles:", err)
	}
	mapping(me, teamTwo)
	if by, err := adder(); err != nil || by != "team_lead" {
		t.Fatal(by, err)
	}
	mapping(me, teamOne)
	if _, err := adder(); err == nil {
		t.Fatal("the lead of another team added this one")
	}
	mapping("", teamTwo)
	if _, err := adder(); err == nil {
		t.Fatal("an unlinked lead row granted the add")
	}
}
