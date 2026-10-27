package bus

import (
	"context"
	"strings"
	"testing"
)

// On the hosted authority, intake changes need a lead or owner, and a
// dismiss also the coordinator session; a member reads only. A request
// through a token queues under the workspace lock.
func TestPostgresIntakeRolesAndRequest(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.worker("coord")
	member := newID("m") + "@identity-test.example"
	f.member(member)
	f.must(member, "sub-member", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "member", Args: map[string]any{"instance": "member-i"}})
	as := func(op string, a map[string]any) (Result, error) {
		a["instance"] = "member-i"
		return f.as(member, "sub-member", Request{Operation: op, Workspace: f.workspace, Project: "p", Session: "member", Args: a})
	}
	if _, err := as("intake_policy_get", map[string]any{}); err != nil {
		t.Fatal("a member cannot read the policy:", err)
	}
	for op, a := range map[string]map[string]any{
		"intake_policy_set":   {"mode": "off"},
		"intake_trust_add":    {"linear_user_id": trustedUser},
		"intake_token_create": {},
	} {
		if _, err := as(op, a); !codeIs(err, "forbidden", "") {
			t.Errorf("%s by a member: %v", op, err)
		}
	}
	if _, err := f.in("coordinator_set", "coord", map[string]any{"target_session": "coord"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.in("intake_policy_set", "coord", map[string]any{"signals": []any{"S5"}}); err != nil {
		t.Fatal(err)
	}
	created, err := f.in("intake_token_create", "coord", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	token := created.Data.(map[string]any)["token"].(string)
	prove := func(context.Context, string, string, string) (IntakeIssue, error) {
		return IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7"}, nil
	}
	out, err := f.s.IntakeRequest(context.Background(), token, intakeIssue, prove)
	if err != nil || out["result"] != "queued" {
		t.Fatal(out, err)
	}
	if _, err = as("intake_dismiss", map[string]any{"issue_uuid": intakeIssue}); !codeIs(err, "forbidden", "coordinator") {
		t.Fatal("a member dismissed:", err)
	}
	if _, err = f.in("intake_dismiss", "coord", map[string]any{"issue_uuid": intakeIssue}); err != nil {
		t.Fatal(err)
	}
}

// The owner manages intake without a registered session; the role decides,
// never the session: a sessionless member reads and changes nothing, and
// can't dismiss (no coordinator session to be).
func TestPostgresIntakeWithoutASession(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.worker("coord")
	member := newID("m") + "@identity-test.example"
	f.member(member)
	owner := func(op string, a map[string]any) (Result, error) {
		return f.as(f.owner, "sub-owner", Request{Operation: op, Workspace: f.workspace, Project: "p", Args: a})
	}
	mem := func(op string, a map[string]any) (Result, error) {
		return f.as(member, "sub-member", Request{Operation: op, Workspace: f.workspace, Project: "p", Args: a})
	}
	if _, err := owner("intake_policy_set", map[string]any{"mode": "off", "signals": []any{"S1", "S5"}}); err != nil {
		t.Fatal("the owner without a session:", err)
	}
	if _, err := owner("intake_trust_add", map[string]any{"linear_user_id": trustedUser}); err != nil {
		t.Fatal(err)
	}
	got, err := mem("intake_trust_list", map[string]any{})
	if err != nil || len(got.Data.([]map[string]any)) != 1 {
		t.Fatal("a member reads without a session:", got, err)
	}
	if p, err := mem("intake_policy_get", map[string]any{}); err != nil || p.Data.(map[string]any)["mode"] != "off" {
		t.Fatal(p, err)
	}
	if _, err = mem("intake_policy_set", map[string]any{"mode": "notify"}); !codeIs(err, "forbidden", "") {
		t.Fatal("a sessionless member changed the policy:", err)
	}
	if _, err = owner("intake_policy_get", map[string]any{"instance": "x"}); err == nil || !strings.Contains(err.Error(), "instance requires session") {
		t.Fatal("an instance without a session:", err)
	}
	if _, err = f.in("coordinator_set", "coord", map[string]any{"target_session": "coord"}); err != nil {
		t.Fatal(err)
	}
	created, err := owner("intake_token_create", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	prove := func(context.Context, string, string, string) (IntakeIssue, error) {
		return IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7"}, nil
	}
	if _, err = owner("intake_policy_set", map[string]any{"mode": "notify"}); err != nil {
		t.Fatal(err)
	}
	if out, err := f.s.IntakeRequest(context.Background(), created.Data.(map[string]any)["token"].(string), intakeIssue, prove); err != nil || out["result"] != "queued" {
		t.Fatal(out, err)
	}
	if _, err = mem("intake_dismiss", map[string]any{"issue_uuid": intakeIssue}); !codeIs(err, "forbidden", "coordinator") {
		t.Fatal("a sessionless member dismissed:", err)
	}
	if out, err := owner("intake_dismiss", map[string]any{"issue_uuid": intakeIssue}); err != nil || out.Data.(map[string]any)["state"] != "dismissed" {
		t.Fatal(out, err)
	}
	if n := f.count(`SELECT COUNT(*) FROM history WHERE entity='intake' AND action='dismiss' AND changed_by_session IS NULL AND changed_by_principal=$1`, f.owner); n != 1 {
		t.Fatal("the sessionless dismiss audit:", n)
	}
}
