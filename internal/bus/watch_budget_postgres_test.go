package bus

import (
	"strings"
	"testing"
)

// Only the project owner sets the project's watch budget; every member
// reads it.
func TestPostgresProjectWatchBudget(t *testing.T) {
	f := newPGIdentityFixture(t)
	people := map[string]string{}
	for _, role := range []string{"reader", "member", "lead"} {
		people[role] = newID(role) + "@identity-test.example"
		f.member(people[role])
		f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": people[role], "role": role}})
	}
	set := Request{Operation: "comment_watch_budget_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"watch_budget": 50}}
	for _, role := range []string{"reader", "member", "lead"} {
		if _, err := f.as(people[role], "sub-"+role, set); err == nil || !strings.Contains(err.Error(), "owner") {
			t.Fatalf("a %s set the watch budget: %v", role, err)
		}
	}
	f.must(f.owner, "sub-owner", set)
	got := f.must(people["reader"], "sub-reader", Request{Operation: "comment_watch_status", Workspace: f.workspace, Project: "p", Args: map[string]any{}}).Data.(map[string]any)
	if got["watch_budget"] != 50 || got["budget_limit"] != 50 || got["budget_source"] != "project" {
		t.Fatal(got)
	}
	f.must(f.owner, "sub-owner", Request{Operation: "comment_watch_budget_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"watch_budget": nil}})
	got = f.must(f.owner, "sub-owner", Request{Operation: "comment_watch_status", Workspace: f.workspace, Project: "p", Args: map[string]any{}}).Data.(map[string]any)
	if got["watch_budget"] != nil || got["budget_source"] != nil {
		t.Fatal(got)
	}
}
