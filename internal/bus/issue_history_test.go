package bus

import (
	"strings"
	"testing"
)

// A local store has no Linear app: an issue's history is refused in the
// server's words, from a session or the person, after the usual checks.
func TestLocalStoreRefusesIssueHistory(t *testing.T) {
	h := newHarness(t)
	issue := map[string]any{"issue_uuid": "d2100000-0000-4000-8000-000000000001", "limit": 20}
	for _, session := range []string{"", "a"} {
		if e := h.errorOf("linear_issue_history", session, issue); e != IssueHistoryAppUnavailable {
			t.Fatal(session, e)
		}
	}
	if e := h.errorOf("linear_issue_history", "", map[string]any{"issue_uuid": "x", "team": "y"}); !strings.Contains(e, "unsupported argument") {
		t.Fatal(e)
	}
	if e := h.errorOf("linear_issue_history", "", map[string]any{"limit": "many"}); !strings.Contains(e, "limit") {
		t.Fatal(e)
	}
}

// The server's history read is charged as kind read, under the caller:
// people without a session, agents with one.
func TestLinearReadChargeCountsPerCaller(t *testing.T) {
	h := newHarness(t)
	if got := h.call("linear_write_charge", "", charge("r-1", "read", 0)).(map[string]any); got["charged"] != 1 {
		t.Fatal("under the floor:", got)
	}
	h.call("linear_write_charge", "a", charge("r-2", "read", 3))
	if people, agents := h.split(); people != 1 || agents != 3 || h.budgetUsed() != 4 {
		t.Fatal(people, agents, h.budgetUsed())
	}
	// A read charges at most 6 calls (3 queries with re-sends): a direct
	// charge adds no more than a history read would, and more is refused
	// with nothing counted. Writes keep their own ceiling.
	h.call("linear_write_charge", "", charge("r-3", "read", 6))
	if e := h.errorOf("linear_write_charge", "", charge("r-4", "read", 7)); !strings.Contains(e, "a read charges 0–6 calls") {
		t.Fatal(e)
	}
	if people, _ := h.split(); people != 7 || h.budgetUsed() != 10 {
		t.Fatal(people, h.budgetUsed())
	}
	h.call("linear_write_charge", "", charge("w-1", "comment", 7))
}
