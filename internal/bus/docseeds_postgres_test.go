package bus

import (
	"strings"
	"testing"
)

// Only a lead or owner reads or records seeds; the owner may do so without
// a session (the CLI).
func TestPostgresDocSeedsAreLeadOrOwner(t *testing.T) {
	f := newPGIdentityFixture(t)
	member := newID("member") + "@identity-test.example"
	f.member(member)
	f.worker("own")
	if _, err := f.in("taskboard_set_v2", "own", taskboardArgsV2(0)); err != nil {
		t.Fatal(err)
	}
	put := Request{Operation: "doc_seed_put", Workspace: f.workspace, Project: "p", Args: map[string]any{"parent_kind": "team", "parent_id": teamOne, "template": "developer", "template_version": 1, "state": "pending", "seed_token": "aaaaaaaaaaaa"}}
	if _, err := f.as(member, "sub-member", put); err == nil || !strings.Contains(err.Error(), "lead or owner") {
		t.Fatal("a member recorded a seed:", err)
	}
	f.must(f.owner, "sub-owner", put)
	got := f.must(f.owner, "sub-owner", Request{Operation: "doc_seed_get", Workspace: f.workspace, Project: "p", Args: map[string]any{"parent_kind": "team", "parent_id": teamOne}})
	if rows := got.Data.([]map[string]any); len(rows) != 1 || rows[0]["state"] != "pending" || rows[0]["updated_by"] != f.owner {
		t.Fatal(rows)
	}
	if f.count(`SELECT COUNT(*) FROM history WHERE entity='linear_doc_seed' AND changed_by_principal=$1`, f.owner) != 1 {
		t.Fatal("the seed is not in history")
	}
}
