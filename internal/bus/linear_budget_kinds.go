package bus

import (
	"context"
	"time"
)

// The hour's Linear calls by kind (live/APP-CALL-ATTRIBUTION-PLAN.md): every
// charge names the job that made its calls, so an operator can say which
// job spent the app's hour. A row is a kind's name and two counts, never
// an issue or who.

// linearBudgetKindsSchemaVersion is the hour's calls by kind.
const linearBudgetKindsSchemaVersion = 52

// linearBudgetKindsSchema is valid for SQLite and Postgres. hour is
// budget_hour's: layout(now)[:13].
const linearBudgetKindsSchema = `
CREATE TABLE linear_budget_kinds(project_id TEXT NOT NULL,hour TEXT NOT NULL,kind TEXT NOT NULL,calls INTEGER NOT NULL DEFAULT 0,events INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(project_id,hour,kind));
`

// otherCallKind is where a charge with an unknown kind is counted: a
// programming error, or an older watcher's watch_charge.
const otherCallKind = "watch.other"

// appWriteCallOps are the app-route writes (server/appwrite.go's
// appWriteOperations); each is charged as app_write.<op>.
var appWriteCallOps = []string{"comment", "create", "update", "relation_add", "relation_remove"}

// linearCallKinds is the closed set of kinds a charge may name, so a typo
// can't make a new one.
var linearCallKinds = func() map[string]bool {
	out := map[string]bool{}
	for _, k := range []string{
		"watch.comments", "watch.markers", "watch.health", "watch.flow", "watch.docs", "watch.subscribers", "watch.intake", "watch.dispatcher", otherCallKind,
		"project_status", "issue_move", "operator.health",
	} {
		out[k] = true
	}
	for _, op := range appWriteCallOps {
		out["app_write."+op] = true
	}
	for kind := range linearWriteCallFloor {
		out["profile."+kind] = true
	}
	return out
}()

// KnownLinearCallKind reports whether kind is in the closed set.
func KnownLinearCallKind(kind string) bool { return linearCallKinds[kind] }

// callKind is kind when it is in the set, else watch.other.
func callKind(kind string) string {
	if linearCallKinds[kind] {
		return kind
	}
	return otherCallKind
}

// watchChargeKind is watch_charge's optional kind: one of the set's watch
// kinds, else watch.other (an older watcher sends none).
func watchChargeKind(v any) string {
	kind, _ := v.(string)
	if len(kind) > len("watch.") && kind[:len("watch.")] == "watch." && linearCallKinds[kind] {
		return kind
	}
	return otherCallKind
}

// recordCallKind adds a charge of calls to its kind's row for the hour, in
// the charge's own transaction. A read (0) records nothing; a refund
// (negative) lowers the calls, never below zero, and is no event. The
// project's rows older than the previous hour are dropped.
func recordCallKind(ctx context.Context, tx *transaction, pid, hour, kind string, calls int) error {
	if calls == 0 {
		return nil
	}
	kind = callKind(kind)
	var err error
	if calls > 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO linear_budget_kinds(project_id,hour,kind,calls,events) VALUES(?,?,?,?,1) ON CONFLICT(project_id,hour,kind) DO UPDATE SET calls=linear_budget_kinds.calls+excluded.calls,events=linear_budget_kinds.events+1`, pid, hour, kind, calls)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE linear_budget_kinds SET calls=CASE WHEN calls>? THEN calls-? ELSE 0 END WHERE project_id=? AND hour=? AND kind=?`, -calls, -calls, pid, hour, kind)
	}
	if err != nil {
		return err
	}
	if prev, ok := previousHour(hour); ok {
		_, err = tx.ExecContext(ctx, `DELETE FROM linear_budget_kinds WHERE project_id=? AND hour<?`, pid, prev)
	}
	return err
}

// hourLayout is budget_hour's shape: layout(now)[:13].
const hourLayout = "2006-01-02T15"

// previousHour is the hour before hour.
func previousHour(hour string) (string, bool) {
	t, err := time.Parse(hourLayout, hour)
	if err != nil {
		return "", false
	}
	return t.Add(-time.Hour).Format(hourLayout), true
}

// LinearBudgetKind is one kind's calls in an hour, and the charges that
// made them (events): calls/events is the cost of one.
type LinearBudgetKind struct {
	Kind   string `json:"kind"`
	Calls  int    `json:"calls"`
	Events int    `json:"events"`
}

// LinearBudgetHour is an ended hour's calls by kind.
type LinearBudgetHour struct {
	Hour  string             `json:"hour"`
	Kinds []LinearBudgetKind `json:"kinds"`
}

// budgetKinds reads a project's kinds in hour, the most calls first.
func budgetKinds(ctx context.Context, tx *transaction, pid, hour string) ([]LinearBudgetKind, error) {
	rows, err := query(ctx, tx, `SELECT kind,calls,events FROM linear_budget_kinds WHERE project_id=? AND hour=? ORDER BY calls DESC,kind`, pid, hour)
	if err != nil {
		return nil, err
	}
	var out []LinearBudgetKind
	for _, r := range rows {
		out = append(out, LinearBudgetKind{Kind: str(r, "kind"), Calls: asInt(r["calls"]), Events: asInt(r["events"])})
	}
	return out, nil
}

// budgetView is the budget's view with its kinds: this hour's, and the
// previous hour's when it has any.
func budgetView(ctx context.Context, tx *transaction, pid string, b projectBudget) (LinearBudget, error) {
	v := b.view()
	var err error
	if v.Kinds, err = budgetKinds(ctx, tx, pid, b.hour); err != nil {
		return v, err
	}
	if prev, ok := previousHour(b.hour); ok {
		kinds, err := budgetKinds(ctx, tx, pid, prev)
		if err != nil {
			return v, err
		}
		if len(kinds) > 0 {
			v.LastHour = &LinearBudgetHour{Hour: prev, Kinds: kinds}
		}
	}
	return v, nil
}

// LinearBudgetKindsAt reads a project's calls by kind in hour (budget_hour's
// shape), the most first: the watcher's hourly log line names the top
// ones. An hour older than the previous one has none left.
func (s *Store) LinearBudgetKindsAt(ctx context.Context, pid, hour string) ([]LinearBudgetKind, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return budgetKinds(ctx, tx, pid, hour)
}
