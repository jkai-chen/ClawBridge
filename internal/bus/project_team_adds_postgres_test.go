package bus

import (
	"encoding/json"
	"strings"
	"testing"
)

// On the hosted authority the project owner and a lead, without a session,
// reserve and settle a team add; a member and a reader are refused; one
// person can't settle another's add.
func TestPostgresProjectTeamAdds(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.s.SetProjectTeamAddProven(true)
	device := map[string]string{}
	people := map[string]string{}
	for _, role := range []string{"owner", "lead", "member", "reader"} {
		p := newID(role) + "@identity-test.example"
		code := f.invite(f.owner, p, role)["code"].(string)
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
			t.Fatal(r.Operation, err)
		}
		m, _ := out.Data.(map[string]any)
		return m
	}
	binding := taskboardArgsV2(0, addProject)
	binding["linear_team_ids"] = []string{teamOne, teamTwo}
	must("owner", Request{Operation: "taskboard_set_v2", Args: binding})
	for _, role := range []string{"member", "reader"} {
		if _, err := as(role, Request{Operation: "project_team_add_reserve", Args: teamAddArgs("t-"+role, teamTwo, teamOne)}); err == nil || !strings.Contains(err.Error(), "permission") && !strings.Contains(err.Error(), "the team's own lead") {
			t.Fatal(role, "reserved:", err)
		}
	}
	if got := must("owner", Request{Operation: "project_team_add_reserve", Args: teamAddArgs("t-owner", teamTwo, teamOne)}); got["by"] != "owner" {
		t.Fatal(got)
	}
	if got := must("lead", Request{Operation: "project_team_add_reserve", Args: teamAddArgs("t-lead", teamTwo, teamOne)}); got["by"] != "lead" {
		t.Fatal(got)
	}
	if _, err := as("lead", Request{Operation: "project_team_add_settle", Args: map[string]any{"operation_id": "t-owner", "outcome": "accepted"}}); err == nil || !strings.Contains(err.Error(), "no reservation") {
		t.Fatal("the lead settled the owner's add:", err)
	}
	if got := must("lead", Request{Operation: "project_team_add_settle", Args: map[string]any{"operation_id": "t-lead", "outcome": "accepted", "changed": []any{"members"}}}); got["state"] != "accepted_with_changes" {
		t.Fatal(got)
	}
	if got := must("member", Request{Operation: "project_team_add_list"}); got["added"] != 1 || got["with_changes"] != 1 {
		t.Fatal(got)
	}
	// R3: a member the owner linked to a lead row of the added team reserves
	// it as team_lead; the reader, though linked, is still refused.
	const leadUser = "d1000000-0000-4000-8000-0000000000d1"
	for _, who := range []string{"member", "reader"} {
		m := TeamRoles{Teams: []TeamRoleTeam{{TeamID: teamTwo, Leads: []string{leadUser}}}, People: []TeamRolePerson{{LinearUserID: leadUser, Role: "lead", Teams: []string{teamTwo}, Principal: people[who]}}}
		raw, _ := json.Marshal(m)
		if _, err := f.s.db.Exec(`DELETE FROM team_role_mapping`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.db.Exec(`INSERT INTO team_role_mapping(project_id,revision,mapping,updated_by,updated_at) SELECT id,1,$1,$2,$3 FROM projects WHERE slug='p'`, string(raw), f.owner, "2026-10-29T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
		got, err := as(who, Request{Operation: "project_team_add_reserve", Args: teamAddArgs("t-lead-"+who, teamTwo, teamOne)})
		data, _ := got.Data.(map[string]any)
		if who == "member" && (err != nil || data["by"] != "team_lead") || who == "reader" && err == nil {
			t.Fatal(who, data, err)
		}
	}
}
