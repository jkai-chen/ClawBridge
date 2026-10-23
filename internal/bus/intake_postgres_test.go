package bus

import (
	"context"
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
