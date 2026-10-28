package bus

import (
	"context"
	"testing"
)

// On the hosted authority only the project owner sets the project kind,
// from a person's own device key; a Google sign-in, a session and a lesser
// role are refused. Every member reads it.
func TestPostgresProjectKindAccess(t *testing.T) {
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
	set := Request{Operation: "project_kind_set", Args: kindSet(0, "", "", "Engineering")}
	if _, err := as("member", set); !refusedWith(err, "owner") {
		t.Fatal("member:", err)
	}
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "project_kind_set", Workspace: f.workspace, Project: "p", Args: kindSet(0, "", "", "Engineering")}); !refusedWith(err, "device") {
		t.Fatal("google:", err)
	}
	if _, err := f.s.ExecuteAs(context.Background(), f.owner, Request{Operation: "project_kind_set", Workspace: f.workspace, Project: "p", Args: kindSet(0, "", "", "Engineering")}); !refusedWith(err, "device") {
		t.Fatal("no identity:", err)
	}
	if _, err := as("owner", set); err != nil {
		t.Fatal(err)
	}
	got, err := as("member", Request{Operation: "project_kind_get", Args: map[string]any{}})
	if err != nil || got.Data.(map[string]any)["revision"] != 1 {
		t.Fatal(got, err)
	}
}
