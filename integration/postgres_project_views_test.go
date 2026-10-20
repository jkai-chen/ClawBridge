package integration

import "testing"

func TestPostgresProjectAndMemberViewsAreScoped(t *testing.T) {
	f := newPGFixture(t)
	f.setup("visible")
	f.call(pgOwner, "project_create", "", "", map[string]any{
		"slug": "restricted", "git_remote": "https://example.test/review/" + f.w + "/restricted.git",
	})

	ownerProjects := object(t, f.call(pgOwner, "list_projects", "", "", nil))
	ownerRows := array(t, ownerProjects["projects"])
	if len(ownerRows) != 2 {
		t.Fatalf("workspace owner sees %d projects, want 2: %v", len(ownerRows), ownerProjects)
	}
	for _, raw := range ownerRows {
		row := object(t, raw)
		if row["role"] != "owner" {
			t.Fatalf("workspace owner project role=%v: %v", row["role"], row)
		}
	}

	readerProjects := object(t, f.call(pgPrincipal("reader"), "list_projects", "", "", nil))
	readerRows := array(t, readerProjects["projects"])
	if len(readerRows) != 1 {
		t.Fatalf("reader sees %d projects, want only visible: %v", len(readerRows), readerProjects)
	}
	visible := object(t, readerRows[0])
	if visible["slug"] != "visible" || visible["role"] != "reader" {
		t.Fatalf("reader project row not scoped/effective role: %v", visible)
	}
	f.reject("stranger@example.test", "list_projects", "", "", nil)
	f.reject("stranger@example.test", "list_members", "visible", "", nil)

	for _, name := range []string{"owner", "alice", "bob", "lead", "reader"} {
		f.role("visible", name, map[string]string{
			"owner": "owner", "alice": "member", "bob": "member",
			"lead": "lead", "reader": "reader",
		}[name])
	}
	members := object(t, f.call(pgPrincipal("reader"), "list_members", "visible", "", nil))
	if members["project_id"] == "" {
		t.Fatalf("member view lacks project ID: %v", members)
	}
	rows := array(t, members["members"])
	effective := map[string]string{}
	for _, raw := range rows {
		row := object(t, raw)
		principal := stringField(t, row, "principal")
		if _, duplicate := effective[principal]; duplicate {
			t.Fatalf("duplicate member principal %s: %v", principal, rows)
		}
		effective[principal] = stringField(t, row, "role")
		for _, forbidden := range []string{"instance", "session"} {
			if _, exists := row[forbidden]; exists {
				t.Fatalf("member view leaks %s: %v", forbidden, row)
			}
		}
	}
	want := map[string]string{
		pgOwner:               "owner",
		pgPrincipal("alice"):  "member",
		pgPrincipal("bob"):    "member",
		pgPrincipal("lead"):   "lead",
		pgPrincipal("reader"): "reader",
	}
	if len(effective) != len(want) {
		t.Fatalf("member view has %d rows, want %d: %v", len(effective), len(want), effective)
	}
	for principal, role := range want {
		if effective[principal] != role {
			t.Errorf("effective role for %s=%q, want %q: %v", principal, effective[principal], role, effective)
		}
	}
	f.reject("stranger@example.test", "list_members", "visible", "", nil)
	f.reject(pgOwner, "list_members", "missing", "", nil)
}
