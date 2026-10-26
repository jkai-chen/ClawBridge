package bus

import (
	"context"
	"time"
)

// presenceSchemaV20 adds a session's liveness lease (dispatcher step 4b):
// the process holding the session renews alive_until, and may say whether
// its agent is working or idle. alive_state is the client's claim, used for
// ordering only, never for a grant.
const presenceSchemaV20 = `
ALTER TABLE sessions ADD COLUMN alive_until TEXT;
ALTER TABLE sessions ADD COLUMN alive_state TEXT CHECK(alive_state IN ('working','idle'));
`

const (
	// leaseTTL is how long one heartbeat keeps a session alive; clients
	// renew every 30 s.
	leaseTTL = 90 * time.Second
	// leaseRenewable is the remaining lease below which a heartbeat writes:
	// at most one write per 20 s, enforced in the database.
	leaseRenewable = 70 * time.Second
)

// lease is the heartbeat a pure read leaves for after its commit.
type lease struct{ workspace, project, session, instance, state string }

// heartbeatOp verifies that the caller holds the session (its current
// instance, active, the caller's own) and leaves its lease to be renewed.
// It never touches last_seen: a heartbeat is liveness, not activity.
func (s *Store) heartbeatOp(ctx context.Context, tx *transaction, pid string, r Request) (any, error) {
	state, instance := str(r.Args, "state"), str(r.Args, "instance")
	if state != "" && state != "working" && state != "idle" {
		return nil, invalid("state is working or idle")
	}
	if r.Session == "" || instance == "" {
		return nil, invalid("heartbeat needs the session and its instance")
	}
	match, args := sessionAddress(r.Session)
	actor, err := one(ctx, tx, `SELECT id,status FROM sessions WHERE project_id=? AND `+match+` AND instance=? AND owner_principal=?`, append(append([]any{pid}, args...), instance, s.principal)...)
	if err != nil {
		return nil, scopedError(err, "session instance mismatch; pass the instance returned by register", "forbidden")
	}
	if actor["status"] != "active" {
		return nil, invalid("the session is retired or moved; it holds no lease")
	}
	l := &lease{tx.workspace, pid, actor["id"].(string), instance, state}
	if tx.pure {
		tx.lease = l
		return map[string]any{"lease_seconds": int(leaseTTL / time.Second)}, nil
	}
	// A local store has no lock queue to stay out of: renew in place.
	until, renewable := leaseTimes()
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET alive_until=?,alive_state=? WHERE project_id=? AND id=? AND instance=? AND status='active' AND (alive_until IS NULL OR alive_until<?)`, until, nullable(state), pid, l.session, instance, renewable)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	return map[string]any{"lease_seconds": int(leaseTTL / time.Second), "renewed": n == 1}, nil
}

// renewLease records a heartbeat's lease after its read has committed, on
// the beat's light path. A lease with more than leaseRenewable left is not
// written again.
func (s *Store) renewLease(ctx context.Context, r Request, l *lease) bool {
	until, renewable := leaseTimes()
	return s.sessionWrite(ctx, r.Operation, l.workspace, `UPDATE sessions SET alive_until=?,alive_state=? FROM workspaces w WHERE sessions.project_id=? AND sessions.id=? AND sessions.instance=? AND sessions.status='active' AND (sessions.alive_until IS NULL OR sessions.alive_until<?)`, []any{until, nullable(l.state), l.project, l.session, l.instance, renewable})
}

func leaseTimes() (until, renewable string) {
	now := hostClock().UTC()
	return now.Add(leaseTTL).Format(timestampLayout), now.Add(leaseRenewable).Format(timestampLayout)
}

// liveness derives a session's presence for the dispatcher (§1) from its
// lease, its last bus call and its status:
//   - working: the lease is live and the client says working, or the
//     session made a bus call within staleAfter;
//   - idle: the lease is live and the session is quiet;
//   - asleep: the lease expired (a working claim with it too) and the last
//     call is within staleAfterHours;
//   - gone: retired or moved, or nothing for staleAfterHours.
//
// A session that never sent a heartbeat (an old client) keeps today's rule:
// active is working, stale is asleep, never gone; §7 judges it by last_seen
// as before.
func liveness(m map[string]any, now time.Time, staleAfterHours int) string {
	if m["status"] != "active" {
		return "gone"
	}
	seen, err := time.Parse(timestampLayout, str(m, "last_seen"))
	if err != nil {
		return "asleep"
	}
	quiet := now.Sub(seen) > staleAfter
	until, err := time.Parse(timestampLayout, str(m, "alive_until"))
	switch {
	case err != nil && quiet:
		return "asleep"
	case err != nil:
		return "working"
	case until.After(now) && (!quiet || str(m, "alive_state") == "working"):
		return "working"
	case until.After(now):
		return "idle"
	case now.Sub(seen) > time.Duration(staleAfterHours)*time.Hour:
		return "gone"
	}
	return "asleep"
}
