package bus

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// linear_write_charge (FR8, live/FR8-PLAN.md r3): a write made through a
// person's own Linear profile counts against the project's hourly Linear
// count, like the app's: after its calls, to people without a session and
// to agents with one. It is never refused (the owner's rule for v0.3.9:
// no ClawBridge limit; Linear's own rate limit is respected through the
// pacer and the queues). The client's count is policy, not proof: the
// server raises it to a floor per kind and refuses one above
// linearWriteCallCeiling. Moves are charged by issue_move_reserve, and app
// writes by the server, so neither sends this.

// linearBudgetSchemaVersion is the hour's split: schema 50.
const linearBudgetSchemaVersion = 50

// linearBudgetSchema is the hour's split. The rest of budget_used is the
// app's.
const linearBudgetSchema = `
ALTER TABLE watch_lease ADD COLUMN budget_people INTEGER NOT NULL DEFAULT 0;
ALTER TABLE watch_lease ADD COLUMN budget_agents INTEGER NOT NULL DEFAULT 0;
`

// linearWriteCallFloor is the fewest calls each kind of write makes: the
// write, and its read-back where it always reads back.
var linearWriteCallFloor = map[string]int{
	"create": 2, "update": 2, "comment": 2, "relation": 2, "project_create": 2, "project_team_add": 2, "project_plan": 2,
	"document": 2, "project_lead": 1, "view": 1, "subscribe": 1, "admin": 1,
}

// linearWriteCallCeiling bounds one operation's count.
const linearWriteCallCeiling = 200

func linearBudgetOperation(op string) bool { return op == "linear_write_charge" }

func (s *Store) linearWriteCharge(ctx context.Context, tx *transaction, pid, projectRole string, actor map[string]any, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	if r.Session == "" && str(a, "instance") != "" {
		return nil, invalid("instance requires session")
	}
	if projectRole == "reader" {
		return nil, errors.New("project member permission required")
	}
	op := str(a, "operation_id")
	if !commentOperationID.MatchString(op) {
		return nil, invalid("operation_id must be 1–128 letters, digits, '.', '_', ':' or '-'")
	}
	kind := str(a, "kind")
	floor, ok := linearWriteCallFloor[kind]
	if !ok {
		return nil, invalid("kind must be one of create, update, comment, relation, project_create, project_team_add, project_plan, document, project_lead, view, subscribe or admin")
	}
	calls := 0
	if v, ok := a["calls"]; ok {
		n, err := integer(v)
		if err != nil {
			return nil, invalid("calls must be an integer")
		}
		calls = int(n)
	}
	if calls < 0 || calls > linearWriteCallCeiling {
		return nil, invalid(fmt.Sprintf("calls must be 0–%d", linearWriteCallCeiling))
	}
	count := max(calls, floor)
	budget, err := s.projectBudget(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	by := "people"
	if actor != nil {
		by = "agents"
	}
	if err = budget.chargeBy(ctx, tx, pid, count, by); err != nil {
		return nil, err
	}
	budget.used += count
	if by == "people" {
		budget.people += count
	} else {
		budget.agents += count
	}
	v := budget.view()
	return map[string]any{"operation_id": op, "charged": count, "used": v.Used, "limit": v.Limit, "capped": v.Capped, "window_resets_at": v.ResetsAt.Format(time.RFC3339), "by": v.By}, nil
}
