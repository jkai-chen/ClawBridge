package bus

import (
	"context"
	"errors"
	"fmt"
)

// The project's watch budget: the owner may set the comment watcher's
// hourly Linear budget for one project. It wins over the server's
// CLAWBRIDGE_WATCH_BUDGET and a client's --budget; NULL is no override.
// The lease keeps the budget its watcher asked for, so clearing the
// override falls back to it without a new lease.
//
// Schema 30: projects gains watch_budget.
const watchBudgetSchemaV30 = `
ALTER TABLE projects ADD COLUMN watch_budget INTEGER;
`

func watchBudgetOperation(op string) bool {
	return op == "comment_watch_budget_set"
}

// watchBudgetOverride is the project's override, if one is set.
func watchBudgetOverride(ctx context.Context, tx *transaction, pid string) (int, bool, error) {
	rows, err := query(ctx, tx, `SELECT watch_budget FROM projects WHERE id=?`, pid)
	if err != nil || len(rows) == 0 || rows[0]["watch_budget"] == nil {
		return 0, false, err
	}
	return asInt(rows[0]["watch_budget"]), true, nil
}

// LinearBudgetDefault is a project's hourly Linear calls when neither its
// owner nor the operator set one; agent writes count against it (owner
// decision Q4).
const LinearBudgetDefault = 600

// LinearBudgetCeiling is how far an owner may raise a project's cap when
// the operator sets no CLAWBRIDGE_LINEAR_BUDGET_MAX (R2).
const LinearBudgetCeiling = 2000

// SetLinearBudgetMax sets how far an owner may raise a project's cap; 0
// keeps LinearBudgetCeiling.
func (s *Store) SetLinearBudgetMax(n int) { s.linearBudgetMax = n }

func (s *Store) budgetCeiling() int {
	if s.linearBudgetMax > 0 {
		return s.linearBudgetMax
	}
	return LinearBudgetCeiling
}

// watchBudgetOp sets or clears the project's watch budget: the project
// owner, with or without a session, audited. Any member reads it with
// comment_watch_status.
func (s *Store) watchBudgetOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	wid, pid := w["id"].(string), p["id"].(string)
	if actor != nil && actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	if projectRole != "owner" {
		return nil, errors.New("project owner permission required")
	}
	v, ok := r.Args["watch_budget"]
	if !ok {
		return nil, invalid(fmt.Sprintf("%s requires watch_budget: 1–%d Linear calls per hour, or null to clear", r.Operation, s.budgetCeiling()))
	}
	var next any
	if v != nil {
		n, err := integer(v)
		if err != nil || n < 1 || n > s.budgetCeiling() {
			return nil, invalid(fmt.Sprintf("watch_budget must be 1–%d Linear calls per hour (this server's ceiling, CLAWBRIDGE_LINEAR_BUDGET_MAX), or null to clear", s.budgetCeiling()))
		}
		next = n
	}
	old, had, err := watchBudgetOverride(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	var prev any
	if had {
		prev = old
	}
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET watch_budget=? WHERE id=?`, next, pid); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, "", "watch_budget", pid, r.Operation, map[string]any{"old": prev, "new": next}); err != nil {
		return nil, err
	}
	return watchStatus(ctx, tx, pid)
}
