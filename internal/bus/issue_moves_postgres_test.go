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
		return map[string]any{"operation_id": op, "issue_uuid": movedIssue, "from_team": teamOne, "to_team": teamTwo, "plan_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "calls": 8}
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

// Who acknowledges an unconfirmed move: the project owner, any; a project
// lead, only a move they confirmed as a lead; never another lead, a member
// or a session.
func TestPostgresIssueMoveAcknowledgedBy(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.s.SetIssueMoveProven(true)
	device := map[string]string{}
	people := map[string]string{}
	for _, role := range []string{"owner", "lead", "lead2", "member"} {
		p := newID(role) + "@identity-test.example"
		grant := role
		if role == "lead2" {
			grant = "lead"
		}
		code := f.invite(f.owner, p, grant)["code"].(string)
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
	binding := taskboardArgsV2(0)
	binding["linear_team_ids"] = []string{teamOne, teamTwo}
	if _, err := as("owner", Request{Operation: "taskboard_set_v2", Args: binding}); err != nil {
		t.Fatal(err)
	}
	move := func(op string) map[string]any {
		return map[string]any{"operation_id": op, "issue_uuid": movedIssue, "from_team": teamOne, "to_team": teamTwo, "plan_sha256": strings.Repeat("a", 64), "calls": 8}
	}
	for _, r := range []struct{ role, op string }{{"owner", "m-owner"}, {"lead", "m-lead"}, {"lead", "m-lead-2"}} {
		if _, err := as(r.role, Request{Operation: "issue_move_reserve", Args: move(r.op)}); err != nil {
			t.Fatal(err)
		}
	}
	ack := func(role, op string) error {
		_, err := as(role, Request{Operation: "project_create_ack", Args: map[string]any{"operation_id": op}})
		return err
	}
	for _, c := range []struct{ role, op, refusal string }{
		{"lead", "m-owner", "the project lead who confirmed"},
		{"lead2", "m-lead", "the project lead who confirmed"},
		{"member", "m-lead", "owner permission"},
	} {
		if err := ack(c.role, c.op); !refusedWith(err, c.refusal) {
			t.Fatal(c.role, "acknowledged", c.op, err)
		}
	}
	if _, err := as("lead", Request{Operation: "project_create_ack", Session: "s", Args: map[string]any{"operation_id": "m-lead"}}); err == nil {
		t.Fatal("a session acknowledged")
	}
	if err := ack("lead", "m-lead"); err != nil {
		t.Fatal("the lead who confirmed it:", err)
	}
	for _, op := range []string{"m-owner", "m-lead-2"} {
		if err := ack("owner", op); err != nil {
			t.Fatal("the owner:", op, err)
		}
	}
}

// On the hosted authority a family under a long operation ID finds its
// children again on a repeat and on its settle, and a neighbouring long
// operation never shares them, whatever the database's collation: the
// children are read by their exact operation IDs.
func TestPostgresIssueMoveFamilyLongOperation(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.s.SetIssueMoveProven(true)
	f.s.SetIssueFamilyMoveProven(true)
	key := newKey(t)
	owner := newID("owner") + "@identity-test.example"
	code := f.invite(f.owner, owner, "owner")["code"].(string)
	if _, err := f.join(owner, code, key); err != nil {
		t.Fatal(err)
	}
	must := func(r Request) map[string]any {
		t.Helper()
		r.Workspace, r.Project = f.workspace, "p"
		out, err := f.asDevice(owner, keyID(t, key), r)
		if err != nil {
			t.Fatal(r.Operation, err)
		}
		m, _ := out.Data.(map[string]any)
		return m
	}
	binding := taskboardArgsV2(0)
	binding["linear_team_ids"] = []string{teamOne, teamTwo}
	must(Request{Operation: "taskboard_set_v2", Args: binding})
	long := strings.Repeat("y", 128)
	// Under en_US collation ':' and ';' aren't neighbours, so a range over
	// the children's IDs could take this one's rows too.
	near := strings.Repeat("y", 127) + "z"
	must(Request{Operation: "issue_move_reserve", Args: familyArgs(long, childOne, childTwo)})
	must(Request{Operation: "issue_move_reserve", Args: familyArgs(near, childOne)})
	if again := must(Request{Operation: "issue_move_reserve", Args: familyArgs(long, childOne, childTwo)}); again["replay"] != true {
		t.Fatal(again)
	}
	kid := func(id string) map[string]any {
		return map[string]any{"issue_uuid": id, "outcome": "accepted", "old_identifier": "OLD-3", "new_identifier": "NEW-3"}
	}
	got := must(Request{Operation: "issue_move_settle", Args: map[string]any{"operation_id": long, "outcome": "accepted", "new_identifier": "NEW-1", "children": []any{kid(childOne), kid(childTwo)}}})
	if got["state"] != "accepted" {
		t.Fatal(got)
	}
	got = must(Request{Operation: "issue_move_settle", Args: map[string]any{"operation_id": near, "outcome": "uncertain", "children": []any{map[string]any{"issue_uuid": childOne, "outcome": "uncertain"}}}})
	if got["state"] != "uncertain" {
		t.Fatal("the neighbouring family:", got)
	}
}
