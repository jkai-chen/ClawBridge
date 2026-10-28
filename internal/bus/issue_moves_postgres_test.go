package bus

import (
	"strings"
	"testing"
)

// On the hosted authority the project owner and a lead, without a session,
// reserve and settle a move; a member without a session, and a reader,
// are refused; the identifier is rewritten by the issue's UUID.
func TestPostgresIssueMoves(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.s.SetIssueMoveProven(true)
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
	must := func(role string, r Request) map[string]any {
		t.Helper()
		out, err := as(role, r)
		if err != nil {
			t.Fatal(r.Operation, err)
		}
		m, _ := out.Data.(map[string]any)
		return m
	}
	binding := taskboardArgsV2(0)
	binding["linear_team_ids"] = []string{teamOne, teamTwo}
	must("owner", Request{Operation: "taskboard_set_v2", Args: binding})
	move := func(op string) map[string]any {
		return map[string]any{"operation_id": op, "issue_uuid": movedIssue, "from_team": teamOne, "to_team": teamTwo, "plan_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	}
	for _, role := range []string{"member", "reader"} {
		if _, err := as(role, Request{Operation: "issue_move_reserve", Args: move("m-" + role)}); !refusedWith(err, "owner or a project lead") {
			t.Fatal(role, "reserved:", err)
		}
	}
	if got := must("owner", Request{Operation: "issue_move_reserve", Args: move("m-owner")}); got["by"] != "owner" {
		t.Fatal(got)
	}
	if got := must("lead", Request{Operation: "issue_move_reserve", Args: move("m-lead")}); got["by"] != "lead" {
		t.Fatal(got)
	}
	// The lead can't settle the owner's move.
	if _, err := as("lead", Request{Operation: "issue_move_settle", Args: map[string]any{"operation_id": "m-owner", "outcome": "accepted", "new_identifier": "NEW-2"}}); err == nil || !strings.Contains(err.Error(), "no reservation of this kind") {
		t.Fatal("the lead settled the owner's move:", err)
	}
	if got := must("lead", Request{Operation: "issue_move_settle", Args: map[string]any{"operation_id": "m-lead", "outcome": "accepted", "old_identifier": "OLD-1", "new_identifier": "NEW-2"}}); got["state"] != "accepted" {
		t.Fatal(got)
	}
	var details string
	if err := f.s.db.QueryRow(`SELECT details FROM history WHERE entity='issue_move' AND action='accepted'`).Scan(&details); err != nil || !strings.Contains(details, "NEW-2") {
		t.Fatal(details, err)
	}
}
