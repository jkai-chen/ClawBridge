package bus

import "testing"

// A project owner who isn't the workspace owner grants up to lead through
// project_role_set, as an invite does (D1); only the workspace owner grants
// owner.
func TestPostgresProjectRoleSetCannotGrantOwnerBelowTheWorkspaceOwner(t *testing.T) {
	f := newPGIdentityFixture(t)
	device := map[string]string{}
	people := map[string]string{}
	for _, role := range []string{"deputy", "colleague"} {
		p := newID(role) + "@identity-test.example"
		grant := "member"
		if role == "deputy" {
			grant = "owner"
		}
		code := f.invite(f.owner, p, grant)["code"].(string)
		key := newKey(t)
		if _, err := f.join(p, code, key); err != nil {
			t.Fatal(err)
		}
		people[role], device[role] = p, keyID(t, key)
	}
	as := func(role string, to, level string) error {
		_, err := f.asDevice(people[role], device[role], Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": people[to], "role": level}})
		return err
	}
	if err := as("deputy", "colleague", "owner"); !refusedWith(err, "only the workspace owner grants owner") {
		t.Fatal("a project owner granted owner:", err)
	}
	if err := as("deputy", "colleague", "lead"); err != nil {
		t.Fatal("a project owner couldn't grant lead:", err)
	}
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": people["colleague"], "role": "owner"}}); err != nil {
		t.Fatal("the workspace owner couldn't grant owner:", err)
	}
}
