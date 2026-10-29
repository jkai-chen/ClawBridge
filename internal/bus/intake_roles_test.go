package bus

import (
	"context"
	"strings"
	"testing"
)

// The intake policy's roles are the team roles intake trusts (§6.3): lead
// and engineer by default, or lead alone; a reviewer, any other name and a
// repeat are refused when set.
func TestIntakePolicyRoles(t *testing.T) {
	h := newIntakeHarness(t)
	for _, bad := range [][]any{{"reviewer"}, {"worker"}, {"lead", "lead"}, {"Lead"}} {
		if e := h.errorOf("intake_policy_set", "c", map[string]any{"roles": bad}); e == "" {
			t.Fatal(bad, "accepted")
		}
	}
	if e := h.errorOf("intake_policy_set", "c", map[string]any{"roles": []any{"reviewer"}}); !strings.Contains(e, "never trusted by role") {
		t.Fatal(e)
	}
	got := h.call("intake_policy_set", "c", map[string]any{"roles": []any{"lead", "engineer"}}).(map[string]any)
	if strings.Join(got["roles"].([]string), ",") != "engineer,lead" {
		t.Fatal(got["roles"])
	}
	h.call("intake_policy_set", "c", map[string]any{"roles": []any{}})
	if got := h.call("intake_policy_get", "c", nil).(map[string]any); got["roles_note"] != nil {
		t.Fatal("a valid policy has a note:", got)
	}
	// A list stored before the check, with a name it now refuses, turns
	// role trust off; the policy says so instead of failing silently.
	if _, err := h.s.db.Exec(`UPDATE intake_policies SET roles='["lead","reviewer"]'`); err != nil {
		t.Fatal(err)
	}
	got = h.call("intake_policy_get", "c", nil).(map[string]any)
	if note, _ := got["roles_note"].(string); !strings.Contains(note, "role trust is off") || !strings.Contains(note, `"reviewer"`) || !strings.Contains(note, "intake policy set --roles") {
		t.Fatal("no note for stored roles intake refuses:", got)
	}
}

// IntakeSystemState trusts by role each (team, person) roleTrusted
// trusts: none without a mapping; lead and engineer by default; lead alone
// when the policy says so; never a reviewer.
func TestIntakeStateRoleTrusted(t *testing.T) {
	h := newIntakeHarness(t)
	h.link(0, []string{cpOne})
	pid := h.projectID()
	state := func() IntakeState {
		t.Helper()
		st, err := h.s.IntakeSystemState(context.Background(), pid)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := state(); len(st.RoleTrusted) != 0 || st.RolesUnreadable {
		t.Fatal("no mapping:", st.RoleTrusted, st.RolesUnreadable)
	}
	teams := []map[string]any{{"team_id": sfTeam, "leads": []string{roleAda}}}
	h.call("team_roles_set", "", rolesSet(0, true, teams, []map[string]any{person(roleAda, "lead", sfTeam), person(roleBen, "engineer", sfTeam), person(roleCleo, "reviewer", sfTeam)}))
	st := state()
	if len(st.RoleTrusted) != 2 || !st.RoleTrusted[sfTeam+"|"+roleAda] || !st.RoleTrusted[sfTeam+"|"+roleBen] || st.RoleTrusted[sfTeam+"|"+roleCleo] {
		t.Fatal("default roles:", st.RoleTrusted)
	}
	h.call("intake_policy_set", "c", map[string]any{"roles": []any{"lead"}})
	if st = state(); len(st.RoleTrusted) != 1 || !st.RoleTrusted[sfTeam+"|"+roleAda] {
		t.Fatal("lead only:", st.RoleTrusted)
	}
	// An unreadable binding trusts nobody by role, and says so.
	if _, err := h.s.db.Exec(`UPDATE taskboard_bindings SET mapping='[]'`); err != nil {
		t.Fatal(err)
	}
	if st = state(); len(st.RoleTrusted) != 0 || !st.RolesUnreadable {
		t.Fatal("unreadable binding:", st.RoleTrusted, st.RolesUnreadable)
	}
}
