package bus

import (
	"strings"
	"testing"
)

const seedDoc = "d0c00000-0000-4000-8000-000000000001"

func seedArgs(template, state, token, expect string, extra ...string) map[string]any {
	a := map[string]any{"parent_kind": "team", "parent_id": teamOne, "template": template, "template_version": 1, "state": state, "seed_token": token, "expect_token": expect}
	for i := 0; i+1 < len(extra); i += 2 {
		a[extra[i]] = extra[i+1]
	}
	return a
}

// A seed is reserved pending, then settled with the read-back digest, by
// compare-and-swap on its token; the parent must be the binding's.
func TestDocSeedReserveSettleAndCAS(t *testing.T) {
	h := newHarness(t)
	if e := h.errorOf("doc_seed_get", "", map[string]any{"parent_kind": "team", "parent_id": teamOne}); !strings.Contains(e, "no task board binding") {
		t.Fatal("an unbound project read seeds:", e)
	}
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0))
	get := func() map[string]any { return map[string]any{"parent_kind": "team", "parent_id": teamOne} }
	if rows := h.call("doc_seed_get", "", get()).([]map[string]any); len(rows) != 0 {
		t.Fatal(rows)
	}
	h.call("doc_seed_put", "", seedArgs("developer", "pending", "aaaaaaaaaaaa", ""))
	// A second run that read no row cannot reserve over the first.
	if e := h.errorOf("doc_seed_put", "", seedArgs("developer", "pending", "bbbbbbbbbbbb", "")); !strings.Contains(e, "changed") {
		t.Fatal("a stale reserve won:", e)
	}
	digest := strings.Repeat("e", 64)
	h.call("doc_seed_put", "", seedArgs("developer", "seeded", "aaaaaaaaaaaa", "aaaaaaaaaaaa", "document_id", seedDoc, "seeded_sha256", digest))
	rows := h.call("doc_seed_get", "a", get()).([]map[string]any)
	if len(rows) != 1 || rows[0]["state"] != "seeded" || rows[0]["document_id"] != seedDoc || rows[0]["seeded_sha256"] != digest || rows[0]["template"] != "developer" {
		t.Fatal(rows)
	}
	if acts := h.actions("linear_doc_seed"); len(acts) != 2 || acts[0] != "pending" || acts[1] != "seeded" {
		t.Fatal("history", acts)
	}
	for name, bad := range map[string]map[string]any{
		"unknown template":   seedArgs("guide", "pending", "cccccccccccc", ""),
		"bad state":          seedArgs("reviewer", "done", "cccccccccccc", ""),
		"bad token":          seedArgs("reviewer", "pending", "XYZ", ""),
		"seeded sans doc":    seedArgs("reviewer", "seeded", "cccccccccccc", ""),
		"pending with doc":   seedArgs("reviewer", "pending", "cccccccccccc", "", "document_id", seedDoc),
		"another team":       {"parent_kind": "team", "parent_id": "22222222-2222-4222-8222-222222222222", "template": "reviewer", "template_version": 1, "state": "pending", "seed_token": "cccccccccccc"},
		"project of a board": {"parent_kind": "project", "parent_id": projectOne, "template": "reviewer", "template_version": 1, "state": "pending", "seed_token": "cccccccccccc"},
		"bad version":        {"parent_kind": "team", "parent_id": teamOne, "template": "reviewer", "template_version": 0, "state": "pending", "seed_token": "cccccccccccc"},
	} {
		if e := h.errorOf("doc_seed_put", "", bad); e == "" {
			t.Fatal(name, "accepted")
		}
	}
	// A one-project binding seeds under its project, not the team.
	h.call("taskboard_set_v2", "a", taskboardArgsV2(1, projectOne))
	if e := h.errorOf("doc_seed_get", "", get()); !strings.Contains(e, "not where") {
		t.Fatal("the team is still a parent:", e)
	}
	h.call("doc_seed_put", "", map[string]any{"parent_kind": "project", "parent_id": projectOne, "template": "reviewer", "template_version": 1, "state": "pending", "seed_token": "dddddddddddd"})
}
