package bus

import (
	"strings"
	"testing"
)

// Only the owner sets domains; a lead edits only the domain they own, and
// only its trust list and coordinator; a reader reads.
func TestPostgresDispatcherRoles(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.worker("own")
	if _, err := f.in("taskboard_set_v2", "own", taskboardArgsV2(0, projectOne, projectTwo)); err != nil {
		t.Fatal(err)
	}
	people := map[string]string{}
	for _, role := range []string{"reader", "lead", "lead2"} {
		people[role] = newID(role) + "@identity-test.example"
		f.member(people[role])
		r := role
		if role == "lead2" {
			r = "lead"
		}
		f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": people[role], "role": r}})
	}
	f.must(people["lead"], "sub-lead", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "lead-eng", Args: map[string]any{"instance": "lead-eng-i"}})
	domain := func(id, project, owner string) map[string]any {
		return map[string]any{"id": id, "name": id, "match": map[string]any{"linear_projects": []any{project}}, "owner_principal": owner, "trusted_sessions": []any{}}
	}
	set := Request{Operation: "dispatcher_domains_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": 0, "dispatcher_domains": map[string]any{"domains": []any{domain("payments", projectOne, people["lead"]), domain("platform", projectTwo, people["lead2"])}}}}
	if _, err := f.as(people["lead"], "sub-lead", set); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatal("a lead set the domains:", err)
	}
	f.must(f.owner, "sub-owner", set)
	edit := func(who, domain string, rev int) error {
		_, err := f.as(people[who], "sub-"+who, Request{Operation: "dispatcher_domain_edit", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": rev, "domain": domain, "action": "trust", "target_session": "lead-eng"}})
		return err
	}
	if err := edit("lead", "payments", 1); err != nil {
		t.Fatal("a lead could not trust in their own domain:", err)
	}
	if err := edit("lead", "platform", 2); err == nil {
		t.Fatal("a lead edited another lead's domain")
	}
	if err := edit("reader", "payments", 2); err == nil {
		t.Fatal("a reader edited a domain")
	}
	out := f.must(people["reader"], "sub-reader", Request{Operation: "dispatcher_get", Workspace: f.workspace, Project: "p", Args: map[string]any{}}).Data.(map[string]any)
	if out["revision"] != int64(2) || len(out["domains"].([]DispatcherDomain)) != 2 {
		t.Fatal(out)
	}
}

// The same columns on Postgres.
func TestPostgresDispatcherSchemaColumns(t *testing.T) {
	f := newPGIdentityFixture(t)
	for _, q := range []string{
		`SELECT binding_secret_sha256,bound_instance,used_at FROM dispatcher_agent_credentials LIMIT 0`,
		`SELECT state,ciphertext,nonce,config_revision FROM dispatcher_model_keys LIMIT 0`,
		`SELECT token_sha256 FROM dispatcher_launcher_tokens LIMIT 0`,
		`SELECT issue_project_id,issue_labels FROM session_assignments LIMIT 0`,
	} {
		rows, err := f.s.db.Query(q)
		if err != nil {
			t.Fatal(q, err)
		}
		rows.Close()
	}
}
