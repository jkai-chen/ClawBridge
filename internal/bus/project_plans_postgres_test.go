package bus

import (
	"encoding/json"
	"strings"
	"testing"
)

// On the hosted authority: the owner reserves as owner and a project lead
// as project_lead; a member the owner linked to a lead row reserves as
// team_lead, in their terminal, for a project holding that team, and as
// team_lead_session from their own session inside a lane holding it; a
// lead of a team the project doesn't hold, a reader and another member's
// session are refused. A team_lead_session row is acknowledged by its
// owning lead in their own terminal, never by the session or another
// lead; the owner acknowledges any.
func TestPostgresProjectPlans(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.s.SetProjectPlanProven(true)
	device := map[string]string{}
	people := map[string]string{}
	for _, role := range []string{"owner", "lead", "member", "other", "reader"} {
		p := newID(role) + "@identity-test.example"
		grant := role
		if role == "other" {
			grant = "member"
		}
		code := f.invite(f.owner, p, grant)["code"].(string)
		key := newKey(t)
		if _, err := f.join(p, code, key); err != nil {
			t.Fatal(err)
		}
		people[role], device[role] = p, keyID(t, key)
	}
	as := func(role string, r Request) (Result, error) {
		r.Workspace, r.Project = f.workspace, "p"
		return f.asDevice(people[role], device[role], r)
	}
	must := func(role string, r Request) map[string]any {
		t.Helper()
		out, err := as(role, r)
		if err != nil {
			t.Fatal(role, r.Operation, err)
		}
		m, _ := out.Data.(map[string]any)
		return m
	}
	binding := taskboardArgsLanes(0)
	binding["mode"], binding["linear_project_ids"] = "project_set", []string{addProject}
	must("owner", Request{Operation: "taskboard_set_v2", Args: binding})
	// member leads teamTwo, other leads teamOne.
	const memberUser, otherUser = "d3000000-0000-4000-8000-0000000000d3", "d4000000-0000-4000-8000-0000000000d4"
	m := TeamRoles{
		Teams:  []TeamRoleTeam{{TeamID: teamOne, Leads: []string{otherUser}}, {TeamID: teamTwo, Leads: []string{memberUser}}},
		People: []TeamRolePerson{{LinearUserID: memberUser, Role: "lead", Teams: []string{teamTwo}, Principal: people["member"]}, {LinearUserID: otherUser, Role: "lead", Teams: []string{teamOne}, Principal: people["other"]}},
	}
	raw, _ := json.Marshal(m)
	if _, err := f.s.db.Exec(`INSERT INTO team_role_mapping(project_id,revision,mapping,updated_by,updated_at) SELECT id,1,$1,$2,$3 FROM projects WHERE slug='p' ON CONFLICT(project_id) DO UPDATE SET mapping=EXCLUDED.mapping,revision=team_role_mapping.revision+1`, string(raw), f.owner, "2026-10-29T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	reserve := func(role, session, op string, teams ...string) (map[string]any, error) {
		a := planArgs(op, true, teams...)
		if session != "" {
			a["instance"] = session + "-i"
		}
		out, err := as(role, Request{Operation: "project_plan_reserve", Session: session, Args: a})
		data, _ := out.Data.(map[string]any)
		return data, err
	}
	for role, want := range map[string]string{"owner": "owner", "lead": "project_lead", "member": "team_lead"} {
		got, err := reserve(role, "", "p-"+role, teamOne, teamTwo)
		if err != nil || got["by"] != want {
			t.Fatal(role, got, err)
		}
	}
	if _, err := reserve("other", "", "p-other", teamTwo); err == nil || !strings.Contains(err.Error(), "ask the project owner or a lead") {
		t.Fatal("the lead of a team the project doesn't hold reserved:", err)
	}
	if _, err := reserve("reader", "", "p-reader", teamOne); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatal("a reader reserved:", err)
	}
	// member's own session, pinned to lane "both" (it holds teamTwo).
	must("member", Request{Operation: "register_session", Session: "m", Args: map[string]any{"instance": "m-i", "role": "worker"}})
	must("owner", Request{Operation: "lane_pin_set", Args: map[string]any{"expected_revision": 0, "kind": "session", "key": "m", "lane": "both", "confirm": true}})
	got, err := reserve("member", "m", "p-session", teamTwo)
	if err != nil || got["by"] != "team_lead_session" || got["lane"] != "both" || got["team_id"] != teamTwo {
		t.Fatal(got, err)
	}
	var principal string
	if err := f.s.db.QueryRow(`SELECT principal FROM linear_write_reservations WHERE operation_id='p-session'`).Scan(&principal); err != nil || principal != people["member"] {
		t.Fatal(principal, err)
	}
	// Another member can't use member's session.
	if _, err := reserve("other", "m", "p-steal", teamTwo); err == nil || !strings.Contains(err.Error(), "another principal") {
		t.Fatal("another member's session reserved:", err)
	}
	if out := must("member", Request{Operation: "project_plan_settle", Session: "m", Args: map[string]any{"instance": "m-i", "operation_id": "p-session", "outcome": "uncertain"}}); out["state"] != "uncertain" {
		t.Fatal(out)
	}
	ack := func(role, session, op string) error {
		a := map[string]any{"operation_id": op}
		if session != "" {
			a["instance"] = session + "-i"
		}
		_, err := as(role, Request{Operation: "project_create_ack", Session: session, Args: a})
		return err
	}
	if err := ack("member", "m", "p-session"); err == nil || !strings.Contains(err.Error(), "a session can't acknowledge") {
		t.Fatal("the session acknowledged:", err)
	}
	if err := ack("other", "", "p-session"); err == nil || !strings.Contains(err.Error(), "forbidden") && !strings.Contains(err.Error(), "permission") {
		t.Fatal("another lead acknowledged:", err)
	}
	if err := ack("lead", "", "p-session"); err == nil {
		t.Fatal("a project lead acknowledged a team lead's plan")
	}
	if err := ack("member", "", "p-session"); err != nil {
		t.Fatal("the owning lead couldn't acknowledge:", err)
	}
	// The owner acknowledges any; the project lead their own.
	must("lead", Request{Operation: "project_plan_settle", Args: map[string]any{"operation_id": "p-lead", "outcome": "accepted", "changed": []any{"lead"}}})
	must("member", Request{Operation: "project_plan_settle", Args: map[string]any{"operation_id": "p-member", "outcome": "accepted", "changed": []any{"lead"}}})
	if err := ack("owner", "", "p-member"); err != nil {
		t.Fatal("the owner couldn't acknowledge:", err)
	}
	if err := ack("lead", "", "p-lead"); err != nil {
		t.Fatal("the project lead couldn't acknowledge their own:", err)
	}
}
