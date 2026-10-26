package bus

import (
	"strings"
	"testing"
)

// The document policy on Postgres: the owner sets it, a lead reads the
// deny patterns' regexes, a reader sees their IDs only and cannot set it.
func TestPostgresDocPolicyRoles(t *testing.T) {
	f := newPGIdentityFixture(t)
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "doc_policy_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": 0, "doc_policy": dpPolicy()}}); err != nil {
		t.Fatal(err)
	}
	for role, full := range map[string]bool{"reader": false, "lead": true} {
		who := newID(role) + "@identity-test.example"
		f.member(who)
		f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": who, "role": role}})
		out, err := f.as(who, "sub-"+role, Request{Operation: "doc_policy_get", Workspace: f.workspace, Project: "p", Args: map[string]any{}})
		if err != nil {
			t.Fatal(role, err)
		}
		got := out.Data.(map[string]any)
		patterns, isFull := got["deny_patterns"].([]DocDenyPattern)
		if isFull != full || got["revision"] != int64(1) || len(got["initiatives"].([]string)) != 1 {
			t.Fatal(role, got)
		}
		if _, shown := got["source_root"]; shown != full || (!full && got["source_root_set"] != true) {
			t.Fatal(role, "source root", got)
		}
		if !full && (len(got["deny_patterns"].([]string)) != 1 || got["deny_patterns"].([]string)[0] != "internal-name") {
			t.Fatal(role, "pattern IDs", got)
		}
		if full && patterns[0].Regex == "" {
			t.Fatal(role, got)
		}
		if _, err = f.as(who, "sub-"+role, Request{Operation: "doc_policy_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": 1, "doc_policy": dpPolicy()}}); err == nil || !strings.Contains(err.Error(), "owner") {
			t.Fatal(role, "set the policy:", err)
		}
	}
}
