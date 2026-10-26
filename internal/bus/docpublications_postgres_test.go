package bus

import (
	"strings"
	"testing"
)

// A lead records publications; a reader may not; only the owner overrides
// a deny pattern.
func TestPostgresDocPublicationRoles(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.worker("own")
	if _, err := f.in("taskboard_set_v2", "own", taskboardArgsV2(0)); err != nil {
		t.Fatal(err)
	}
	people := map[string]string{}
	for _, role := range []string{"reader", "lead"} {
		people[role] = newID(role) + "@identity-test.example"
		f.member(people[role])
		f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": people[role], "role": role}})
	}
	put := Request{Operation: "doc_publication_put", Workspace: f.workspace, Project: "p", Args: pubArgs(1, 1, 1, "pending", "aaaaaaaaaaaa", "")}
	if _, err := f.as(people["reader"], "sub-reader", put); err == nil || !strings.Contains(err.Error(), "lead or owner") {
		t.Fatal("a reader recorded a publication:", err)
	}
	f.must(people["lead"], "sub-lead", put)
	override := Request{Operation: "doc_publish_override", Workspace: f.workspace, Project: "p", Args: map[string]any{"parent_kind": "team", "parent_id": teamOne, "source_path": pubPath, "operation_id": "publish-1", "pattern_ids": []any{"internal-name"}, "lines": []any{2}, "source_commit": pubCommit}}
	if _, err := f.as(people["lead"], "sub-lead", override); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatal("a lead overrode a deny pattern:", err)
	}
	f.must(f.owner, "sub-owner", override)
	if f.count(`SELECT COUNT(*) FROM history WHERE entity='linear_doc_publish_override' AND changed_by_principal=$1`, f.owner) != 1 {
		t.Fatal("the override is not in history")
	}
}
