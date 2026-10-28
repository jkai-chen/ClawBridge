package bus

import "testing"

// On the hosted authority the project lead setting and its records are
// the owner's, from their own device: a member reads but never sets, and a
// Google sign-in is refused.
func TestPostgresProjectLeadAccess(t *testing.T) {
	f := newPGIdentityFixture(t)
	device := map[string]string{}
	people := map[string]string{}
	for _, role := range []string{"owner", "member"} {
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
	set := map[string]any{"expected_revision": 0, "mode": "on"}
	record := map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted"}
	for op, a := range map[string]map[string]any{"project_lead_set": set, "project_lead_record": record} {
		if _, err := as("member", Request{Operation: op, Args: a}); !refusedWith(err, "owner") {
			t.Fatal("member:", op, err)
		}
		if _, err := f.as(f.owner, "sub-owner", Request{Operation: op, Workspace: f.workspace, Project: "p", Args: a}); !refusedWith(err, "device") {
			t.Fatal("google:", op, err)
		}
	}
	if _, err := as("owner", Request{Operation: "project_lead_set", Args: set}); err != nil {
		t.Fatal(err)
	}
	got, err := as("member", Request{Operation: "project_lead_get", Args: map[string]any{}})
	if err != nil || got.Data.(map[string]any)["mode"] != "on" {
		t.Fatal(got, err)
	}
	// The notice reaches the member the owner linked as the lead, and only
	// their sessions: never the owner's, who recorded the write.
	for _, role := range []string{"owner", "member"} {
		if _, err := as(role, Request{Operation: "register_session", Session: role + "-s", Args: map[string]any{"instance": role + "-i", "role": "worker"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := as("owner", Request{Operation: "taskboard_set_v2", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": []string{cpOne}}}); err != nil {
		t.Fatal(err)
	}
	lead := person(roleAda, "lead", sfTeam)
	lead["principal"] = people["member"]
	if _, err := as("owner", Request{Operation: "team_roles_set", Args: rolesSet(0, true, []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda}}}, []map[string]any{lead})}); err != nil {
		t.Fatal(err)
	}
	got, err = as("owner", Request{Operation: "project_lead_record", Args: map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted", "lead": roleAda, "operation_id": "op-1"}})
	if err != nil || got.Data.(map[string]any)["notified"] != 1 {
		t.Fatal(got, err)
	}
	var to string
	if err := f.s.db.QueryRow(`SELECT s.owner_principal FROM deliveries d JOIN messages m ON m.id=d.message_id JOIN sessions s ON s.id=d.recipient_id JOIN projects p ON p.id=m.project_id WHERE m.kind='project-lead' AND p.workspace_id IN (SELECT id FROM workspaces WHERE id=$1 OR name=$1)`, f.workspace).Scan(&to); err != nil || to != people["member"] {
		t.Fatal(to, err)
	}
	// A member removed from the project is never told, even while their
	// sessions are still active (T3: the link is re-checked when used).
	if _, err := f.s.db.Exec(`DELETE FROM project_roles WHERE principal=$1`, people["member"]); err != nil {
		t.Fatal(err)
	}
	got, err = as("owner", Request{Operation: "project_lead_record", Args: map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted", "lead": roleAda, "operation_id": "op-2"}})
	if err != nil || got.Data.(map[string]any)["notified"] != 0 {
		t.Fatal("a removed member was told", got, err)
	}
}
