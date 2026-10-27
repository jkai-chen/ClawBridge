package bus

import (
	"context"
	"fmt"
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
	// Recording is idempotent by operation ID.
	if again := h.call("linear_admin_audit", "a", audit).(map[string]any); again["already"] != true {
		t.Fatal("a repeated audit was recorded twice", again)
	}
	// A write checked under an older revision already happened in Linear: it
	// is recorded with both revisions, never refused.
	h.call("label_policy_set", "a", map[string]any{"expected_revision": 1, "label_policy": policy})
	late := map[string]any{"kind": "label_create", "outcome": "accepted", "operation_id": "op-late", "linear_team_id": policyTeam, "linear_id": policyLabel, "name": "Agent", "policy_revision": 1}
	h.call("linear_admin_audit", "a", late)
	if e := h.errorOf("linear_admin_audit", "a", map[string]any{"kind": "label_create", "outcome": "accepted", "operation_id": "op-x", "linear_team_id": policyTeam}); e == "" {
		t.Fatal("a label audit without its revision accepted")
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
	if acts := h.actions("linear_label_policy"); len(acts) != 2 || acts[0] != "set" {
		t.Fatal("policy history", acts)
	}
	if acts := h.actions("linear_label"); len(acts) != 2 || acts[0] != "label_create" {
		t.Fatal("label history", acts)
	}
	if acts := h.actions("linear_project"); len(acts) != 1 || acts[0] != "project_create" {
		t.Fatal("project history", acts)
	}
	events := h.call("history", "a", map[string]any{"entity": "linear_label", "limit": 10}).(map[string]any)["events"].([]map[string]any)
	byOp := map[string]map[string]any{}
	for _, e := range events {
		d := e["details"].(map[string]any)
		byOp[d["operation_id"].(string)] = d
	}
	if d := byOp["op-1"]; d["policy_revision"] == nil || d["name"] != "Bug" || d["policy_revision_now"] != nil {
		t.Fatal("audit details", d)
	}
	if d := byOp["op-late"]; fmt.Sprint(d["policy_revision"]) != "1" || fmt.Sprint(d["policy_revision_now"]) != "2" {
		t.Fatal("late audit details", d)
	}
}

// Schema 8 is applied to a schema-7 store and keeps its rows.
func TestLabelPolicySchemaEightMigratesAVersionSevenStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP INDEX history_entity; ALTER TABLE projects DROP COLUMN threads_from; ALTER TABLE taskboard_bindings DROP COLUMN names; ALTER TABLE workspaces DROP COLUMN device_idle_days; ALTER TABLE invites DROP COLUMN issued_device; ALTER TABLE projects DROP COLUMN watch_budget; DROP TABLE device_event_acks; DROP TABLE device_events; DROP TABLE dispatcher_asks; DROP TABLE dispatcher_principal_refs; DROP TABLE dispatcher_agent_status; DROP TABLE dispatcher_owners; DROP TABLE project_flow_nudges; DROP TABLE project_flow_moves; DROP TABLE project_flow_state; DROP TABLE context_notices; DROP TABLE context_watch_policies; ALTER TABLE sessions DROP COLUMN context_warn_tokens; ALTER TABLE sessions DROP COLUMN context_handover_tokens; ALTER TABLE sessions DROP COLUMN context_tokens; ALTER TABLE sessions DROP COLUMN context_window; ALTER TABLE sessions DROP COLUMN context_level; ALTER TABLE sessions DROP COLUMN context_compactions; ALTER TABLE sessions DROP COLUMN context_at; ALTER TABLE sessions DROP COLUMN context_backstop_at; ALTER TABLE sessions DROP COLUMN context_notified_at; ALTER TABLE sessions DROP COLUMN context_handover_at; ALTER TABLE sessions DROP COLUMN alive_until; ALTER TABLE sessions DROP COLUMN alive_state; DROP TABLE health_posts; DROP TABLE device_nonces; DROP TABLE owner_transfers; DROP TABLE invites; DROP TABLE devices; DROP TABLE dispatcher_launcher_tokens; DROP TABLE dispatcher_agent_credentials; DROP TABLE dispatcher_model_keys; DROP TABLE dispatcher_cases; DROP TABLE dispatcher_model_tokens; DROP TABLE dispatcher_releases; DROP TABLE dispatcher_escalations; DROP TABLE dispatcher_decisions; DROP TABLE dispatcher_policies; ALTER TABLE session_assignments DROP COLUMN issue_project_id; ALTER TABLE session_assignments DROP COLUMN issue_labels; DROP TABLE linear_doc_publications; DROP TABLE linear_doc_policies; DROP TABLE project_health; DROP TABLE linear_status_policy; ALTER TABLE assignment_markers DROP COLUMN state_state; ALTER TABLE assignment_markers DROP COLUMN state_policy_revision; ALTER TABLE assignment_markers DROP COLUMN state_target; ALTER TABLE assignment_markers DROP COLUMN state_landed; ALTER TABLE assignment_markers DROP COLUMN state_skip_reason; ALTER TABLE assignment_markers DROP COLUMN outcome; DROP TABLE linear_doc_seeds; DROP TABLE intake_policies; DROP TABLE intake_trusted; DROP TABLE intake_queue; DROP TABLE intake_tokens; DROP TABLE intake_counters; DROP TABLE watch_ticks; ALTER TABLE assignment_markers DROP COLUMN engineer; DROP TABLE linear_write_unsettled; DROP TABLE linear_write_ledger; DROP TABLE linear_app_tokens; DROP TABLE linear_label_policy; PRAGMA user_version=7;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, tables int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 34 {
		t.Fatal(version, err)
	}
	if err = s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='linear_label_policy'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatal("linear_label_policy missing after migration", tables, err)
	}
}

// Initiative writes are recorded without a team or a policy revision; adding
// a project names the project too.
func TestAdminAuditRecordsInitiatives(t *testing.T) {
	h := newHarness(t)
	const initiative = "00000000-0000-4000-8000-0000000000e6"
	const project = "00000000-0000-4000-8000-0000000000e7"
	h.call("linear_admin_audit", "a", map[string]any{"kind": "initiative_create", "outcome": "accepted", "operation_id": "i-1", "linear_id": initiative, "name": "Product Review"})
	h.call("linear_admin_audit", "a", map[string]any{"kind": "initiative_add_project", "outcome": "accepted", "operation_id": "i-2", "linear_id": initiative, "linear_project_id": project})
	for name, bad := range map[string]map[string]any{
		"no initiative":   {"kind": "initiative_create", "outcome": "accepted", "operation_id": "x"},
		"no project":      {"kind": "initiative_add_project", "outcome": "accepted", "operation_id": "x", "linear_id": initiative},
		"label sans team": {"kind": "label_create", "outcome": "accepted", "operation_id": "x", "policy_revision": 1},
	} {
		if e := h.errorOf("linear_admin_audit", "a", bad); e == "" {
			t.Fatal(name, "accepted")
		}
	}
	events := h.call("history", "a", map[string]any{"entity": "linear_initiative", "limit": 10}).(map[string]any)["events"].([]map[string]any)
	if len(events) != 2 {
		t.Fatal(events)
	}
	for _, e := range events {
		d := e["details"].(map[string]any)
		if e["action"] == "initiative_add_project" && d["linear_project_id"] != project {
			t.Fatal(d)
		}
		if d["policy_revision"] != nil {
			t.Fatal("an initiative audit carried a policy revision", d)
		}
	}
}

// The owner's admin commands run from the CLI with or without a session: a
// sessionless write records its audit and may set the list; an instance
// without a session, or a named session that is retired, is refused.
func TestAdminAuditAndPolicySetWithoutSession(t *testing.T) {
	h := newHarness(t)
	project := map[string]any{"kind": "project_create", "outcome": "accepted", "operation_id": "cb-rp-01", "linear_team_id": policyTeam, "linear_id": policyLabel, "name": "Review · One"}
	if got := h.call("linear_admin_audit", "", project).(map[string]any); got["recorded"] != true || got["already"] == true {
		t.Fatal("sessionless audit", got)
	}
	if again := h.call("linear_admin_audit", "", project).(map[string]any); again["already"] != true {
		t.Fatal("a repeated sessionless audit was recorded twice", again)
	}
	h.call("linear_admin_audit", "", map[string]any{"kind": "initiative_create", "outcome": "accepted", "operation_id": "cb-init-1", "linear_id": policyLabel, "name": "Product Review"})
	set := h.call("label_policy_set", "", map[string]any{"expected_revision": 0, "label_policy": map[string]any{"labels": []any{"Bug"}}}).(map[string]any)
	if set["revision"] != 1 {
		t.Fatal("sessionless policy set", set)
	}
	if e := h.errorOf("linear_admin_audit", "", map[string]any{"instance": "a-instance", "kind": "project_create", "outcome": "accepted", "operation_id": "x", "linear_team_id": policyTeam}); !strings.Contains(e, "instance requires session") {
		t.Fatal("instance without session accepted", e)
	}
	h.call("retire_session", "c", nil)
	if e := h.errorOf("linear_admin_audit", "c", map[string]any{"kind": "project_create", "outcome": "accepted", "operation_id": "y", "linear_team_id": policyTeam}); !strings.Contains(e, "retired") {
		t.Fatal("retired session recorded an audit", e)
	}
	if acts := h.actions("linear_project"); len(acts) != 1 {
		t.Fatal("project history", acts)
	}
	if acts := h.actions("linear_initiative"); len(acts) != 1 {
		t.Fatal("initiative history", acts)
	}
	// Other operations still need a session, and say so to the caller.
	_, err := h.s.Execute(context.Background(), h.request("assignment_propose", "", map[string]any{"issue_uuid": policyLabel}))
	if msg, ok := PublicMessage(err); !ok || !strings.Contains(msg, "session is required") {
		t.Fatal("sessionless refusal is not caller-facing", err)
	}
}
