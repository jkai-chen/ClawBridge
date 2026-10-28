package bus

import (
	"context"
	"database/sql"
	"net/url"
	"os"
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
	// Forgetting a deleted view is the owner's, from a device, too.
	forget := map[string]any{"view_id": lvView, "expected_filter_as_of": "2026-10-27T21:00:00.000Z"}
	if _, err = as("lead", Request{Operation: "linear_view_forget", Args: forget}); !refusedWith(err, "owner") {
		t.Fatal("lead forgot:", err)
	}
	if _, err = f.as(f.owner, "sub-owner", Request{Operation: "linear_view_forget", Workspace: f.workspace, Project: "p", Args: forget}); !refusedWith(err, "device") {
		t.Fatal("google forgot:", err)
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

// The owner's device forgets a deleted view once; history says so.
func TestPostgresLinearViewForget(t *testing.T) {
	f := newPGIdentityFixture(t)
	p := newID("owner") + "@identity-test.example"
	key := newKey(t)
	if _, err := f.join(p, f.invite(f.owner, p, "owner")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	as := func(r Request) (Result, error) {
		r.Workspace, r.Project = f.workspace, "p"
		return f.asDevice(p, keyID(t, key), r)
	}
	if _, err := as(Request{Operation: "taskboard_set_v2", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": []string{cpOne}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := as(Request{Operation: "linear_view_record", Args: lvRecord("create", lvView, "Urgent bugs", lvFilter, "2026-10-27T21:00:00.000Z", "", 1)}); err != nil {
		t.Fatal(err)
	}
	got, err := as(Request{Operation: "linear_view_forget", Args: map[string]any{"view_id": lvView, "expected_filter_as_of": "2026-10-27T21:00:00.000Z"}})
	if err != nil || got.Data.(map[string]any)["replayed"] != false || f.audited("view_deleted", lvView) != 1 {
		t.Fatal(got, err)
	}
	if _, err = as(Request{Operation: "linear_view_get", Args: map[string]any{"view_id": lvView}}); err == nil {
		t.Fatal("a forgotten view is still read")
	}
}

// Schema 38 on Postgres: schema 37's kind CHECK, under the name Postgres
// gives an inline CHECK, is replaced by one that takes project views.
func TestPostgresLinearViewsSchema38Upgrade(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := strings.ReplaceAll(newID("lv"), "-", "_")
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	owner := newID("owner") + "@identity-test.example"
	check := func() string {
		var def string
		if err := admin.QueryRowContext(ctx, `SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname=$1 AND c.conname='linear_views_kind_check'`, schema).Scan(&def); err != nil {
			t.Fatal(err)
		}
		return def
	}
	s, err := OpenPostgres(ctx, parsed.String(), owner)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Back to schema 37: its CHECK as Postgres named it.
	if _, err = admin.ExecContext(ctx, leadDowngradePostgres(schema)+rolesDowngradePostgres(schema)+kindDowngradePostgres(schema)+`ALTER TABLE `+schema+`.linear_views DROP CONSTRAINT linear_views_kind_check, ADD CHECK(kind='issue'); DELETE FROM `+schema+`.clawbridge_schema WHERE version>=38`); err != nil {
		t.Fatal(err)
	}
	if def := check(); strings.Contains(def, "project") {
		t.Fatal("schema 37's CHECK took project views:", def)
	}
	if s, err = OpenPostgres(ctx, parsed.String(), owner); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var version int
	if err = admin.QueryRowContext(ctx, `SELECT MAX(version) FROM `+schema+`.clawbridge_schema`).Scan(&version); err != nil || version != 44 {
		t.Fatal(version, err)
	}
	if def := check(); !strings.Contains(def, "'project'") {
		t.Fatal(def)
	}
}
