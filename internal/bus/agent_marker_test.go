package bus

import (
	"context"
	"strings"
	"testing"
)

func (h *harness) historyCount() int {
	h.t.Helper()
	var n int
	if err := h.s.db.QueryRow(`SELECT count(*) FROM history`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

const markerRefusal = "this is a person's decision; run it in your own terminal, not from an agent session (the agent marker "

// Every person-only operation refuses a non-empty agent_marker before it runs,
// names the variable and never a value, and changes nothing. A marker is an
// accident guard, so the test also pins that a missing or empty one passes
// the same check.
func TestPersonOnlyOpsRefuseAMarker(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	before := h.historyCount()
	for _, op := range PersonOnlyOps() {
		if markerWhenPolicyChanges[op] {
			continue
		}
		_, err := h.s.Execute(context.Background(), h.request(op, "", map[string]any{"agent_marker": "CODEX_HOME"}))
		if err == nil || !strings.HasPrefix(err.Error(), markerRefusal+"CODEX_* is set)") {
			t.Errorf("%s with a marker: %v", op, err)
		}
		for _, bad := range []any{5, strings.Repeat("x", 65)} {
			if _, err = h.s.Execute(context.Background(), h.request(op, "", map[string]any{"agent_marker": bad})); err == nil || !strings.Contains(err.Error(), "agent_marker must be") {
				t.Errorf("%s with marker %v: %v", op, bad, err)
			}
		}
	}
	if after := h.historyCount(); after != before {
		t.Fatalf("a refused operation wrote history: %d rows became %d", before, after)
	}
	// Missing and empty both pass the check (they may fail for other reasons).
	for _, op := range PersonOnlyOps() {
		for _, a := range []map[string]any{{}, {"agent_marker": ""}} {
			if _, err := h.s.Execute(context.Background(), h.request(op, "", a)); err != nil && strings.Contains(err.Error(), markerRefusal) {
				t.Errorf("%s with marker %v refused: %v", op, a, err)
			}
		}
	}
}

func TestPersonOnlyOpsAreKnownOperations(t *testing.T) {
	for _, op := range PersonOnlyOps() {
		if _, ok := operationArgs[op]; !ok {
			t.Errorf("%s is person-only but not an operation", op)
		}
		if err := validateArgs(op, map[string]any{"agent_marker": ""}); err != nil && !strings.Contains(err.Error(), "required") {
			t.Errorf("%s does not take agent_marker: %v", op, err)
		}
	}
	// An operation outside the table still refuses the unknown argument.
	if err := validateArgs("taskboard_get_v2", map[string]any{"agent_marker": ""}); err == nil {
		t.Error("a read took agent_marker")
	}
}

// Closes, cancel kinds and project kind: a marker changes nothing; the same
// request without one is allowed as before.
func TestMarkerRefusedChangesNothingAndMissingIsAllowed(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true), "via": "cli-tty"})
	before, rows := h.storedPolicy(), h.historyCount()
	_, err := h.s.Execute(context.Background(), h.request("status_closes_set", "", map[string]any{"expected_revision": 1, "linear_project": cpOne, "closes": "agent", "via": "cli-tty", "agent_marker": "CLAUDECODE"}))
	if err == nil || !strings.Contains(err.Error(), markerRefusal+"CLAUDECODE is set)") {
		t.Fatal(err)
	}
	if after := h.storedPolicy(); len(after.Closes) != len(before.Closes) || h.historyCount() != rows {
		t.Fatal("a refused closes set changed the policy")
	}
	if _, err = h.s.Execute(context.Background(), h.request("status_closes_set", "", map[string]any{"expected_revision": 1, "linear_project": cpOne, "closes": "agent", "via": "cli-tty"})); err != nil {
		t.Fatal("without a marker:", err)
	}
	if _, err = h.s.Execute(context.Background(), h.request("project_kind_set", "", withMarker(kindSet(0, "", "", "Engineering"), "CODEX_SANDBOX"))); err == nil || !strings.Contains(err.Error(), "CODEX_* is set") {
		t.Fatal(err)
	}
	h.call("project_kind_set", "", withMarker(kindSet(0, "", "", "Engineering"), ""))
}

func withMarker(a map[string]any, marker string) map[string]any {
	a["agent_marker"] = marker
	return a
}

// A status policy refuses a marker only when it changes who closes or who
// may cancel; its other fields stay an owner session's to edit.
func TestStatusPolicyMarkerOnlyWhenClosesChange(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true), "via": "cli-tty"})
	same := sfPolicy(true)
	same["claim_from_triage"] = true
	h.call("status_policy_set", "", map[string]any{"expected_revision": 1, "status_policy": same, "via": "cli-tty", "agent_marker": "CLAUDECODE"})
	changed := sfPolicy(true)
	changed["closes"] = map[string]any{cpOne: "agent"}
	_, err := h.s.Execute(context.Background(), h.request("status_policy_set", "", map[string]any{"expected_revision": 2, "status_policy": changed, "via": "cli-tty", "agent_marker": "CLAUDECODE"}))
	if err == nil || !strings.Contains(err.Error(), markerRefusal+"CLAUDECODE is set)") {
		t.Fatal(err)
	}
	h.call("status_policy_set", "", map[string]any{"expected_revision": 2, "status_policy": changed, "via": "cli-tty"})
}

func TestRefuseAgentEchoesOnlyKnownNames(t *testing.T) {
	if msg := refuseAgent("SECRET_TOKEN_NAME").Error(); strings.Contains(msg, "SECRET_TOKEN_NAME") {
		t.Fatalf("refusal echoed an arbitrary name: %s", msg)
	}
	if msg := refuseAgent("CLAUDECODE").Error(); !strings.Contains(msg, "CLAUDECODE") {
		t.Fatalf("refusal lost a known name: %s", msg)
	}
}

// taskboard_lanes_set is person-only like the other binding writes: a marker
// is refused naming the variable and the lanes stay as they were, and the
// same request with none is allowed.
func TestLanesSetRefusesAMarker(t *testing.T) {
	h := newHarness(t)
	h.call("taskboard_set_v2", "", taskboardArgsLanes(0))
	before := h.historyCount()
	_, err := h.s.Execute(context.Background(), h.request("taskboard_lanes_set", "", map[string]any{"expected_revision": 1, "unpinned": "one", "agent_marker": "CLAUDECODE"}))
	if err == nil || !strings.Contains(err.Error(), markerRefusal+"CLAUDECODE is set)") {
		t.Fatal(err)
	}
	if h.historyCount() != before {
		t.Fatal("a refused lanes set wrote history")
	}
	h.call("taskboard_lanes_set", "", map[string]any{"expected_revision": 1, "unpinned": "one"})
}
