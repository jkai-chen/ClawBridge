package bus

import (
	"context"
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

// The server's history read is charged by the server alone, through
// ChargeLinearBudgetBy, as app_read.history under the caller: people
// without a session, agents with one. Any other by is refused, and no
// client can send the charge: linear_write_charge takes no read kind.
// (ChargeLinearBudgetBy's ledger is the hosted one: the Postgres tests
// charge through it end to end.)
func TestLinearReadChargeIsTheServers(t *testing.T) {
	h := newHarness(t)
	h.appChargeBy(2, "people", "app_read.history")
	h.appChargeBy(3, "agents", "app_read.history")
	if people, agents := h.split(); people != 2 || agents != 3 || h.budgetUsed() != 5 {
		t.Fatal(people, agents, h.budgetUsed())
	}
	ctx, pid := context.Background(), h.projectID()
	for _, by := range []string{"", "app", "People"} {
		if err := h.s.ChargeLinearBudgetBy(ctx, pid, 1, by, "app_read.history"); err == nil || !strings.Contains(err.Error(), "want people or agents") {
			t.Fatal("charged by", by, err)
		}
	}
	for _, session := range []string{"", "a"} {
		if e := h.errorOf("linear_write_charge", session, charge("r-"+session, "read", 1)); !strings.Contains(e, "kind must be one of") || strings.Contains(e, "read") {
			t.Fatal(session, e)
		}
	}
	if h.budgetUsed() != 5 {
		t.Fatal("a refused read charge counted:", h.budgetUsed())
	}
}
