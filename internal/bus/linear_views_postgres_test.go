package bus

import (
	"context"
	"strings"
	"testing"
)

// On the hosted authority only the project owner records a view, from a
// person's own device key: a Google sign-in, a session, a CI device and
// every lesser role are refused. Every member reads the stored views.
func TestPostgresLinearViewRecordAccess(t *testing.T) {
	f := newPGIdentityFixture(t)
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
	if _, err := as("owner", Request{Operation: "taskboard_set_v2", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": []string{cpOne}}}); err != nil {
		t.Fatal(err)
	}
	record := func() map[string]any {
		return lvRecord("create", lvView, "Urgent bugs", lvFilter, "2026-10-27T21:00:00.000Z", "", 1)
	}
	refused := func(what, want string, run func(a map[string]any) (Result, error)) {
		t.Helper()
		if _, err := run(record()); !refusedWith(err, want) {
			t.Fatal(what, err)
		}
	}
	for _, role := range []string{"lead", "member", "reader"} {
		refused(role, "owner", func(a map[string]any) (Result, error) {
			return as(role, Request{Operation: "linear_view_record", Args: a})
		})
	}
	refused("google", "device", func(a map[string]any) (Result, error) {
		return f.as(f.owner, "sub-owner", Request{Operation: "linear_view_record", Workspace: f.workspace, Project: "p", Args: a})
	})
	refused("no identity", "device", func(a map[string]any) (Result, error) {
		return f.s.ExecuteAs(context.Background(), f.owner, Request{Operation: "linear_view_record", Workspace: f.workspace, Project: "p", Args: a})
	})
	if _, err := as("owner", Request{Operation: "register_session", Session: "mine", Args: map[string]any{"instance": "mine-i", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	refused("session", "session", func(a map[string]any) (Result, error) {
		a["instance"] = "mine-i"
		return as("owner", Request{Operation: "linear_view_record", Session: "mine", Args: a})
	})
	key := newKey(t)
	if _, err := f.auth(f.owner, "device_ci_create", map[string]any{"name": "deploy", "grants": map[string]any{"p": "reader"}, "expires_days": 30, "public_key": key}); err != nil {
		t.Fatal(err)
	}
	ci := "ci:deploy@" + strings.ToLower(f.workspace)
	f.exec(`UPDATE project_roles SET role='owner' WHERE principal=?`, ci)
	refused("ci", "device", func(a map[string]any) (Result, error) {
		return f.asDevice(ci, keyID(t, key), Request{Operation: "linear_view_record", Workspace: f.workspace, Project: "p", Args: a})
	})
	if f.audited("view_created", lvView) != 0 {
		t.Fatal("a refused record was audited")
	}

	got, err := as("owner", Request{Operation: "linear_view_record", Args: record()})
	if err != nil {
		t.Fatal(err)
	}
	if view := got.Data.(map[string]any)["view"].(map[string]any); view["scope"] != "team" || view["adopted"] != false || view["binding_revision"] != 1 {
		t.Fatal(view)
	}
	if f.audited("view_created", lvView) != 1 {
		t.Fatal("audit rows")
	}
	for _, role := range []string{"lead", "member", "reader"} {
		one, err := as(role, Request{Operation: "linear_view_get", Args: map[string]any{"view_id": lvView}})
		if err != nil || one.Data.(map[string]any)["filter"] != lvFilter || one.Data.(map[string]any)["team_bound"] != true {
			t.Fatal(role, one, err)
		}
		list, err := as(role, Request{Operation: "linear_views_list", Args: map[string]any{}})
		if err != nil || len(list.Data.(map[string]any)["views"].([]any)) != 1 {
			t.Fatal(role, list, err)
		}
	}
}
