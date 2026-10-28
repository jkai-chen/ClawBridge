package bus

import (
	"context"
	"strings"
	"testing"
)

// On the hosted authority only the project owner sets team roles, from a
// person's own device key; a Google sign-in, no identity and a member are
// refused. Every member reads them. A principal must be a project member:
// a member's account links, a stranger's doesn't.
func TestPostgresTeamRolesAccess(t *testing.T) {
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
	if _, err := as("owner", Request{Operation: "taskboard_set_v2", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": []string{cpOne}}}); err != nil {
		t.Fatal(err)
	}
	set := func(principal string) map[string]any {
		p := person(roleAda, "reviewer", sfTeam)
		p["principal"] = principal
		return rolesSet(0, true, nil, []map[string]any{p})
	}
	if _, err := as("member", Request{Operation: "team_roles_set", Args: set(people["member"])}); !refusedWith(err, "owner") {
		t.Fatal("member:", err)
	}
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "team_roles_set", Workspace: f.workspace, Project: "p", Args: set(people["member"])}); !refusedWith(err, "device") {
		t.Fatal("google:", err)
	}
	if _, err := f.s.ExecuteAs(context.Background(), f.owner, Request{Operation: "team_roles_set", Workspace: f.workspace, Project: "p", Args: set(people["member"])}); !refusedWith(err, "device") {
		t.Fatal("no identity:", err)
	}
	if _, err := as("owner", Request{Operation: "team_roles_set", Args: set("stranger@identity-test.example")}); err == nil || !strings.Contains(err.Error(), "member of this project") {
		t.Fatal("stranger:", err)
	}
	if _, err := as("owner", Request{Operation: "team_roles_set", Args: set(people["member"])}); err != nil {
		t.Fatal(err)
	}
	got, err := as("member", Request{Operation: "team_roles_get", Args: map[string]any{}})
	if err != nil || got.Data.(map[string]any)["revision"] != 1 {
		t.Fatal(got, err)
	}
}
