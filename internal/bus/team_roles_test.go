package bus

import (
	"strings"
	"testing"
)

const (
	roleAda   = "aaaaaaaa-0000-4000-8000-00000000000a"
	roleBen   = "bbbbbbbb-0000-4000-8000-00000000000b"
	roleCleo  = "cccccccc-0000-4000-8000-00000000000c"
	roleTeamX = "99999999-0000-4000-8000-000000000099"
)

func rolesSet(rev int, confirm bool, teams []map[string]any, people []map[string]any) map[string]any {
	return map[string]any{"expected_revision": rev, "confirm": confirm, "mapping": map[string]any{"teams": teams, "people": people}}
}

func person(id, role string, teams ...string) map[string]any {
	return map[string]any{"linear_user_id": id, "role": role, "teams": teams}
}

// Unset, the mapping reads as revision 0 and empty. The owner previews
// (nothing written), then confirms; any session reads it; history counts
// and never names anyone.
func TestTeamRolesSetAndRead(t *testing.T) {
	h := newHarness(t)
	got := h.call("team_roles_get", "", nil).(map[string]any)
	if got["revision"] != 0 || len(got["teams"].([]map[string]any)) != 0 {
		t.Fatal(got)
	}
	teams := []map[string]any{{"team_id": sfTeam, "leads": []string{strings.ToUpper(roleAda)}}}
	people := []map[string]any{person(roleAda, "lead", sfTeam), person(roleBen, "reviewer", sfTeam), person(roleCleo, "engineer", sfTeam)}
	if err := h.viewErr("team_roles_set", "", rolesSet(0, false, teams, people)); !strings.Contains(err.Error(), "isn't linked") {
		t.Fatal(err)
	}
	h.link(0, []string{cpOne})
	preview := h.call("team_roles_set", "", rolesSet(0, false, teams, people)).(map[string]any)
	if preview["preview"] != true || preview["change"].(map[string]any)["added"] != 3 {
		t.Fatal(preview)
	}
	if read := h.call("team_roles_get", "", nil).(map[string]any); read["revision"] != 0 {
		t.Fatal("a preview wrote", read)
	}
	got = h.call("team_roles_set", "", rolesSet(0, true, teams, people)).(map[string]any)
	stored := got["teams"].([]map[string]any)
	if got["revision"] != 1 || len(stored) != 1 || stored[0]["team_bound"] != true || stored[0]["leads"].([]string)[0] != roleAda {
		t.Fatal(got)
	}
	if counts := got["roles"].(map[string]int); counts["lead"] != 1 || counts["reviewer"] != 1 || counts["engineer"] != 1 {
		t.Fatal(counts)
	}
	h.call("register_session", "roles-s", map[string]any{"instance": "roles-i", "role": "worker"})
	if read := h.call("team_roles_get", "roles-s", map[string]any{"instance": "roles-i"}).(map[string]any); read["revision"] != 1 {
		t.Fatal(read)
	}
	var details string
	if err := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='team_roles' ORDER BY sequence DESC LIMIT 1`).Scan(&details); err != nil || !strings.Contains(details, `"people":3`) || strings.Contains(details, roleAda) || strings.Contains(details, "aaaaaaaa") {
		t.Fatal(details, err)
	}
	// Ben becomes a member and Cleo leaves: one changed, one removed.
	people = []map[string]any{person(roleAda, "lead", sfTeam), person(roleBen, "engineer", sfTeam)}
	change := h.call("team_roles_set", "", rolesSet(1, false, teams, people)).(map[string]any)["change"].(map[string]any)
	if change["added"] != 0 || change["changed"] != 1 || change["removed"] != 1 {
		t.Fatal(change)
	}
}

// Refused: a session, a lead (Q1 ships owner-only), a stale revision, an
// unbound team, a lead who isn't a lead person in that team, more than 3
// leads, a person twice, an unknown role, a person in no team, a principal
// who isn't a project member, a CI principal, an unknown field.
func TestTeamRolesSetRefusals(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("register_session", "roles-s", map[string]any{"instance": "roles-i", "role": "worker"})
	ok := []map[string]any{person(roleAda, "lead", sfTeam)}
	lead := []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda}}}
	session := rolesSet(0, true, lead, ok)
	session["instance"] = "roles-i"
	if err := h.viewErr("team_roles_set", "roles-s", session); !strings.Contains(err.Error(), "a session can't") {
		t.Fatal(err)
	}
	stranger := person(roleBen, "engineer", sfTeam)
	stranger["principal"] = "nobody@example.test"
	ci := person(roleBen, "engineer", sfTeam)
	ci["principal"] = "ci:builder@w"
	four := []map[string]any{person(roleAda, "lead", sfTeam), person(roleBen, "lead", sfTeam), person(roleCleo, "lead", sfTeam), person("dddddddd-0000-4000-8000-00000000000d", "lead", sfTeam)}
	for want, a := range map[string]map[string]any{
		"current revision is 0":         rolesSet(1, true, lead, ok),
		"a team of this project's":      rolesSet(0, true, nil, []map[string]any{person(roleAda, "lead", roleTeamX)}),
		"listed under people with role": rolesSet(0, true, lead, []map[string]any{person(roleAda, "reviewer", sfTeam)}),
		"at most 3 leads":               rolesSet(0, true, []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda, roleBen, roleCleo, "dddddddd-0000-4000-8000-00000000000d"}}}, four),
		"listed once, with one role":    rolesSet(0, true, nil, []map[string]any{person(roleAda, "engineer", sfTeam), person(roleAda, "reviewer", sfTeam)}),
		"lead, engineer or reviewer":    rolesSet(0, true, nil, []map[string]any{person(roleAda, "boss", sfTeam)}),
		"at least one bound team":       rolesSet(0, true, nil, []map[string]any{person(roleAda, "engineer")}),
		"is a Linear user ID":           rolesSet(0, true, nil, []map[string]any{person("ada", "engineer", sfTeam)}),
		"a member of this project":      rolesSet(0, true, nil, []map[string]any{stranger}),
		"not a CI device":               rolesSet(0, true, nil, []map[string]any{ci}),
		"mapping is":                    {"expected_revision": 0, "confirm": true, "mapping": map[string]any{"people": []map[string]any{{"linear_user_id": roleAda, "role": "engineer", "teams": []string{sfTeam}, "email": "x"}}}},
		"requires mapping":              {"expected_revision": 0, "confirm": true},
	} {
		if err := h.viewErr("team_roles_set", "", a); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(want, err)
		}
	}
	// The owner's own principal is a project member, so it links.
	me := person(roleAda, "lead", sfTeam)
	me["principal"] = h.s.principal
	if got := h.call("team_roles_set", "", rolesSet(0, true, lead, []map[string]any{me})).(map[string]any); got["revision"] != 1 {
		t.Fatal(got)
	}
}

// A team the binding later drops stays in the mapping, marked unbound, so
// no consumer uses it.
func TestTeamRolesUnboundTeam(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("team_roles_set", "", rolesSet(0, true, []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda}}}, []map[string]any{person(roleAda, "lead", sfTeam)}))
	if _, err := h.s.db.Exec(`DELETE FROM taskboard_bindings`); err != nil {
		t.Fatal(err)
	}
	got := h.call("team_roles_get", "", nil).(map[string]any)
	if got["teams"].([]map[string]any)[0]["team_bound"] != false {
		t.Fatal(got)
	}
}

// Two first sets at revision 0: the second finds the row and gets the
// same conflict as a stale revision, never a database error (review L3).
func TestTeamRolesFirstSetRace(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	if _, err := h.s.db.Exec(`INSERT INTO team_role_mapping(project_id,revision,mapping,updated_by,updated_at) SELECT id,1,'{"teams":[],"people":[]}','x','2026-10-28T00:00:00Z' FROM projects`); err != nil {
		t.Fatal(err)
	}
	// The second writer read revision 0 before the first committed: run its
	// insert directly, as setTeamRoles would.
	r, err := h.s.db.Exec(`INSERT INTO team_role_mapping(project_id,revision,mapping,updated_by,updated_at) SELECT id,1,'{}','y','2026-10-28T00:00:00Z' FROM projects WHERE true ON CONFLICT(project_id) DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := r.RowsAffected(); n != 0 {
		t.Fatal("the racing insert wrote")
	}
	if err := h.viewErr("team_roles_set", "", rolesSet(0, true, nil, []map[string]any{person(roleAda, "engineer", sfTeam)})); err == nil || !strings.Contains(err.Error(), "current revision is 1") {
		t.Fatal(err)
	}
}

// A local principal with capitals (Windows: local:SID:DOMAIN\user) is
// stored lowercased, and still matches.
func TestSamePrincipalIgnoresCase(t *testing.T) {
	if !samePrincipal("local:s-1-5-21-1:runner\\admin", "local:S-1-5-21-1:RUNNER\\Admin") || samePrincipal("local:1:ada", "local:1:ben") {
		t.Fatal("principal compare")
	}
}
