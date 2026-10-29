package bus

import (
	"strings"
	"testing"
)

// On the hosted authority the generated document's record is the owner's
// alone, from their own device: a member and a Google sign-in are refused.
func TestPostgresTeamRolesDocAccess(t *testing.T) {
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
	put := map[string]any{"parent_kind": "team", "parent_id": sfTeam, "source_path": teamRolesDocSource, "state": "pending", "publish_token": "aaaaaaaaaaaa", "expect_token": "", "mapping_revision": 1, "source_sha256": strings.Repeat("5", 64)}
	for _, op := range []string{"team_roles_doc_get", "team_roles_doc_put"} {
		a := map[string]any{}
		if op == "team_roles_doc_put" {
			a = put
		}
		if _, err := as("member", Request{Operation: op, Args: a}); !refusedWith(err, "owner") {
			t.Fatal("member:", op, err)
		}
		if _, err := f.as(f.owner, "sub-owner", Request{Operation: op, Workspace: f.workspace, Project: "p", Args: a}); !refusedWith(err, "device") {
			t.Fatal("google:", op, err)
		}
	}
	if _, err := as("owner", Request{Operation: "team_roles_doc_put", Args: put}); err != nil {
		t.Fatal(err)
	}
	got, err := as("owner", Request{Operation: "team_roles_doc_get", Args: map[string]any{}})
	if err != nil || got.Data.(map[string]any)["published"].(map[string]any)["state"] != "pending" {
		t.Fatal(got, err)
	}
}
