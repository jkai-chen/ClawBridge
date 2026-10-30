package bus

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"strings"
)

// A marker's label or state part that may have reached Linear keeps the
// operation ID of each such attempt (live/MARKER-LEDGER-CLOSE-PLAN.md).
// When a later reread settles the part done or skipped, the earlier
// attempts' records in the server's operation ledger are acknowledged in
// the same transaction, so they are no longer listed as uncertain issue
// writes.

// markerOperationsSchemaVersion keeps a marker part's attempts.
const markerOperationsSchemaVersion = 53

// markerOperationsSchema is valid for SQLite and Postgres. The lists are
// JSON arrays of operation IDs, never Linear text; acknowledged_by says
// who acknowledged a ledger row: "" for a person (linear writes --ack),
// reread or reread_skipped for a marker's reread.
const markerOperationsSchema = `
ALTER TABLE assignment_markers ADD COLUMN state_operations TEXT NOT NULL DEFAULT '[]';
ALTER TABLE assignment_markers ADD COLUMN label_operations TEXT NOT NULL DEFAULT '[]';
ALTER TABLE linear_write_ledger ADD COLUMN acknowledged_by TEXT NOT NULL DEFAULT '';
`

// markerOperationsKept is how many attempts a part keeps, the newest. An
// older one dropped stays listed for a person to acknowledge; five
// uncertain attempts already mean the backoff ran long.
const markerOperationsKept = 5

// markerOperationID is the shape of a marker attempt's operation ID
// (linearboard.markerOperation).
var markerOperationID = regexp.MustCompile(`^marker-[0-9a-f]{16}$`)

// markerLedgerRecord is what binding an attempt reads of a shared ledger
// record: who wrote it for which issue and part, and its state.
type markerLedgerRecord struct {
	OperationID string `json:"operation_id"`
	Operation   string `json:"operation"`
	ProjectID   string `json:"project_id"`
	IssueID     string `json:"issue_id"`
	MarkerPart  string `json:"marker_part"`
}

// markerOperationsColumn is the part's list, or "" for the comment part,
// which its own comment ledger settles.
func markerOperationsColumn(part string) string {
	switch part {
	case "state":
		return "state_operations"
	case "label":
		return "label_operations"
	}
	return ""
}

// operations is the part's kept attempts, oldest first. A malformed list
// reads as empty.
func (m markerCall) operations() []string {
	var ops []string
	if json.Unmarshal([]byte(str(m.row, markerOperationsColumn(m.part))), &ops) != nil {
		return nil
	}
	out := ops[:0]
	for _, op := range ops {
		if markerOperationID.MatchString(op) {
			out = append(out, op)
		}
	}
	return out
}

// boundOperation returns the namespaces of op's ledger rows that are this
// marker part's own attempt: in this project, for this marker's issue,
// an update made by the same part. An older record names no issue and is
// never bound; nor is an ID of any other shape.
func (m markerCall) boundOperation(ctx context.Context, op string) ([]any, error) {
	if !markerOperationID.MatchString(op) {
		return nil, nil
	}
	rows, err := query(ctx, m.tx, `SELECT namespace,record FROM linear_write_ledger WHERE operation_id=? AND project_id=?`, op, m.pid)
	if err != nil {
		return nil, err
	}
	issue := str(m.row, "issue_uuid")
	var out []any
	for _, row := range rows {
		var r markerLedgerRecord
		if json.Unmarshal([]byte(str(row, "record")), &r) != nil {
			continue
		}
		if r.OperationID == op && r.ProjectID == m.pid && r.IssueID != "" && issue != "" && strings.EqualFold(r.IssueID, issue) && r.Operation == "update" && r.MarkerPart == m.part {
			out = append(out, row["namespace"])
		}
	}
	return out, nil
}

// ignoreOperation logs an operation_id a settle left out. It never fails
// the settle: the attempt's record stays for a person to acknowledge.
func (m markerCall) ignoreOperation(op, why string) {
	if len(op) > 128 {
		op = op[:128]
	}
	log.Printf("clawbridge: marker_settle ignored operation_id %q for %s %s/%s: %s", op, m.assignment, m.phase, m.part, why)
}

// keepOperation adds the settling attempt's operation_id, when bound, to
// the part's list, keeping the newest markerOperationsKept. It returns
// how many older ones it dropped.
func (m markerCall) keepOperation(ctx context.Context) (int, error) {
	op, column := str(m.a, "operation_id"), markerOperationsColumn(m.part)
	if op == "" || column == "" {
		return 0, nil
	}
	bound, err := m.boundOperation(ctx, op)
	if err != nil || len(bound) == 0 {
		if err == nil {
			m.ignoreOperation(op, "not this marker part's attempt in the server's ledger")
		}
		return 0, err
	}
	ops := m.operations()
	for _, kept := range ops {
		if kept == op {
			return 0, nil
		}
	}
	ops = append(ops, op)
	dropped := 0
	if len(ops) > markerOperationsKept {
		dropped = len(ops) - markerOperationsKept
		ops = ops[dropped:]
		log.Printf("clawbridge: marker_settle dropped %d older operation IDs of %s %s/%s; they stay listed for a person to acknowledge", dropped, m.assignment, m.phase, m.part)
	}
	_, err = m.tx.ExecContext(ctx, `UPDATE assignment_markers SET `+column+`=? WHERE assignment_id=? AND phase=?`, asJSON(ops), m.assignment, m.phase)
	return dropped, err
}

// closeOperations acknowledges, once a reread settled the part done
// (by "reread") or skipped ("reread_skipped"), the ledger records of its
// kept attempts and of the settling attempt's own operation_id, then
// clears the list. Each is bound again first. Only a record still
// pending (uncertain or still being sent) is acknowledged, in one
// conditional UPDATE: an outcome already recorded is left as it is. The
// record itself is never changed, so a late outcome from the server that
// sent it still replaces it (LedgerReplace compares the record only) and
// applied wins; an applied record is never listed anyway. Each
// acknowledged ID writes one audit row.
func (m markerCall) closeOperations(ctx context.Context, by string) (int, error) {
	column := markerOperationsColumn(m.part)
	if column == "" {
		return 0, nil
	}
	ops := m.operations()
	if op := str(m.a, "operation_id"); op != "" {
		own := true
		for _, kept := range ops {
			own = own && kept != op
		}
		if own {
			ops = append(ops, op)
		}
	}
	acknowledged := 0
	for _, op := range ops {
		bound, err := m.boundOperation(ctx, op)
		if err != nil {
			return 0, err
		}
		if len(bound) == 0 {
			m.ignoreOperation(op, "no longer this marker part's attempt in the server's ledger")
			continue
		}
		rows := 0
		for _, ns := range bound {
			r, err := m.tx.ExecContext(ctx, `UPDATE linear_write_ledger SET acknowledged_at=?,acknowledged_by=? WHERE namespace=? AND operation_id=? AND project_id=? AND pending=1 AND acknowledged_at=''`, layout(m.now), by, ns, op, m.pid)
			if err != nil {
				return 0, err
			}
			n, _ := r.RowsAffected()
			rows += int(n)
		}
		if rows == 0 {
			continue
		}
		acknowledged++
		if err = m.s.audit(ctx, m.tx, m.wid, m.pid, m.sid, "linear_write_ledger", op, "acknowledged", map[string]any{"by": by, "reason": "accepted_by_reread", "assignment_id": m.assignment, "phase": m.phase, "part": m.part, "operation_id": op, "rows": rows}); err != nil {
			return 0, err
		}
	}
	if str(m.row, column) != "[]" {
		if _, err := m.tx.ExecContext(ctx, `UPDATE assignment_markers SET `+column+`='[]' WHERE assignment_id=? AND phase=?`, m.assignment, m.phase); err != nil {
			return 0, err
		}
	}
	return acknowledged, nil
}

// settleOperations keeps or closes the part's attempts for a settle that
// left it in state, and adds what it did to out.
func (m markerCall) settleOperations(ctx context.Context, outcome, state string, out map[string]any) error {
	switch {
	case state == "done" || state == "skipped":
		by := "reread"
		if state == "skipped" {
			by = "reread_skipped"
		}
		n, err := m.closeOperations(ctx, by)
		if n > 0 {
			out["operations_acknowledged"] = n
		}
		return err
	case outcome == "uncertain" || outcome == "failed":
		n, err := m.keepOperation(ctx)
		if n > 0 {
			out["operations_dropped"] = n
		}
		return err
	}
	return nil
}
