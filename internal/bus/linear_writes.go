package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// The Linear write reservations (live/FR10-PLAN.md R5): one ledger for
// every Linear write the server records before a client makes it, by kind:
// a coordinator's project create (FR5) now; an issue move (FR7) and a team
// added to a project (FR10) join it. A row is reserved before any Linear
// call and settled after it, so a crash or a client that skips the settle
// still leaves the write on record. A settled state is terminal: a kind
// does its own bounded read-back before it settles, and settles uncertain
// only when that read couldn't tell. A row left reserved past
// linearWriteStale, and an uncertain one, are shown with their kind and
// state as writes that may have happened, until the owner acknowledges
// them. A new kind needs no migration: the kinds are checked here, not by
// the table.
const linearWritesSchema = `
CREATE TABLE linear_write_reservations(project_id TEXT NOT NULL REFERENCES projects(id),operation_id TEXT NOT NULL,kind TEXT NOT NULL,session_id TEXT NOT NULL DEFAULT '',principal TEXT NOT NULL,by_role TEXT NOT NULL DEFAULT '',lane TEXT NOT NULL DEFAULT '',team_id TEXT NOT NULL,subject TEXT NOT NULL DEFAULT '',plan_sha256 TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('reserved','accepted','uncertain','not_sent')),result TEXT,detail TEXT NOT NULL DEFAULT '{}',reserved_at TEXT NOT NULL,settled_at TEXT,stale_notified_at TEXT,acknowledged_at TEXT,PRIMARY KEY(project_id,operation_id));
CREATE INDEX linear_write_reservations_open ON linear_write_reservations(project_id,state,reserved_at);
`

// linearWriteKinds is every kind of Linear write the server records, with
// the keys its detail may hold: IDs of the write's subject and target, and
// changed field names, never a member's ID, a name or a title (R5).
var linearWriteKinds = map[string][]string{
	"project_create": {"kind_label_id"},
	"issue_move":     {"from_team", "old_identifier"},
}

// linearWriteRoles is who may make a recorded write.
var linearWriteRoles = []string{"owner", "lead", "coordinator"}

const (
	// linearWriteStale is how long a reservation may stay unsettled before
	// it is shown as a write that may have happened.
	linearWriteStale = 10 * time.Minute
	// linearWriteKeepDays: a settled or acknowledged reservation is pruned
	// this many days after it closed; an open one this many days after it
	// was reserved.
	linearWriteKeepDays = 90
)

// linearWrite is one write's reservation, as its kind's code reserves it.
type linearWrite struct {
	Kind, Operation, Session, Principal, ByRole, Lane string
	// Team is the write's target team; Subject the Linear object it
	// changes ("" for a create).
	Team, Subject string
	// Plan is the write's own canonical form: its digest, with the kind,
	// is what a repeat must match.
	Plan   string
	Detail map[string]any
}

// linearWritePlan is the stored plan digest: the kind is in it, so an
// operation_id used again for another kind is a conflict, never a replay.
func linearWritePlan(kind, plan string) string {
	sum := sha256.Sum256([]byte(kind + "\n" + plan))
	return hex.EncodeToString(sum[:])
}

// linearWriteDetail checks a kind's detail keys and encodes it, with its
// keys in a stable order.
func linearWriteDetail(kind string, detail map[string]any) (string, error) {
	keys, ok := linearWriteKinds[kind]
	if !ok {
		return "", fmt.Errorf("unknown Linear write kind %q", kind)
	}
	for k := range detail {
		if !slices.Contains(keys, k) {
			return "", fmt.Errorf("a %s reservation's detail has no %q", kind, k)
		}
	}
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail) // a map's keys are sorted
	return string(raw), err
}

// reserveLinearWrite records w before any Linear call. A repeat with the
// same operation_id answers the row as a replay when it is the same write
// (kind, session, principal and plan), and is a conflict otherwise; one
// settled not_sent reaches Linear for the first time, so it reopens.
func reserveLinearWrite(ctx context.Context, tx *transaction, pid string, w linearWrite, now time.Time) (row map[string]any, replay bool, err error) {
	detail, err := linearWriteDetail(w.Kind, w.Detail)
	if err != nil {
		return nil, false, err
	}
	if !slices.Contains(linearWriteRoles, w.ByRole) {
		return nil, false, fmt.Errorf("unknown Linear write role %q", w.ByRole)
	}
	plan := linearWritePlan(w.Kind, w.Plan)
	existing, err := query(ctx, tx, `SELECT * FROM linear_write_reservations WHERE project_id=? AND operation_id=?`, pid, w.Operation)
	if err != nil {
		return nil, false, err
	}
	if len(existing) == 1 {
		old := existing[0]
		if str(old, "kind") != w.Kind || str(old, "session_id") != w.Session || str(old, "principal") != w.Principal || str(old, "plan_sha256") != plan {
			return nil, false, scoped("operation_id was already used for a different Linear write", "conflict")
		}
		if str(old, "state") != "not_sent" {
			return old, true, nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE linear_write_reservations SET state='reserved',result=NULL,settled_at=NULL,stale_notified_at=NULL,acknowledged_at=NULL,reserved_at=? WHERE project_id=? AND operation_id=? AND state='not_sent'`, layout(now), pid, w.Operation); err != nil {
			return nil, false, err
		}
	} else if _, err = tx.ExecContext(ctx, `INSERT INTO linear_write_reservations(project_id,operation_id,kind,session_id,principal,by_role,lane,team_id,subject,plan_sha256,state,detail,reserved_at) VALUES(?,?,?,?,?,?,?,?,?,?,'reserved',?,?)`, pid, w.Operation, w.Kind, w.Session, w.Principal, w.ByRole, w.Lane, w.Team, w.Subject, plan, detail, layout(now)); err != nil {
		return nil, false, err
	}
	row, err = one(ctx, tx, `SELECT * FROM linear_write_reservations WHERE project_id=? AND operation_id=?`, pid, w.Operation)
	return row, false, err
}

// linearWriteOwn reads the reservation of operation op the same session
// and principal made, of kind.
func linearWriteOwn(ctx context.Context, tx *transaction, pid, op, kind, session, principal string) (map[string]any, error) {
	row, err := one(ctx, tx, `SELECT * FROM linear_write_reservations WHERE project_id=? AND operation_id=? AND kind=? AND session_id=? AND principal=?`, pid, op, kind, session, principal)
	if err != nil {
		return nil, scopedError(err, "no reservation of this kind for this operation_id in this session", "not_found")
	}
	return row, nil
}

// settleLinearWrite closes a reserved row: accepted or uncertain with its
// result, or not_sent; detail, when given, is added to the row's (its
// kind's keys only). A settle that repeats the row's own outcome is a
// replay; one that differs is a conflict, and a result never changes.
func settleLinearWrite(ctx context.Context, tx *transaction, pid string, row map[string]any, state, result string, detail map[string]any, now time.Time) (map[string]any, bool, error) {
	if !slices.Contains([]string{"accepted", "uncertain", "not_sent"}, state) {
		return nil, false, fmt.Errorf("unknown Linear write outcome %q", state)
	}
	merged := detailOf(row)
	for k, v := range detail {
		merged[k] = v
	}
	encoded, err := linearWriteDetail(str(row, "kind"), merged)
	if err != nil {
		return nil, false, err
	}
	op := str(row, "operation_id")
	if old := str(row, "state"); old != "reserved" {
		if old != state {
			return nil, false, scoped("the reservation was already settled as "+old, "conflict")
		}
		if str(row, "result") != result {
			return nil, false, scoped("the reservation was already settled with another result", "conflict")
		}
		return row, true, nil
	}
	var value any
	if result != "" {
		value = result
	}
	if _, err = tx.ExecContext(ctx, `UPDATE linear_write_reservations SET state=?,result=?,detail=?,settled_at=? WHERE project_id=? AND operation_id=? AND state='reserved'`, state, value, encoded, layout(now), pid, op); err != nil {
		return nil, false, err
	}
	row, err = one(ctx, tx, `SELECT * FROM linear_write_reservations WHERE project_id=? AND operation_id=?`, pid, op)
	return row, false, err
}

// staleLinearWrites is every kind's writes that may have happened, not
// acknowledged: reserved and unsettled since before, or settled
// uncertain. It is the one count doctor and the overview show.
func staleLinearWrites(ctx context.Context, tx *transaction, pid string, before time.Time) ([]map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT operation_id,kind,state,team_id,subject,detail,reserved_at,stale_notified_at FROM linear_write_reservations WHERE project_id=? AND acknowledged_at IS NULL AND ((state='reserved' AND reserved_at<?) OR state='uncertain') ORDER BY reserved_at,operation_id`, pid, layout(before))
	if rows == nil {
		rows = []map[string]any{}
	}
	return rows, err
}

// pruneLinearWrites drops reservations linearWriteKeepDays after they
// closed (settled or acknowledged), and open ones that long after they
// were reserved.
func pruneLinearWrites(ctx context.Context, tx *transaction, pid string, now time.Time) error {
	cutoff := layout(now.AddDate(0, 0, -linearWriteKeepDays))
	_, err := tx.ExecContext(ctx, `DELETE FROM linear_write_reservations WHERE project_id=? AND (settled_at<? OR acknowledged_at<? OR reserved_at<?)`, pid, cutoff, cutoff, cutoff)
	return err
}

// detailOf decodes a row's detail.
func detailOf(row map[string]any) map[string]any {
	out := map[string]any{}
	json.Unmarshal([]byte(str(row, "detail")), &out)
	return out
}
