package bus

import (
	"strings"
	"testing"
	"time"
)

func (h *harness) split() (people, agents int) {
	h.t.Helper()
	if e := h.s.db.QueryRow(`SELECT COALESCE(SUM(budget_people),0),COALESCE(SUM(budget_agents),0) FROM watch_lease`).Scan(&people, &agents); e != nil {
		h.t.Fatal(e)
	}
	return people, agents
}

func charge(op, kind string, calls int) map[string]any {
	return map[string]any{"operation_id": op, "kind": kind, "calls": calls}
}

// FR8 r3: a write through a personal profile is counted after its calls,
// raised to its kind's floor, to people without a session and to agents
// with one, and never refused, cap or not; the overview splits the hour
// between the app, people and agents, and says when there is no cap.
func TestLinearWriteChargeCountsAndNeverRefuses(t *testing.T) {
	h := newHarness(t)
	if got := h.call("linear_write_charge", "", charge("p-1", "create", 1)).(map[string]any); got["charged"] != 2 || got["capped"] != false || got["limit"] != 0 {
		t.Fatal("under the floor, no cap:", got)
	}
	h.call("linear_write_charge", "a", charge("a-1", "update", 5))
	// An opt-in cap never refuses a personal write.
	h.call("comment_watch_budget_set", "", map[string]any{"watch_budget": 5})
	h.call("linear_write_charge", "a", charge("a-2", "view", 12))
	h.call("linear_write_charge", "", charge("p-2", "comment", 3))
	if h.budgetUsed() != 22 {
		t.Fatal("charged", h.budgetUsed())
	}
	if people, agents := h.split(); people != 5 || agents != 17 {
		t.Fatal(people, agents)
	}
	for _, bad := range []map[string]any{charge("p-4", "create", 201), charge("p-4", "move", 2), charge("", "create", 2), {"operation_id": "p-5", "kind": "create", "calls": 2, "admit": true}} {
		if e := h.errorOf("linear_write_charge", "", bad); !strings.Contains(e, "must be") && !strings.Contains(e, "unsupported argument") {
			t.Fatal(bad, e)
		}
	}
	// The app's calls are the rest of the hour.
	if _, e := h.s.db.Exec(`UPDATE watch_lease SET budget_used=budget_used+3`); e != nil {
		t.Fatal(e)
	}
	b := h.overview("")["linear_budget"].(LinearBudget)
	if b.By.App != 3 || b.By.People != 5 || b.By.Agents != 17 || !b.Capped || b.Limit != 5 {
		t.Fatal(b)
	}
	h.call("comment_watch_budget_set", "", map[string]any{"watch_budget": nil})
	if b = h.overview("")["linear_budget"].(LinearBudget); b.Capped || b.Limit != 0 || b.Source != "default" {
		t.Fatal("cleared:", b)
	}
}

// The split resets with the hour.
func TestLinearWriteChargeResetsWithTheHour(t *testing.T) {
	h := newHarness(t)
	h.call("linear_write_charge", "", charge("p-1", "create", 5))
	h.call("linear_write_charge", "a", charge("a-1", "create", 5))
	if _, e := h.s.db.Exec(`UPDATE watch_lease SET budget_hour=?`, layout(time.Now().Add(-time.Hour))[:13]); e != nil {
		t.Fatal(e)
	}
	h.call("linear_write_charge", "", charge("p-2", "view", 1))
	if people, agents := h.split(); people != 1 || agents != 0 || h.budgetUsed() != 1 {
		t.Fatal("the split outlived its hour:", people, agents, h.budgetUsed())
	}
}
