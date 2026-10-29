package watcher

import (
	"testing"
	"time"
)

const (
	roleLead     = "bbbbbbbb-0000-4000-8000-00000000000b"
	roleReviewer = "cccccccc-0000-4000-8000-00000000000c"
	roleGuest    = "dddddddd-0000-4000-8000-00000000000d"
)

// A person the team roles trust for the issue's team is trusted when
// Linear says they are an active, non-guest user, read once and cached;
// a reviewer, a guest, an unreadable user and another team's person only
// notify (TEAM-ROLE-MAPPING-DESIGN §6.3, T2').
func TestIntakeTrustsByTeamRole(t *testing.T) {
	e := setup(t)
	teams := []map[string]any{{"team_id": team, "leads": []string{roleLead}}}
	people := []map[string]any{
		{"linear_user_id": roleLead, "role": "lead", "teams": []string{team}},
		{"linear_user_id": roleReviewer, "role": "reviewer", "teams": []string{team}},
		{"linear_user_id": roleGuest, "role": "engineer", "teams": []string{team}},
		{"linear_user_id": strangerID, "role": "engineer", "teams": []string{team}},
	}
	e.call("team_roles_set", "", map[string]any{"expected_revision": 0, "confirm": true, "mapping": map[string]any{"teams": teams, "people": people}})
	e.f.users = map[string]map[string]any{
		roleLead:     {"id": roleLead, "active": true, "guest": false},
		roleReviewer: {"id": roleReviewer, "active": true, "guest": false},
		roleGuest:    {"id": roleGuest, "active": true, "guest": true},
		// strangerID's read answers for someone else: unverified.
		strangerID: {"id": roleLead, "active": true, "guest": false},
	}
	e.f.labelled = []string{issueA, issueB}
	e.tick()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	add := func(who string) []map[string]any {
		return []map[string]any{entry(now, who, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	}
	e.f.history[issueA], e.f.history[issueB] = add(roleLead), add(roleGuest)
	e.dueNow()
	tick := e.tick()
	caps := map[string]string{}
	for _, row := range e.queue() {
		caps[row["issue_uuid"].(string)] = row["trigger_cap"].(string)
	}
	if caps[issueA] != "auto" || caps[issueB] != "notify" {
		t.Fatal("caps:", caps, tick.Intake)
	}
	if tick.Intake.Trust["role"] != 1 || tick.Intake.Trust["role_ineligible"] != 1 {
		t.Fatal("trust counts:", tick.Intake.Trust)
	}
	// The reviewer and the unreadable user only notify; the lead's
	// eligibility is cached, so it isn't read again.
	reads := func() int {
		n := 0
		for _, q := range e.f.queries {
			if q == userQuery {
				n++
			}
		}
		return n
	}
	before := reads()
	e.f.history[issueA], e.f.history[issueB] = add(roleReviewer), add(strangerID)
	e.sql(`UPDATE intake_queue SET state='closed'`)
	e.dueNow()
	tick = e.tick()
	for _, row := range e.queue() {
		if row["trigger_cap"] != "notify" {
			t.Fatal("trusted:", row)
		}
	}
	if tick.Intake.Trust["role_unverified"] != 1 || tick.Intake.Trust["role"] != 0 {
		t.Fatal("trust counts:", tick.Intake.Trust)
	}
	// Only the stranger was read: the reviewer's role isn't trusted, so no
	// read is spent on them.
	if reads()-before != 1 {
		t.Fatal("user reads:", reads()-before)
	}
	e.f.history[issueA] = add(roleLead)
	e.f.history[issueB] = nil
	e.sql(`UPDATE intake_queue SET state='closed'`)
	e.dueNow()
	before = reads()
	if tick = e.tick(); tick.Intake.Trust["role"] != 1 || reads() != before {
		t.Fatal("cached lead:", tick.Intake.Trust, reads()-before)
	}
}

// Role trust fails closed: team roles that can't be read trust nobody by
// role, and the poll says so (team_roles among its unreadable sources).
func TestIntakeUnreadableRolesAreReported(t *testing.T) {
	e := setup(t)
	teams := []map[string]any{{"team_id": team, "leads": []string{roleLead}}}
	people := []map[string]any{{"linear_user_id": roleLead, "role": "lead", "teams": []string{team}}}
	e.call("team_roles_set", "", map[string]any{"expected_revision": 0, "confirm": true, "mapping": map[string]any{"teams": teams, "people": people}})
	e.f.users = map[string]map[string]any{roleLead: {"id": roleLead, "active": true, "guest": false}}
	e.sql(`UPDATE team_role_mapping SET mapping='not json'`)
	e.f.labelled = []string{issueA}
	e.tick()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.history[issueA] = []map[string]any{entry(now, roleLead, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	e.dueNow()
	tick := e.tick()
	unreadable := false
	for _, u := range tick.Intake.Unreadable {
		unreadable = unreadable || u == "team_roles"
	}
	if !unreadable {
		t.Fatal("unreadable roles weren't reported:", tick.Intake.Unreadable)
	}
	for _, row := range e.queue() {
		if row["trigger_cap"] != "notify" {
			t.Fatal("trusted by unreadable roles:", row)
		}
	}
}
