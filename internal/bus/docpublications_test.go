package bus

import (
	"strings"
	"testing"
)

const (
	pubCommit = "0123456789abcdef0123456789abcdef01234567"
	pubPath   = "docs/plan.md"
)

func pubArgs(part, parts, generation int, state, token, expect string, extra ...string) map[string]any {
	a := map[string]any{"parent_kind": "team", "parent_id": teamOne, "source_path": pubPath, "part_number": part, "part_count": parts, "generation": generation, "state": state, "publish_token": token, "expect_token": expect, "source_commit": pubCommit, "source_sha256": strings.Repeat("5", 64)}
	for i := 0; i+1 < len(extra); i += 2 {
		a[extra[i]] = extra[i+1]
	}
	return a
}

// A part is reserved pending, then recorded with the read-back digest, by
// compare-and-swap on its token; the parent must be one the project may
// publish to, and the path repository-relative.
func TestDocPublicationReserveRecordAndCAS(t *testing.T) {
	h := newHarness(t)
	get := func() map[string]any {
		return map[string]any{"parent_kind": "team", "parent_id": teamOne, "source_path": pubPath}
	}
	if e := h.errorOf("doc_publication_get", "", get()); !strings.Contains(e, "no task board binding") {
		t.Fatal("an unbound project read publications:", e)
	}
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0))
	if rows := h.call("doc_publication_get", "", get()).([]map[string]any); len(rows) != 0 {
		t.Fatal(rows)
	}
	h.call("doc_publication_put", "", pubArgs(1, 2, 1, "pending", "aaaaaaaaaaaa", ""))
	if e := h.errorOf("doc_publication_put", "", pubArgs(1, 2, 1, "pending", "bbbbbbbbbbbb", "")); !strings.Contains(e, "changed") {
		t.Fatal("a stale reserve won:", e)
	}
	digest := strings.Repeat("e", 64)
	h.call("doc_publication_put", "", pubArgs(1, 2, 1, "published", "aaaaaaaaaaaa", "aaaaaaaaaaaa", "document_id", seedDoc, "published_sha256", digest))
	h.call("doc_publication_put", "a", pubArgs(2, 2, 1, "pending", "cccccccccccc", ""))
	rows := h.call("doc_publication_get", "a", get()).([]map[string]any)
	if len(rows) != 2 || rows[0]["state"] != "published" || rows[0]["document_id"] != seedDoc || rows[0]["published_sha256"] != digest || rows[1]["state"] != "pending" {
		t.Fatal(rows)
	}
	if age, ok := rows[0]["age_seconds"].(int); !ok || age < 0 || age > 60 {
		t.Fatal("age", rows[0]["age_seconds"])
	}
	if acts := h.actions("linear_doc_publication"); len(acts) != 3 {
		t.Fatal("history", acts)
	}
	for name, bad := range map[string]map[string]any{
		"part over parts":     pubArgs(3, 2, 1, "pending", "dddddddddddd", ""),
		"too many parts":      pubArgs(1, 21, 1, "pending", "dddddddddddd", ""),
		"no generation":       pubArgs(1, 1, 0, "pending", "dddddddddddd", ""),
		"bad state":           pubArgs(1, 1, 1, "done", "dddddddddddd", ""),
		"bad token":           pubArgs(1, 1, 1, "pending", "XYZ", ""),
		"published sans doc":  pubArgs(1, 1, 1, "published", "dddddddddddd", ""),
		"pending with doc":    pubArgs(1, 1, 1, "pending", "dddddddddddd", "", "document_id", seedDoc),
		"absolute path":       pubArgs(1, 1, 1, "pending", "dddddddddddd", "", "source_path", "/home/me/plan.md"),
		"dot-dot path":        pubArgs(1, 1, 1, "pending", "dddddddddddd", "", "source_path", "docs/../../x.md"),
		"short commit":        pubArgs(1, 1, 1, "pending", "dddddddddddd", "", "source_commit", "0123456"),
		"another team":        pubArgs(1, 1, 1, "pending", "dddddddddddd", "", "parent_id", "22222222-2222-4222-8222-222222222222"),
		"issue parent":        pubArgs(1, 1, 1, "pending", "dddddddddddd", "", "parent_kind", "issue"),
		"unlisted initiative": pubArgs(1, 1, 1, "pending", "dddddddddddd", "", "parent_kind", "initiative", "parent_id", "3333aaaa-3333-4333-8333-3333333333a1"),
	} {
		if e := h.errorOf("doc_publication_put", "", bad); e == "" {
			t.Fatal(name, "accepted")
		}
	}
	// An initiative the policy lists is a parent the project may publish to.
	h.call("doc_policy_set", "", map[string]any{"expected_revision": 0, "doc_policy": dpPolicy()})
	initiative := pubArgs(1, 1, 1, "pending", "dddddddddddd", "", "parent_kind", "initiative", "parent_id", dpInitiative)
	h.call("doc_publication_put", "", initiative)
}

// A deny-pattern override is recorded in history with its operation ID,
// pattern IDs, lines, commit and path, never the matched text.
func TestDocPublishOverrideIsAudited(t *testing.T) {
	h := newHarness(t)
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0))
	args := map[string]any{"parent_kind": "team", "parent_id": teamOne, "source_path": pubPath, "operation_id": "publish-1", "pattern_ids": []any{"internal-name"}, "lines": []any{3, 9}, "source_commit": pubCommit}
	h.call("doc_publish_override", "", args)
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='linear_doc_publish_override' AND entity_key='publish-1'`).Scan(&details); e != nil || !strings.Contains(details, "internal-name") || !strings.Contains(details, pubCommit) || !strings.Contains(details, "[3,9]") {
		t.Fatal(details, e)
	}
	for name, edit := range map[string]func(a map[string]any){
		"no operation": func(a map[string]any) { delete(a, "operation_id") },
		"no patterns":  func(a map[string]any) { a["pattern_ids"] = []any{} },
		"bad line":     func(a map[string]any) { a["lines"] = []any{0} },
		"bad pattern":  func(a map[string]any) { a["pattern_ids"] = []any{"Bad ID"} },
	} {
		bad := map[string]any{}
		for k, v := range args {
			bad[k] = v
		}
		edit(bad)
		if e := h.errorOf("doc_publish_override", "", bad); e == "" {
			t.Fatal(name, "accepted")
		}
	}
}
