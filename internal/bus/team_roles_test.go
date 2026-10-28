package bus

import (
	"context"
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

// A lead's set (design §11.1 Q1, review T2'): only engineer and reviewer
// rows working only in teams the lead leads; never a lead row, their own
// included, a team or its leads, or a person's account link.
func TestTeamRolesLeadEdit(t *testing.T) {
	const teamA, teamB = "aaaa0000-0000-4000-8000-0000000000a1", "bbbb0000-0000-4000-8000-0000000000b2"
	me := "lead@example.test"
	base := func() TeamRoles {
		return TeamRoles{
			Teams: []TeamRoleTeam{{TeamID: teamA, Leads: []string{roleAda}}, {TeamID: teamB, Leads: []string{roleCleo}}},
			People: []TeamRolePerson{
				{LinearUserID: roleAda, Role: "lead", Teams: []string{teamA}, Principal: me},
				{LinearUserID: roleBen, Role: "engineer", Teams: []string{teamA}, Principal: "ben@example.test"},
				{LinearUserID: roleCleo, Role: "lead", Teams: []string{teamB}},
			},
		}
	}
	bound := []string{teamA, teamB}
	if got := ledTeams(base(), me, bound); len(got) != 1 || got[0] != teamA {
		t.Fatal(got)
	}
	if got := ledTeams(base(), "LEAD@example.test", bound); len(got) != 1 {
		t.Fatal("the principal compares normalised", got)
	}
	if got := ledTeams(base(), me, []string{teamB}); len(got) != 0 {
		t.Fatal("an unbound team is never led", got)
	}
	if got := ledTeams(base(), "ben@example.test", bound); len(got) != 0 {
		t.Fatal("an engineer leads nothing", got)
	}
	noLink := base()
	noLink.People[0].Principal = ""
	if got := ledTeams(noLink, "", bound); len(got) != 0 {
		t.Fatal("an unlinked lead row matches nobody", got)
	}
	led := []string{teamA}
	dan := TeamRolePerson{LinearUserID: "dddddddd-0000-4000-8000-00000000000d", Role: "reviewer", Teams: []string{teamA}}
	for name, c := range map[string]struct {
		edit func(*TeamRoles)
		want string
	}{
		"no change":          {func(*TeamRoles) {}, ""},
		"add a reviewer":     {func(m *TeamRoles) { m.People = append(m.People, dan) }, ""},
		"engineer->reviewer": {func(m *TeamRoles) { m.People[1].Role = "reviewer" }, ""},
		"remove an engineer": {func(m *TeamRoles) { m.People = m.People[:1:1]; m.People = append(m.People, base().People[2]) }, ""},
		"keep the link":      {func(m *TeamRoles) { m.People[1].Role = "reviewer"; m.People[1].Principal = "ben@example.test" }, ""},
		"own row":            {func(m *TeamRoles) { m.People[0].Role = "engineer" }, "sets leads"},
		"make a lead":        {func(m *TeamRoles) { m.People[1].Role = "lead" }, "sets leads"},
		"add a lead row":     {func(m *TeamRoles) { d := dan; d.Role = "lead"; m.People = append(m.People, d) }, "sets leads"},
		"remove a lead row":  {func(m *TeamRoles) { m.People = m.People[:2] }, "sets leads"},
		"change leads":       {func(m *TeamRoles) { m.Teams[0].Leads = nil }, "who leads them"},
		"drop a team":        {func(m *TeamRoles) { m.Teams = m.Teams[:1] }, "who leads them"},
		"another team":       {func(m *TeamRoles) { d := dan; d.Teams = []string{teamB}; m.People = append(m.People, d) }, "teams they lead"},
		"also another team":  {func(m *TeamRoles) { m.People[1].Teams = []string{teamA, teamB} }, "teams they lead"},
		"link an account":    {func(m *TeamRoles) { d := dan; d.Principal = "dan@example.test"; m.People = append(m.People, d) }, "links a person"},
		"change a link":      {func(m *TeamRoles) { m.People[1].Principal = "me@example.test" }, "links a person"},
		"clear a link":       {func(m *TeamRoles) { m.People[1].Principal = "" }, "links a person"},
	} {
		next := base()
		c.edit(&next)
		err := checkLeadEdit(base(), next, led)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Fatal(name, err)
		}
	}
}

// roleTrusted (§6.3, T2' (a)) for intake: a role in the issue's own bound
// team, at the current revision; reviewer never; no mapping trusts nobody.
func TestTeamRolesRoleTrusted(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	ctx := context.Background()
	pid := h.projectID()
	check := func(team, who string, roles []string) (bool, string) {
		tx, err := h.s.beginIntake(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		ok, reason, err := h.s.roleTrusted(ctx, tx, pid, team, who, roles)
		if err != nil {
			t.Fatal(err)
		}
		return ok, reason
	}
	both := []string{"lead", "engineer"}
	if ok, reason := check(sfTeam, roleAda, both); ok || !strings.Contains(reason, "no team roles") {
		t.Fatal(reason)
	}
	teams := []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda}}}
	h.call("team_roles_set", "", rolesSet(0, true, teams, []map[string]any{person(roleAda, "lead", sfTeam), person(roleBen, "engineer", sfTeam), person(roleCleo, "reviewer", sfTeam)}))
	for _, c := range []struct {
		team, who string
		roles     []string
		ok        bool
		reason    string
	}{
		{sfTeam, strings.ToUpper(roleAda), both, true, "role lead"},
		{sfTeam, roleBen, both, true, "role engineer"},
		{sfTeam, roleBen, []string{"lead"}, false, "isn't trusted"},
		{sfTeam, roleCleo, both, false, "isn't trusted"},
		{sfTeam, roleCleo, []string{"reviewer"}, false, "only lead and engineer"},
		{sfTeam, roleAda, nil, false, "no roles"},
		{roleTeamX, roleAda, both, false, "isn't bound"},
		{sfTeam, "dddddddd-0000-4000-8000-00000000000d", both, false, "no team role"},
	} {
		if ok, reason := check(c.team, c.who, c.roles); ok != c.ok || !strings.Contains(reason, c.reason) {
			t.Fatal(c, ok, reason)
		}
	}
}

// A local principal with capitals (Windows: local:SID:DOMAIN\user) is
// stored lowercased, and still matches.
func TestSamePrincipalIgnoresCase(t *testing.T) {
	if !samePrincipal("local:s-1-5-21-1:runner\\admin", "local:S-1-5-21-1:RUNNER\\Admin") || samePrincipal("local:1:ada", "local:1:ben") {
		t.Fatal("principal compare")
	}
}
