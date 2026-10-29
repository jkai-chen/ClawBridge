package bus

import (
	"strings"
	"testing"
)

// On the hosted authority the project lead setting and its records are
// the owner's, from their own device: a member reads but never sets, and a
// Google sign-in is refused.
func TestPostgresProjectLeadAccess(t *testing.T) {
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
	set := map[string]any{"expected_revision": 0, "mode": "on"}
	record := map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted"}
	for op, a := range map[string]map[string]any{"project_lead_set": set, "project_lead_record": record} {
		if _, err := as("member", Request{Operation: op, Args: a}); !refusedWith(err, "owner") {
			t.Fatal("member:", op, err)
		}
		if _, err := f.as(f.owner, "sub-owner", Request{Operation: op, Workspace: f.workspace, Project: "p", Args: a}); !refusedWith(err, "device") {
			t.Fatal("google:", op, err)
		}
	}
	if _, err := as("owner", Request{Operation: "project_lead_set", Args: set}); err != nil {
		t.Fatal(err)
	}
	got, err := as("member", Request{Operation: "project_lead_get", Args: map[string]any{}})
	if err != nil || got.Data.(map[string]any)["mode"] != "on" {
		t.Fatal(got, err)
	}
	// The notice reaches the member the owner linked as the lead, and only
	// their sessions: never the owner's, who recorded the write.
	if _, err := as("owner", Request{Operation: "register_session", Session: "owner-s", Args: map[string]any{"instance": "owner-i", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := as("owner", Request{Operation: "taskboard_set_v2", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": []string{cpOne}}}); err != nil {
		t.Fatal(err)
	}
	lead := person(roleAda, "lead", sfTeam)
	lead["principal"] = people["member"]
	if _, err := as("owner", Request{Operation: "team_roles_set", Args: rolesSet(0, true, []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda}}}, []map[string]any{lead})}); err != nil {
		t.Fatal(err)
	}
	// The member has no session yet: the notice waits, and the owner's
	// overview counts it waiting.
	got, err = as("owner", Request{Operation: "project_lead_record", Args: map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted", "lead": roleAda, "operation_id": "op-1"}})
	if err != nil || got.Data.(map[string]any)["notified"] != 0 || got.Data.(map[string]any)["queued"] != 1 {
		t.Fatal(got, err)
	}
	overview, err := as("owner", Request{Operation: "project_overview", Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if leads := overview.Data.(map[string]any)["project_leads"].(map[string]any); leads["set_7d"] != 1 || leads["waiting"] != 1 || leads["told_7d"] != 0 {
		t.Fatal(leads)
	}
	if member, err := as("member", Request{Operation: "project_overview", Args: map[string]any{}}); err != nil || member.Data.(map[string]any)["project_leads"] != nil {
		t.Fatal("a member sees the lead counts", member, err)
	}
	// The PROVEN gates are the owner's to see.
	if gates, _ := mustOverview(t, as)["proven_gates"].([]map[string]any); len(gates) != len(ProvenGateNames) {
		t.Fatal("the owner doesn't see the PROVEN gates", gates)
	}
	if member, err := as("member", Request{Operation: "project_overview", Args: map[string]any{}}); err != nil || member.Data.(map[string]any)["proven_gates"] != nil {
		t.Fatal("a member sees the PROVEN gates", member, err)
	}
	// Delivered once, when they register a session; a second session gets
	// nothing more.
	for _, name := range []string{"member-s", "member-t"} {
		if _, err := as("member", Request{Operation: "register_session", Session: name, Args: map[string]any{"instance": name + "-i", "role": "worker"}}); err != nil {
			t.Fatal(err)
		}
	}
	if leads := mustOverview(t, as)["project_leads"].(map[string]any); leads["told_7d"] != 1 || leads["waiting"] != 0 {
		t.Fatal("after delivery", leads)
	}
	var delivered int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM deliveries d JOIN messages m ON m.id=d.message_id JOIN projects p ON p.id=m.project_id WHERE m.kind='project-lead' AND p.workspace_id IN (SELECT id FROM workspaces WHERE id=$1 OR name=$1)`, f.workspace).Scan(&delivered); err != nil || delivered != 1 {
		t.Fatal("delivered", delivered, err)
	}
	var to string
	if err := f.s.db.QueryRow(`SELECT s.owner_principal FROM deliveries d JOIN messages m ON m.id=d.message_id JOIN sessions s ON s.id=d.recipient_id JOIN projects p ON p.id=m.project_id WHERE m.kind='project-lead' AND p.workspace_id IN (SELECT id FROM workspaces WHERE id=$1 OR name=$1)`, f.workspace).Scan(&to); err != nil || to != people["member"] {
		t.Fatal(to, err)
	}
	// A member removed from the project is never told, even while their
	// sessions are still active (T3: the link is re-checked when used).
	if _, err := f.s.db.Exec(`DELETE FROM project_roles WHERE principal=$1`, people["member"]); err != nil {
		t.Fatal(err)
	}
	got, err = as("owner", Request{Operation: "project_lead_record", Args: map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted", "lead": roleAda, "operation_id": "op-2"}})
	if err != nil || got.Data.(map[string]any)["notified"] != 0 || got.Data.(map[string]any)["queued"] != 0 {
		t.Fatal("a removed member was told", got, err)
	}
}

func mustOverview(t *testing.T, as func(string, Request) (Result, error)) map[string]any {
	t.Helper()
	got, err := as("owner", Request{Operation: "project_overview", Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return got.Data.(map[string]any)
}

// Stale notices (review R1): a later notice of the other kind for the same
// person and project replaces a waiting one, so a member away while they
// were made lead and then replaced hears only that they're no longer the
// lead; and a notice waiting past 30 days is never delivered or counted.
func TestPostgresProjectLeadStaleNotices(t *testing.T) {
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
	ada, ben := person(roleAda, "lead", sfTeam), person(roleBen, "lead", sfTeam)
	ada["principal"], ben["principal"] = people["member"], people["owner"]
	if _, err := as("owner", Request{Operation: "team_roles_set", Args: rolesSet(0, true, []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda, roleBen}}}, []map[string]any{ada, ben})}); err != nil {
		t.Fatal(err)
	}
	record := func(lead, previous, op string) {
		t.Helper()
		a := map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted", "lead": lead, "operation_id": op}
		if previous != "" {
			a["previous_lead"] = previous
		}
		if _, err := as("owner", Request{Operation: "project_lead_record", Args: a}); err != nil {
			t.Fatal(err)
		}
	}
	record(roleAda, "", "op-1")
	record(roleBen, roleAda, "op-2")
	if _, err := as("member", Request{Operation: "register_session", Session: "member-s", Args: map[string]any{"instance": "member-i", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.db.Query(`SELECT m.body FROM deliveries d JOIN messages m ON m.id=d.message_id JOIN sessions s ON s.id=d.recipient_id WHERE m.kind='project-lead' AND s.owner_principal=$1`, people["member"])
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for rows.Next() {
		var b string
		rows.Scan(&b)
		bodies = append(bodies, b)
	}
	rows.Close()
	if len(bodies) != 1 || !strings.Contains(bodies[0], "no longer") {
		t.Fatal("superseded notice delivered", bodies)
	}
	// A notice waiting past 30 days is out of date.
	pid := ""
	if err := f.s.db.QueryRow(`SELECT p.id FROM projects p WHERE p.slug='p' AND p.workspace_id IN (SELECT id FROM workspaces WHERE id=$1 OR name=$1)`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	before := mustOverview(t, as)["project_leads"].(map[string]any)["waiting"]
	oldID := newID("pln_")
	if _, err := f.s.db.Exec(`INSERT INTO project_lead_notices(id,project_id,principal,linear_project,kind,op,created_at) VALUES($4,$1,$2,$3,'new','op-old','2026-01-01T00:00:00.000000000Z')`, pid, people["member"], cpOne, oldID); err != nil {
		t.Fatal(err)
	}
	if leads := mustOverview(t, as)["project_leads"].(map[string]any); leads["waiting"] != before {
		t.Fatal("an expired notice counts as waiting", leads)
	}
	if _, err := as("member", Request{Operation: "register_session", Session: "member-t", Args: map[string]any{"instance": "member-j", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	var delivered any
	if err := f.s.db.QueryRow(`SELECT delivered_at FROM project_lead_notices WHERE id=$1`, oldID).Scan(&delivered); err != nil || delivered != nil {
		t.Fatal("an expired notice was delivered", delivered, err)
	}
	record(roleAda, roleBen, "op-3")
	var left int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM project_lead_notices WHERE id=$1`, oldID).Scan(&left); err != nil || left != 0 {
		t.Fatal("an expired notice wasn't pruned", left, err)
	}
}
