package bus

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

const policyTeam = "00000000-0000-4000-8000-0000000000e1"
const policyLabel = "00000000-0000-4000-8000-0000000000e2"

func TestLabelPolicyParsesStrictly(t *testing.T) {
	ok := map[string]any{"labels": []any{"Bug", "agent-working"}, "renames": map[string]any{"Bugs": "Bug old"}}
	if p, err := ParseLabelPolicy(ok); err != nil || len(p.Labels) != 2 || p.Renames["Bugs"] != "Bug old" {
		t.Fatal(p, err)
	}
	if p, err := ParseLabelPolicy(map[string]any{}); err != nil || p.Labels == nil || p.Renames == nil {
		t.Fatal("an empty policy is valid and normalised", p, err)
	}
	long := strings.Repeat("x", maxPolicyName+1)
	many := []any{}
	for i := 0; i <= maxPolicyLabels; i++ {
		many = append(many, "l"+strings.Repeat("x", i%40)+string(rune('a'+i%26))+strings.Repeat("y", i/26))
	}
	for name, bad := range map[string]any{
		"extra key":          map[string]any{"labels": []any{}, "renames": map[string]any{}, "groups": []any{}},
		"not an array":       map[string]any{"labels": "Bug"},
		"non-string label":   map[string]any{"labels": []any{7}},
		"non-string rename":  map[string]any{"renames": map[string]any{"a": 1}},
		"duplicate any case": map[string]any{"labels": []any{"Bug", "bug"}},
		"too long":           map[string]any{"labels": []any{long}},
		"padded":             map[string]any{"labels": []any{" Bug"}},
		"empty":              map[string]any{"labels": []any{""}},
		"mention":            map[string]any{"labels": []any{"ask @ann"}},
		"mention link":       map[string]any{"labels": []any{"mention://x"}},
		"control":            map[string]any{"labels": []any{"a\tb"}},
		"rename to itself":   map[string]any{"renames": map[string]any{"Bug": "bug"}},
		"chained rename":     map[string]any{"renames": map[string]any{"A": "B", "B": "C"}},
		"shared target":      map[string]any{"renames": map[string]any{"A": "C", "B": "c"}},
		"too many labels":    map[string]any{"labels": many},
		"not an object":      []any{"Bug"},
	} {
		if _, err := ParseLabelPolicy(bad); err == nil {
			t.Error(name, "accepted")
		}
	}
	if _, err := ParseLabelPolicy(map[string]any{"labels": []any{"ops@example.com"}}); err != nil {
		t.Fatal("an email-shaped name is not a mention", err)
	}
}

// The owner sets the policy with a compare-and-swap; any member reads it;
// every change and every admin write is in history with the revision.
func TestLabelPolicySetGetAndAudit(t *testing.T) {
	h := newHarness(t)
	empty := h.call("label_policy_get", "a", nil).(map[string]any)
	if empty["revision"] != 0 || len(empty["labels"].([]string)) != 0 {
		t.Fatal("unset policy", empty)
	}
	policy := map[string]any{"labels": []any{"Bug", "Agent"}, "renames": map[string]any{"bugs": "Bug"}}
	set := h.call("label_policy_set", "a", map[string]any{"expected_revision": 0, "label_policy": policy}).(map[string]any)
	if set["revision"] != 1 || len(set["labels"].([]string)) != 2 || set["updated_by"] == nil || set["updated_at"] == nil {
		t.Fatal("set", set)
	}
	if e := h.errorOf("label_policy_set", "a", map[string]any{"expected_revision": 0, "label_policy": policy}); !strings.Contains(e, "current revision is 1") {
		t.Fatal("stale revision accepted", e)
	}
	if e := h.errorOf("label_policy_set", "a", map[string]any{"label_policy": policy}); e == "" {
		t.Fatal("set without expected_revision accepted")
	}
	if e := h.errorOf("label_policy_set", "a", map[string]any{"expected_revision": 1, "label_policy": map[string]any{"labels": []any{"x"}, "extra": true}}); e == "" {
		t.Fatal("loose policy accepted")
	}
	got := h.call("label_policy_get", "b", nil).(map[string]any)
	if got["revision"] != 1 || got["renames"].(map[string]string)["bugs"] != "Bug" {
		t.Fatal("read back", got)
	}
	audit := map[string]any{"kind": "label_create", "outcome": "accepted", "operation_id": "op-1", "linear_team_id": policyTeam, "linear_id": policyLabel, "name": "Bug", "policy_revision": 1}
	h.call("linear_admin_audit", "a", audit)
	audit["policy_revision"] = 2
	if e := h.errorOf("linear_admin_audit", "a", audit); !strings.Contains(e, "policy changed") {
		t.Fatal("audit against another revision accepted", e)
	}
	project := map[string]any{"kind": "project_create", "outcome": "observed_unchanged", "operation_id": "op-2", "linear_team_id": policyTeam, "name": "Q4"}
	h.call("linear_admin_audit", "a", project)
	for name, bad := range map[string]map[string]any{
		"kind":    {"kind": "label_delete", "outcome": "accepted", "operation_id": "x", "linear_team_id": policyTeam, "policy_revision": 1},
		"outcome": {"kind": "label_create", "outcome": "done", "operation_id": "x", "linear_team_id": policyTeam, "policy_revision": 1},
		"team":    {"kind": "label_create", "outcome": "accepted", "operation_id": "x", "linear_team_id": "FJA", "policy_revision": 1},
		"mention": {"kind": "label_create", "outcome": "accepted", "operation_id": "x", "linear_team_id": policyTeam, "name": "@ann", "policy_revision": 1},
	} {
		if e := h.errorOf("linear_admin_audit", "a", bad); e == "" {
			t.Fatal(name, "accepted")
		}
	}
	if acts := h.actions("linear_label_policy"); len(acts) != 1 || acts[0] != "set" {
		t.Fatal("policy history", acts)
	}
	if acts := h.actions("linear_label"); len(acts) != 1 || acts[0] != "label_create" {
		t.Fatal("label history", acts)
	}
	if acts := h.actions("linear_project"); len(acts) != 1 || acts[0] != "project_create" {
		t.Fatal("project history", acts)
	}
	events := h.call("history", "a", map[string]any{"entity": "linear_label", "limit": 10}).(map[string]any)["events"].([]map[string]any)
	if d := events[0]["details"].(map[string]any); d["policy_revision"] == nil || d["name"] != "Bug" {
		t.Fatal("audit details", d)
	}
}

// Schema 8 is applied to a schema-7 store and keeps its rows.
func TestLabelPolicySchemaEightMigratesAVersionSevenStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TABLE linear_label_policy; PRAGMA user_version=7;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, tables int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 8 {
		t.Fatal(version, err)
	}
	if err = s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='linear_label_policy'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatal("linear_label_policy missing after migration", tables, err)
	}
}
