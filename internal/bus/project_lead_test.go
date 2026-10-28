package bus

import (
	"fmt"
	"strings"
	"testing"
)

// The project lead setting: off until set; the owner turns it on (on_at
// stamped), and on again keeps on_at; any session reads it; a session, a
// stale revision and a bad mode are refused. A write is recorded by
// outcome, never naming the lead.
func TestProjectLeadSetting(t *testing.T) {
	h := newHarness(t)
	got := h.call("project_lead_get", "", nil).(map[string]any)
	if got["revision"] != 0 || got["mode"] != "off" || got["on_at"] != nil {
		t.Fatal(got)
	}
	got = h.call("project_lead_set", "", map[string]any{"expected_revision": 0, "mode": "on"}).(map[string]any)
	onAt := got["on_at"]
	if got["revision"] != 1 || got["mode"] != "on" || onAt == nil {
		t.Fatal(got)
	}
	if got = h.call("project_lead_set", "", map[string]any{"expected_revision": 1, "mode": "on"}).(map[string]any); got["on_at"] != onAt {
		t.Fatal("on again moved on_at", got)
	}
	if got = h.call("project_lead_set", "", map[string]any{"expected_revision": 2, "mode": "off"}).(map[string]any); got["mode"] != "off" || got["on_at"] != onAt {
		t.Fatal(got)
	}
	h.call("register_session", "s", map[string]any{"instance": "s-i", "role": "worker"})
	if read := h.call("project_lead_get", "s", map[string]any{"instance": "s-i"}).(map[string]any); read["revision"] != 3 {
		t.Fatal(read)
	}
	for want, c := range map[string]struct {
		op, session string
		args        map[string]any
	}{
		"a session can't":         {"project_lead_set", "s", map[string]any{"instance": "s-i", "expected_revision": 3, "mode": "on"}},
		"current revision is 3":   {"project_lead_set", "", map[string]any{"expected_revision": 1, "mode": "on"}},
		"mode is on or off":       {"project_lead_set", "", map[string]any{"expected_revision": 3, "mode": "auto"}},
		"requires expected":       {"project_lead_set", "", map[string]any{"mode": "on"}},
		"by UUID":                 {"project_lead_record", "", map[string]any{"linear_project": "P-1", "source": "apply", "outcome": "accepted"}},
		"create or apply":         {"project_lead_record", "", map[string]any{"linear_project": cpOne, "source": "watcher", "outcome": "accepted"}},
		"accepted or uncertain":   {"project_lead_record", "", map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "refused"}},
		"a session can't change ": {"project_lead_record", "s", map[string]any{"instance": "s-i", "linear_project": cpOne, "source": "apply", "outcome": "accepted"}},
	} {
		if e := h.errorOf(c.op, c.session, c.args); !strings.Contains(e, strings.TrimSpace(want)) {
			t.Fatal(want, e)
		}
	}
	if e := h.errorOf("project_lead_record", "", map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted", "members_before": 0}); !strings.Contains(e, "member counts") {
		t.Fatal(e)
	}
	h.call("project_lead_record", "", map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted", "replaced": true, "members_before": 0, "members_after": 1})
	var details string
	if err := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_lead' AND action='lead'`).Scan(&details); err != nil || !strings.Contains(details, `"replaced":true`) || !strings.Contains(details, cpOne) || !strings.Contains(details, `"members_after":1`) {
		t.Fatal(details, err)
	}
}

// An accepted lead write tells the new lead's sessions, once per
// operation, with the project's UUID only. The recipient comes from the
// stored mapping, never the client's UUID alone (review): a lead row that
// leads a bound team, linked by the owner to a principal who is still a
// project member.
func TestProjectLeadNotice(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("register_session", "ada-s", map[string]any{"instance": "ada-i", "role": "worker"})
	me := h.s.principal
	ada := person(roleAda, "lead", sfTeam)
	ada["principal"] = me
	cleo := person(roleCleo, "engineer", sfTeam)
	cleo["principal"] = me
	h.call("team_roles_set", "", rolesSet(0, true, []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda, roleBen}}}, []map[string]any{ada, person(roleBen, "lead", sfTeam), cleo}))
	record := func(lead, op, outcome string) map[string]any {
		return h.call("project_lead_record", "", map[string]any{"linear_project": cpOne, "source": "apply", "outcome": outcome, "lead": lead, "operation_id": op}).(map[string]any)
	}
	notices := func() []string {
		rows, err := h.s.db.Query(`SELECT body FROM messages WHERE kind='project-lead'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var b string
			rows.Scan(&b)
			out = append(out, b)
		}
		return out
	}
	// Locally every session is the one principal's: each active one is told.
	var mine int
	if err := h.s.db.QueryRow(`SELECT count(*) FROM sessions WHERE status='active' AND role<>'system' AND owner_principal=?`, me).Scan(&mine); err != nil || mine < 1 {
		t.Fatal(mine, err)
	}
	if got := record(strings.ToUpper(roleAda), "op-1", "accepted"); got["notified"] != mine {
		t.Fatal(got, mine)
	}
	n := notices()
	if len(n) != mine || !strings.Contains(n[0], cpOne) || strings.Contains(n[0], roleAda) || !strings.Contains(n[0], "not an instruction") {
		t.Fatal(n)
	}
	for name, c := range map[string][3]string{
		"a retry":                  {roleAda, "op-1", "accepted"},
		"uncertain":                {roleAda, "op-2", "uncertain"},
		"a lead with no link":      {roleBen, "op-3", "accepted"},
		"a linked non-lead":        {roleCleo, "op-4", "accepted"},
		"someone not in the roles": {"dddddddd-0000-4000-8000-00000000000d", "op-5", "accepted"},
	} {
		if got := record(c[0], c[1], c[2]); got["notified"] != 0 || len(notices()) != mine {
			t.Fatal(name, got, notices())
		}
	}
	var details string
	if err := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_lead' AND action='lead' ORDER BY sequence LIMIT 1`).Scan(&details); err != nil || !strings.Contains(details, fmt.Sprintf(`"notified":%d`, mine)) || strings.Contains(details, roleAda) {
		t.Fatal(details, err)
	}
	// A principal no longer a project member is never told, nor a lead of
	// a team that left the binding.
	if _, err := h.s.db.Exec(`UPDATE team_role_mapping SET mapping=replace(mapping,?,?)`, `"principal":"`+me+`"`, `"principal":"gone@example.test"`); err != nil {
		t.Fatal(err)
	}
	if got := record(roleAda, "op-6", "accepted"); got["notified"] != 0 {
		t.Fatal("a removed principal was told", got)
	}
	if _, err := h.s.db.Exec(`UPDATE team_role_mapping SET mapping=replace(mapping,?,?)`, `"principal":"gone@example.test"`, `"principal":"`+me+`"`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.db.Exec(`DELETE FROM taskboard_bindings`); err != nil {
		t.Fatal(err)
	}
	if got := record(roleAda, "op-7", "accepted"); got["notified"] != 0 {
		t.Fatal("a lead of an unbound team was told", got)
	}
	for want, a := range map[string]map[string]any{
		"Linear user UUID":  {"linear_project": cpOne, "source": "apply", "outcome": "accepted", "lead": "ada", "operation_id": "op"},
		"names the write's": {"linear_project": cpOne, "source": "apply", "outcome": "accepted", "lead": roleAda},
	} {
		if e := h.errorOf("project_lead_record", "", a); !strings.Contains(e, want) {
			t.Fatal(want, e)
		}
	}
}

// The earlier lead is told only when ClawBridge itself set them there and
// the client agrees (review): a spoofed previous UUID and a lead set
// outside ClawBridge tell nobody, counted not_told.
func TestProjectLeadOldNotice(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	me := h.s.principal
	ada, ben := person(roleAda, "lead", sfTeam), person(roleBen, "lead", sfTeam)
	ada["principal"], ben["principal"] = me, me
	h.call("team_roles_set", "", rolesSet(0, true, []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda, roleBen}}}, []map[string]any{ada, ben}))
	record := func(lead, previous, op string) map[string]any {
		a := map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted", "lead": lead, "operation_id": op}
		if previous != "" {
			a["previous_lead"] = previous
		}
		return h.call("project_lead_record", "", a).(map[string]any)
	}
	oldNotices := func() int {
		var n int
		if err := h.s.db.QueryRow(`SELECT count(*) FROM messages WHERE kind='project-lead' AND body LIKE 'You''re no longer%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A lead set outside ClawBridge: nothing recorded there, so not told.
	if got := record(roleAda, roleCleo, "op-1"); got["old_lead"] != "not_told" || oldNotices() != 0 {
		t.Fatal(got)
	}
	// ClawBridge set Ada; a client naming someone else as the earlier lead
	// tells nobody.
	if got := record(roleBen, "dddddddd-0000-4000-8000-00000000000d", "op-2"); got["old_lead"] != "not_told" || oldNotices() != 0 {
		t.Fatal("spoofed", got)
	}
	// ClawBridge set Ben (op-2); replacing Ben with Ada tells Ben, once.
	if got := record(roleAda, roleBen, "op-3"); got["old_lead"] != "told" || oldNotices() == 0 {
		t.Fatal(got)
	}
	n := oldNotices()
	if got := record(roleAda, roleBen, "op-3"); oldNotices() != n || got["notified"] != 0 {
		t.Fatal("a retry told again", got)
	}
	// The same lead again is no replacement: nobody is told they're out.
	if got := record(roleAda, roleAda, "op-4"); got["old_lead"] != "" || oldNotices() != n {
		t.Fatal("the same lead was told they're out", got)
	}
	var details string
	if err := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_lead' AND action='lead' ORDER BY sequence DESC LIMIT 1`).Scan(&details); err != nil || strings.Contains(details, roleBen) || strings.Contains(details, roleAda) {
		t.Fatal(details, err)
	}
}

// With no session open the notice waits, and the person's next session
// registration delivers it once. Locally that person is the OS user, whose
// principal keeps its case (capitals on Windows) while the notice holds it
// normalized.
func TestProjectLeadNoticeWaitsForRegister(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	ada := person(roleAda, "lead", sfTeam)
	ada["principal"] = h.s.principal
	h.call("team_roles_set", "", rolesSet(0, true, []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda}}}, []map[string]any{ada}))
	if _, err := h.s.db.Exec(`UPDATE sessions SET status='retired' WHERE role<>'system'`); err != nil {
		t.Fatal(err)
	}
	got := h.call("project_lead_record", "", map[string]any{"linear_project": cpOne, "source": "apply", "outcome": "accepted", "lead": roleAda, "operation_id": "op-1"}).(map[string]any)
	if got["notified"] != 0 || got["queued"] != 1 {
		t.Fatal(got)
	}
	h.call("register_session", "late", map[string]any{"instance": "late-i", "role": "worker"})
	h.call("register_session", "later", map[string]any{"instance": "later-i", "role": "worker"})
	var n int
	if err := h.s.db.QueryRow(`SELECT count(*) FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE m.kind='project-lead'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("delivered", n, err)
	}
}
