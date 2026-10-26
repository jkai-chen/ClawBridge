package bus

import (
	"context"
	"strings"
	"testing"
)

const (
	dpInitiative = "3333aaaa-3333-4333-8333-3333333333a1"
	dpOther      = "3333bbbb-3333-4333-8333-3333333333b2"
)

func dpPolicy() map[string]any {
	return map[string]any{
		"source_root":   "~/src/example",
		"deny_patterns": []any{map[string]any{"id": "internal-name", "regex": `internal-name-[0-9]+`}},
		"initiatives":   []any{dpInitiative},
	}
}

func TestDocPolicyRulesAndRevisions(t *testing.T) {
	h := newHarness(t)
	got := h.call("doc_policy_get", "a", map[string]any{}).(map[string]any)
	if got["revision"] != int64(0) || len(got["initiatives"].([]string)) != 0 || got["source_root"] != "" {
		t.Fatal("default policy", got)
	}
	bad := func(edit func(p map[string]any)) map[string]any {
		p := dpPolicy()
		edit(p)
		return p
	}
	for name, p := range map[string]map[string]any{
		"relative root":   bad(func(p map[string]any) { p["source_root"] = "src/example" }),
		"newline in root": bad(func(p map[string]any) { p["source_root"] = "/src/a\nb" }),
		"bad regex":       bad(func(p map[string]any) { p["deny_patterns"] = []any{map[string]any{"id": "x", "regex": "("}} }),
		"backreference":   bad(func(p map[string]any) { p["deny_patterns"] = []any{map[string]any{"id": "x", "regex": `(a)\1`}} }),
		"empty regex":     bad(func(p map[string]any) { p["deny_patterns"] = []any{map[string]any{"id": "x", "regex": ""}} }),
		"upper-case id":   bad(func(p map[string]any) { p["deny_patterns"] = []any{map[string]any{"id": "X", "regex": "a"}} }),
		"duplicate id": bad(func(p map[string]any) {
			p["deny_patterns"] = []any{map[string]any{"id": "x", "regex": "a"}, map[string]any{"id": "x", "regex": "b"}}
		}),
		"non-UUID":        bad(func(p map[string]any) { p["initiatives"] = []any{"I-12"} }),
		"upper-case UUID": bad(func(p map[string]any) { p["initiatives"] = []any{strings.ToUpper(dpInitiative)} }),
		"duplicate UUID":  bad(func(p map[string]any) { p["initiatives"] = []any{dpInitiative, dpInitiative} }),
		"unknown key":     bad(func(p map[string]any) { p["extra"] = true }),
		"pattern extra key": bad(func(p map[string]any) {
			p["deny_patterns"] = []any{map[string]any{"id": "x", "regex": "a", "flags": "i"}}
		}),
	} {
		if _, err := h.s.Execute(context.Background(), h.request("doc_policy_set", "c", map[string]any{"expected_revision": 0, "doc_policy": p})); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	set := h.call("doc_policy_set", "c", map[string]any{"expected_revision": 0, "doc_policy": dpPolicy()}).(map[string]any)
	if set["revision"] != int64(1) || set["source_root"] != "~/src/example" || len(set["deny_patterns"].([]DocDenyPattern)) != 1 {
		t.Fatal(set)
	}
	if _, err := h.s.Execute(context.Background(), h.request("doc_policy_set", "c", map[string]any{"expected_revision": 0, "doc_policy": dpPolicy()})); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatal("stale revision", err)
	}
	next := dpPolicy()
	next["initiatives"] = []any{dpInitiative, dpOther}
	if got := h.call("doc_policy_set", "c", map[string]any{"expected_revision": 1, "doc_policy": next}).(map[string]any); got["revision"] != int64(2) || len(got["initiatives"].([]string)) != 2 {
		t.Fatal(got)
	}
	// The audit records the shape of the change, never a regex.
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='linear_doc_policy' AND action='set' ORDER BY sequence DESC LIMIT 1`).Scan(&details); e != nil || strings.Contains(details, "internal-name") || !strings.Contains(details, dpOther) {
		t.Fatal("audit", details, e)
	}
	// Without a session, as the CLI sets it.
	if _, err := h.s.Execute(context.Background(), h.request("doc_policy_set", "", map[string]any{"expected_revision": 2, "doc_policy": dpPolicy()})); err != nil {
		t.Fatal("sessionless set", err)
	}
}
