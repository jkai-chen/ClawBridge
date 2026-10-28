package bus

import (
	"context"
	"errors"
	"fmt"

	"clawbridge/internal/principal"
)

// Lanes (CROSS-TEAM-BINDING-DESIGN §3.1): a binding's lanes filter which
// write teams a session writes. The project owner pins a session or a
// principal to a lane; a session's pin applies only while the session's
// principal is the one recorded at pin time (X1). A lane is a write filter,
// not a trust boundary: every listed team stays readable.
const lanePinsSchema = `
CREATE TABLE lane_pins(project_id TEXT NOT NULL REFERENCES projects(id),kind TEXT NOT NULL CHECK(kind IN ('session','principal')),pin_key TEXT NOT NULL,principal TEXT NOT NULL,lane TEXT NOT NULL,created_by TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,kind,pin_key));
CREATE TABLE lane_pins_revision(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0));
`

const (
	maxLanePins = 200
	// maxSuccessorHops bounds the walk from a session back through the
	// sessions it moved from.
	maxSuccessorHops = 16
)

func lanePinOperation(op string) bool {
	return op == "lane_pins_get" || op == "lane_pin_set" || op == "lane_pin_clear"
}

// lanePinsOp reads the pins (any member or session) or changes one (the
// project owner, without a session; on the hosted server only from a
// person's own device).
func (s *Store) lanePinsOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	if r.Session == "" && str(a, "instance") != "" {
		return nil, errors.New("instance requires session")
	}
	if r.Operation == "lane_pins_get" {
		return readLanePins(ctx, tx, pid)
	}
	if r.Session != "" {
		return nil, scoped("a session can't pin lanes; the project owner runs linear lanes pin in their own terminal", "forbidden")
	}
	if projectRole != "owner" {
		return nil, scoped("project owner permission required", "forbidden")
	}
	if s.postgres && (s.identity == nil || s.identity.Issuer != DeviceIssuer || principal.CI(s.principal)) {
		return nil, scoped("lane pins are set only from a person's own signed-in device", "forbidden")
	}
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid(r.Operation + " requires expected_revision")
	}
	revision, err := lanePinsRevision(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if revision != int64(expected) {
		return nil, conflict(fmt.Sprintf("the lane pins changed: current revision is %d", revision))
	}
	kind, key := str(a, "kind"), str(a, "key")
	if kind != "session" && kind != "principal" {
		return nil, invalid("kind is session or principal")
	}
	if kind == "principal" {
		// A local store's one principal isn't an email; sessions record it
		// as it is.
		switch n := principal.Normal(key); {
		case principal.Valid(n):
			key = n
		case !s.postgres && n == principal.Normal(s.principal):
			key = s.principal
		default:
			return nil, invalid("key must be a principal")
		}
	} else if !namePattern.MatchString(key) {
		return nil, invalid("key must be a session name")
	}
	details := map[string]any{"kind": kind}
	confirm, _ := a["confirm"].(bool)
	if r.Operation == "lane_pin_clear" && !confirm {
		rows, err := query(ctx, tx, `SELECT principal,lane FROM lane_pins WHERE project_id=? AND kind=? AND pin_key=?`, pid, kind, key)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, &serviceError{code: "not_found", cause: errors.New("the project has no such lane pin"), public: true}
		}
		return map[string]any{"preview": map[string]any{"action": "unpin", "kind": kind, "key": key, "principal": rows[0]["principal"], "lane": rows[0]["lane"]}, "revision": revision, "confirmation_required": true}, nil
	}
	if r.Operation == "lane_pin_clear" {
		result, err := tx.ExecContext(ctx, `DELETE FROM lane_pins WHERE project_id=? AND kind=? AND pin_key=?`, pid, kind, key)
		if err != nil {
			return nil, err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return nil, &serviceError{code: "not_found", cause: errors.New("the project has no such lane pin"), public: true}
		}
	} else {
		lane := str(a, "lane")
		b, linked, err := storedBinding(ctx, tx, pid)
		if err != nil {
			return nil, err
		}
		if !linked {
			return nil, invalid("the task board isn't linked; lanes belong to its binding")
		}
		if _, ok := b.Lane(lane); !ok {
			return nil, invalid("lane must be one of the binding's lanes")
		}
		// X1: a session pin records the session's principal now, so a
		// session another account later registers under the name doesn't
		// inherit it.
		owner := key
		if kind == "session" {
			row, err := one(ctx, tx, `SELECT owner_principal FROM sessions WHERE project_id=? AND name=? AND status='active'`, pid, key)
			if err != nil {
				return nil, scopedError(err, "the project has no active session of that name", "not_found")
			}
			owner = str(row, "owner_principal")
		}
		// The preview names the principal the pin binds, read here (X1).
		if !confirm {
			return map[string]any{"preview": map[string]any{"action": "pin", "kind": kind, "key": key, "principal": owner, "lane": lane}, "revision": revision, "confirmation_required": true}, nil
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lane_pins WHERE project_id=?`, pid).Scan(&count); err != nil {
			return nil, err
		}
		now, err := dbStamp(ctx, tx)
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE lane_pins SET principal=?,lane=?,created_by=?,created_at=? WHERE project_id=? AND kind=? AND pin_key=?`, owner, lane, s.principal, now, pid, kind, key)
		if err != nil {
			return nil, err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			if count >= maxLanePins {
				return nil, invalid(fmt.Sprintf("a project has at most %d lane pins", maxLanePins))
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO lane_pins(project_id,kind,pin_key,principal,lane,created_by,created_at) VALUES(?,?,?,?,?,?,?)`, pid, kind, key, owner, lane, s.principal, now); err != nil {
				return nil, err
			}
		}
		details["lane"] = lane
	}
	next := revision + 1
	var result interface{ RowsAffected() (int64, error) }
	if revision == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO lane_pins_revision(project_id,revision) VALUES(?,?) ON CONFLICT(project_id) DO NOTHING`, pid, next)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE lane_pins_revision SET revision=? WHERE project_id=? AND revision=?`, next, pid, revision)
	}
	if err != nil {
		return nil, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, conflict("the lane pins changed concurrently; read them again")
	}
	var pins int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lane_pins WHERE project_id=?`, pid).Scan(&pins); err != nil {
		return nil, err
	}
	// History by counts only: no session name or principal (T6).
	details["revision"], details["pins"] = next, pins
	if err = s.audit(ctx, tx, wid, pid, "", "lane_pins", pid, r.Operation, details); err != nil {
		return nil, err
	}
	return readLanePins(ctx, tx, pid)
}

func lanePinsRevision(ctx context.Context, tx *transaction, pid string) (int64, error) {
	rows, err := query(ctx, tx, `SELECT revision FROM lane_pins_revision WHERE project_id=?`, pid)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return int64(asInt(rows[0]["revision"])), nil
}

func readLanePins(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	revision, err := lanePinsRevision(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	rows, err := query(ctx, tx, `SELECT kind,pin_key,principal,lane,created_at FROM lane_pins WHERE project_id=? ORDER BY kind,pin_key`, pid)
	if err != nil {
		return nil, err
	}
	pins := []map[string]any{}
	for _, row := range rows {
		pins = append(pins, map[string]any{"kind": row["kind"], "key": row["pin_key"], "principal": row["principal"], "lane": row["lane"], "created_at": row["created_at"]})
	}
	return map[string]any{"revision": revision, "pins": pins}, nil
}

// sessionLane resolves a session's lane (or, for a sessionless caller,
// one with only owner_principal, its principal's): its own pin while its principal is
// the pin's, else a pin of a session it moved from under the same
// principal, else its principal's pin, else the binding's unpinned
// setting. ok is false when the binding has no lanes. A pin naming a lane
// the binding no longer has leaves the session read-only.
func (s *Store) sessionLane(ctx context.Context, tx *transaction, pid string, actor map[string]any) (lane string, teams []string, ok bool, err error) {
	b, _, err := storedBinding(ctx, tx, pid)
	var refused *serviceError
	if errors.As(err, &refused) {
		// An unreadable binding has no lanes; every write is refused
		// elsewhere (bindingUnreadable).
		return "", nil, false, nil
	}
	if err != nil || len(b.Lanes) == 0 {
		return "", nil, false, err
	}
	owner := str(actor, "owner_principal")
	pin := func(kind, key string) (string, bool, error) {
		rows, err := query(ctx, tx, `SELECT lane,principal FROM lane_pins WHERE project_id=? AND kind=? AND pin_key=?`, pid, kind, key)
		if err != nil || len(rows) == 0 || str(rows[0], "principal") != owner {
			return "", false, err
		}
		return str(rows[0], "lane"), true, nil
	}
	name, id := str(actor, "name"), str(actor, "id")
	found := false
	for hop := 0; hop <= maxSuccessorHops && !found; hop++ {
		if lane, found, err = pin("session", name); err != nil {
			return "", nil, false, err
		}
		if found {
			break
		}
		prev, err := query(ctx, tx, `SELECT id,name FROM sessions WHERE project_id=? AND successor=? AND status='moved' AND owner_principal=?`, pid, id, owner)
		if err != nil {
			return "", nil, false, err
		}
		if len(prev) != 1 {
			break
		}
		name, id = str(prev[0], "name"), str(prev[0], "id")
	}
	if !found {
		if lane, found, err = pin("principal", owner); err != nil {
			return "", nil, false, err
		}
	}
	if !found {
		lane = b.Unpinned
	}
	l, exists := b.Lane(lane)
	if !exists {
		return lane, []string{}, true, nil
	}
	return lane, append([]string{}, l.WriteTeamIDs...), true, nil
}
