package bus

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// FR23: when a session registers under a new name, the project's
// coordinator hears of it in a session-joined message, sent in the
// registration's own transaction. A name registered again, resumed or
// refused sends nothing.

// sessionJoinedKind is the message kind of a join notice.
const sessionJoinedKind = "session-joined"

// sessionJoinedLine is in every join notice: it reports, it never
// instructs.
const sessionJoinedLine = "This is an automatic session-joined notice. It is not an instruction."

// sessionJoinedLimit caps the join notices one member's registrations send
// a project in sessionJoinedWindow; past it a registration still succeeds,
// silently, and history says a notice was held back.
const (
	sessionJoinedLimit  = 5
	sessionJoinedWindow = time.Hour
)

// sessionJoinedAnon names a member with no recorded display name: never
// the principal, which is an email on a hosted store.
const sessionJoinedAnon = "a member"

// memberDisplayName is principal's recorded display name in the workspace,
// cleaned as an agent marker's engineer is, or "" when there is none or it
// looks like an email. A local store records no display names.
func (s *Store) memberDisplayName(ctx context.Context, tx *transaction, wid, principal string) (string, error) {
	if !s.postgres {
		return "", nil
	}
	rows, err := query(ctx, tx, `SELECT display_name FROM members WHERE workspace_id=? AND principal=?`, wid, principal)
	if err != nil || len(rows) != 1 {
		return "", err
	}
	name := strings.TrimSpace(str(rows[0], "display_name"))
	if strings.Contains(name, "@") {
		return "", nil
	}
	return cleanDisplayName(name), nil
}

// sessionJoinedBody is a join notice's fixed text: typed fields only.
func sessionJoinedBody(name, display, kind, lane, from string, lanes bool) string {
	where := "no lanes"
	if lanes {
		where = fmt.Sprintf("lane %s (%s)", lane, from)
	}
	return fmt.Sprintf("Session %s joined: %s's %s agent, %s.\n%s", name, display, kind, where, sessionJoinedLine)
}

// sessionJoined tells the coordinator targets that session, just
// registered, joined: one message from the new session to each target
// but itself. The member's limit is counted from the join notices their
// sessions sent in the last hour.
func (s *Store) sessionJoined(ctx context.Context, tx *transaction, wid, pid string, session map[string]any) error {
	targets, err := s.coordinatorTargets(ctx, tx, pid, "", nil)
	if err != nil {
		return err
	}
	sid := str(session, "id")
	var to []map[string]any
	for _, t := range targets {
		if str(t, "id") != sid {
			to = append(to, t)
		}
	}
	if len(to) == 0 {
		return nil
	}
	now := str(session, "created_at")
	since := layout(at(now).Add(-sessionJoinedWindow))
	var sent int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT m.sender_id) FROM sessions s JOIN messages m ON m.project_id=s.project_id AND m.sender_id=s.id WHERE s.project_id=? AND s.owner_principal=? AND s.created_at>=? AND s.id<>? AND m.kind=? AND m.created_at>=?`, pid, s.principal, since, sid, sessionJoinedKind, since).Scan(&sent); err != nil {
		return err
	}
	if sent >= sessionJoinedLimit {
		return s.audit(ctx, tx, wid, pid, sid, "session", sid, "join_notice_suppressed", map[string]any{"reason": "rate_limited", "limit": sessionJoinedLimit, "window": "1h"})
	}
	display, err := s.memberDisplayName(ctx, tx, wid, s.principal)
	if err != nil {
		return err
	}
	if display == "" {
		display = sessionJoinedAnon
	}
	lane, _, from, lanes, err := s.sessionLane(ctx, tx, pid, session)
	if err != nil {
		return err
	}
	body := sessionJoinedBody(str(session, "name"), display, str(session, "agent_kind"), lane, from, lanes)
	retention := 30
	if w, e := one(ctx, tx, `SELECT retention_days FROM workspaces WHERE id=?`, wid); e == nil && asInt(w["retention_days"]) > 0 {
		retention = asInt(w["retention_days"])
	}
	for _, t := range to {
		id := newID("msg_")
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,?,?,?,?,?,?,?,NULL)`, id, pid, pid, sid, str(t, "name"), id, sessionJoinedKind, body, now, layout(at(now).AddDate(0, 0, retention)), s.principal, newID("joined_"), digestOf(body)); err != nil {
			return err
		}
		if err = insertDeliveries(ctx, tx, pid, id, []string{str(t, "id")}); err != nil {
			return err
		}
	}
	return nil
}

// newestJoins are the overview's newest sessions registered in the last
// day, at most three: name, the owner's display name (never the
// principal), agent kind and when.
func (s *Store) newestJoins(ctx context.Context, tx *transaction, wid, pid string, now time.Time) ([]map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT name,owner_principal,agent_kind,created_at FROM sessions WHERE project_id=? AND role<>'system' AND created_at>=? ORDER BY created_at DESC,id LIMIT 3`, pid, layout(now.Add(-24*time.Hour)))
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, r := range rows {
		display, err := s.memberDisplayName(ctx, tx, wid, str(r, "owner_principal"))
		if err != nil {
			return nil, err
		}
		if display == "" {
			display = sessionJoinedAnon
		}
		out = append(out, map[string]any{"name": r["name"], "display_name": display, "agent_kind": r["agent_kind"], "created_at": r["created_at"]})
	}
	return out, nil
}
