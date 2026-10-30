package bus

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/principal"
)

// writeLedgerScopeSchemaVersion is the schema that scopes the shared
// operation ledger's rows (PR-B).
const writeLedgerScopeSchemaVersion = 49

// Schema 49 (PR-B): the shared operation ledger's rows name their
// ClawBridge project and the principal whose write they record, so a
// project's uncertain issue writes can be listed, by that person or the
// project owner, and acknowledged. pending is 1 while the record is, so a
// listing reads the pending rows only, never the project's whole write
// history. Rows from before have no project and are never guessed into
// one.
const writeLedgerScopeSchema = `
ALTER TABLE linear_write_ledger ADD COLUMN project_id TEXT NOT NULL DEFAULT '';
ALTER TABLE linear_write_ledger ADD COLUMN principal TEXT NOT NULL DEFAULT '';
ALTER TABLE linear_write_ledger ADD COLUMN acknowledged_at TEXT NOT NULL DEFAULT '';
ALTER TABLE linear_write_ledger ADD COLUMN pending INTEGER NOT NULL DEFAULT 0;
CREATE INDEX linear_write_ledger_pending ON linear_write_ledger(project_id,pending,acknowledged_at);
`

// recordPending is 1 for a record whose state is pending: the column
// the listings filter on.
func recordPending(record string) int {
	var r ledgerPending
	if json.Unmarshal([]byte(record), &r) == nil && r.State == "pending" {
		return 1
	}
	return 0
}

// ledgerInProgressFor mirrors the client's inProgressFor: a pending record
// with no reason younger than this is still being sent, not uncertain.
const ledgerInProgressFor = 75 * time.Second

// ledgerInProgress reports a pending row still being sent.
func ledgerInProgress(row map[string]any, now time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, str(row, "updated_at"))
	return str(row, "reason") == "" && err == nil && now.Sub(at) < ledgerInProgressFor
}

// writeLedgerPage caps one listing of the server's uncertain issue writes.
const writeLedgerPage = 100

func writeLedgerOperation(op string) bool {
	return op == "issue_writes_uncertain" || op == "issue_writes_ack"
}

// ledgerPending reads a shared ledger record as the client writes it: a
// pending record names its operation and, when uncertain, its reason.
type ledgerPending struct {
	State     string `json:"state"`
	Operation string `json:"operation"`
	Reason    string `json:"reason"`
	UpdatedAt string `json:"updated_at"`
}

// pendingRows reads the project's unacknowledged pending rows the caller
// may see: the owner sees every one, anyone else only their own.
func (s *Store) pendingRows(ctx context.Context, tx *transaction, pid, projectRole, op string) ([]map[string]any, error) {
	q := `SELECT namespace,operation_id,record FROM linear_write_ledger WHERE project_id=? AND pending=1 AND acknowledged_at=''`
	args := []any{pid}
	if op != "" {
		q += ` AND operation_id=?`
		args = append(args, op)
	}
	if projectRole != "owner" {
		q += ` AND principal=? AND principal<>''`
		args = append(args, s.principal)
	}
	rows, err := query(ctx, tx, q+` ORDER BY operation_id,namespace`, args...)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, row := range rows {
		var r ledgerPending
		if json.Unmarshal([]byte(str(row, "record")), &r) != nil || r.State != "pending" {
			continue
		}
		row["operation"], row["reason"], row["updated_at"] = r.Operation, r.Reason, r.UpdatedAt
		out = append(out, row)
	}
	return out, nil
}

func (s *Store) writeLedgerOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	op := str(a, "operation_id")
	if op != "" && !ledgerKeyID(op) {
		return nil, invalid("operation_id must be 1–128 letters, digits, '.', '_' or '-'")
	}
	switch r.Operation {
	case "issue_writes_uncertain":
		offset := 0
		if c := str(a, "cursor"); c != "" {
			n, err := strconv.Atoi(c)
			if err != nil || n < 0 {
				return nil, invalid("cursor must be a listing's next_cursor")
			}
			offset = n
		}
		rows, err := s.pendingRows(ctx, tx, pid, projectRole, op)
		if err != nil {
			return nil, err
		}
		writes := []map[string]any{}
		for i := offset; i < len(rows) && len(writes) < writeLedgerPage; i++ {
			row := rows[i]
			writes = append(writes, map[string]any{"operation_id": row["operation_id"], "operation": row["operation"], "reason": row["reason"], "updated_at": row["updated_at"]})
		}
		out := map[string]any{"writes": writes}
		if offset+len(writes) < len(rows) {
			out["next_cursor"] = strconv.Itoa(offset + len(writes))
		}
		return out, nil
	}
	// issue_writes_ack: the row's own person or the project owner,
	// from their own signed-in device. It hides the row only: the record
	// stays pending, so a retry of the same operation ID sends nothing.
	if r.Session != "" {
		return nil, scoped("a session can't acknowledge an issue write; the person does, in their own terminal", "forbidden")
	}
	if s.postgres && (s.identity == nil || s.identity.Issuer != DeviceIssuer || principal.CI(s.principal)) {
		return nil, scoped("an issue write is acknowledged only from a person's own signed-in device", "forbidden")
	}
	if op == "" {
		return nil, invalid("operation_id is required")
	}
	rows, err := s.pendingRows(ctx, tx, pid, projectRole, op)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, &serviceError{code: "not_found", cause: errors.New("the project has no unacknowledged uncertain issue write of yours with that operation_id"), public: true}
	}
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if ledgerInProgress(row, at(nowRaw)) {
			return nil, &serviceError{code: "conflict", cause: errors.New("that issue write is still being sent; its outcome comes in a moment, so nothing was acknowledged"), public: true}
		}
	}
	for _, row := range rows {
		if _, err = tx.ExecContext(ctx, `UPDATE linear_write_ledger SET acknowledged_at=? WHERE namespace=? AND operation_id=? AND acknowledged_at=''`, layout(at(nowRaw)), row["namespace"], op); err != nil {
			return nil, err
		}
	}
	if err = s.audit(ctx, tx, wid, pid, "", "linear_write_ledger", op, "acknowledged", map[string]any{"rows": len(rows)}); err != nil {
		return nil, err
	}
	return map[string]any{"operation_id": op, "acknowledged": len(rows)}, nil
}

// uncertainIssueWrites counts the project's uncertain issue writes the
// caller may see, leaving out those still being sent.
func (s *Store) uncertainIssueWrites(ctx context.Context, tx *transaction, pid, projectRole string, now time.Time) (int, error) {
	rows, err := s.pendingRows(ctx, tx, pid, projectRole, "")
	n := 0
	for _, row := range rows {
		if !ledgerInProgress(row, now) {
			n++
		}
	}
	return n, err
}

// validLedgerScope checks the scope a row is recorded under: a project ID
// and a principal, each empty or short and printable.
func validLedgerScope(project, who string) error {
	for _, v := range []string{project, who} {
		if len(v) > 320 || strings.ContainsAny(v, "\x00\n\r") {
			return invalid("invalid operation ledger scope")
		}
	}
	return nil
}
