package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func metadata(a map[string]any) error {
	for _, k := range []string{"instance", "role", "agent_kind", "model", "host", "account"} {
		if len(str(a, k)) > 256 {
			return fmt.Errorf("%s exceeds 256 bytes", k)
		}
	}
	if len(str(a, "notes")) > 8192 {
		return errors.New("notes exceed 8192 bytes")
	}
	if len(asJSON(a["transport"])) > 8192 {
		return errors.New("transport exceeds 8192 bytes")
	}
	return nil
}
func (s *Store) register(ctx context.Context, tx *transaction, wid, pid string, r Request) (any, error) {
	if err := sessionNameOK(r.Session); err != nil {
		return nil, err
	}
	a := r.Args
	if err := metadata(a); err != nil {
		return nil, err
	}
	rows, err := query(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND name=?`, pid, r.Session)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		old := rows[0]
		if old["owner_principal"] != s.principal || str(a, "instance") == "" || old["instance"] != str(a, "instance") {
			return nil, errors.New("session name already registered to another instance; use a distinct name")
		}
		if old["status"] != "active" {
			return nil, errors.New("retired session names cannot be reused; use a distinct name")
		}
		for k, v := range a {
			if k != "instance" && k != "resume" && asJSON(old[k]) != asJSON(v) {
				return nil, errors.New("registration metadata conflicts; use update_session")
			}
		}
		if resume, _ := a["resume"].(bool); resume {
			instance, now := newID("inst_"), stamp()
			if _, err = tx.ExecContext(ctx, `UPDATE sessions SET instance=?,last_seen=?,updated_at=? WHERE project_id=? AND id=?`, instance, now, now, pid, old["id"]); err != nil {
				return nil, err
			}
			if err = s.audit(ctx, tx, wid, pid, old["id"].(string), "session", old["id"].(string), "resumed", map[string]any{"instance": instance}); err != nil {
				return nil, err
			}
			return one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND id=?`, pid, old["id"])
		}
		return old, nil
	}
	if resume, _ := a["resume"].(bool); resume {
		return nil, errors.New("cannot resume an unknown session; register a new name first")
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE project_id=? AND status='active'`, pid).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 1000 {
		return nil, errors.New("project active-session limit (1000) reached")
	}
	instance := str(a, "instance")
	if instance == "" {
		instance = newID("inst_")
	}
	kind := str(a, "agent_kind")
	if kind == "" {
		kind = "other"
	}
	switch kind {
	case "claude-code", "codex", "gemini-cli", "antigravity", "human", "other":
	default:
		return nil, errors.New("unsupported agent_kind")
	}
	transport := a["transport"]
	if transport == nil {
		transport = map[string]any{}
	}
	id, now := newID("ses_"), stamp()
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id,project_id,name,instance,owner_principal,role,agent_kind,model,host,account,transport,notes,status,last_seen,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, pid, r.Session, instance, s.principal, str(a, "role"), kind, str(a, "model"), str(a, "host"), str(a, "account"), asJSON(transport), str(a, "notes"), "active", now, now, now)
	if err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, id, "session", id, "registered", map[string]any{"name": r.Session, "instance": instance, "role": str(a, "role")}); err != nil {
		return nil, err
	}
	return one(ctx, tx, `SELECT * FROM sessions WHERE id=? AND project_id=?`, id, pid)
}
func (s *Store) update(ctx context.Context, tx *transaction, wid, pid string, actor map[string]any, a map[string]any) (any, error) {
	if err := metadata(a); err != nil {
		return nil, err
	}
	sid := actor["id"].(string)
	sets := []string{}
	values := []any{}
	changes := map[string]any{}
	for _, k := range []string{"role", "model", "host", "account", "notes", "transport"} {
		if v, ok := a[k]; ok {
			sets = append(sets, k+"=?")
			if k == "transport" {
				values = append(values, asJSON(v))
			} else {
				values = append(values, v)
			}
			changes[k] = map[string]any{"old": actor[k], "new": v}
		}
	}
	if len(sets) == 0 {
		return nil, errors.New("no session fields supplied")
	}
	sets = append(sets, "updated_at=?")
	now := stamp()
	values = append(values, now)
	if tx.actorID == "" || tx.actorID == sid {
		sets = append(sets, "last_seen=?")
		values = append(values, now)
	}
	values = append(values, sid, pid)
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET `+strings.Join(sets, ",")+` WHERE id=? AND project_id=?`, values...)
	if err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, sid, "session", sid, "updated", changes); err != nil {
		return nil, err
	}
	return one(ctx, tx, `SELECT * FROM sessions WHERE id=? AND project_id=?`, sid, pid)
}
func (s *Store) retire(ctx context.Context, tx *transaction, wid, pid string, actor map[string]any, a map[string]any) (any, error) {
	sid := actor["id"].(string)
	status := str(a, "status")
	if status == "" {
		status = "retired"
	}
	if status != "retired" && status != "moved" {
		return nil, errors.New("retirement status must be retired or moved")
	}
	var successor any
	if name := str(a, "successor"); name != "" {
		match, args := sessionAddress(name)
		next, err := one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND `+match, append([]any{pid}, args...)...)
		if err != nil {
			return nil, scopedError(err, "successor must exist in the same project", "invalid")
		}
		if next["id"] == sid {
			return nil, errors.New("session cannot succeed itself")
		}
		if next["status"] != "active" {
			return nil, errors.New("successor must be active")
		}
		successor = next["id"]
		seen := map[string]bool{sid: true}
		cursor := next
		for cursor != nil {
			key := cursor["id"].(string)
			if seen[key] {
				return nil, errors.New("successor cycle is forbidden")
			}
			seen[key] = true
			if cursor["successor"] == nil {
				break
			}
			cursor, err = one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND id=?`, pid, cursor["successor"])
			if err != nil {
				return nil, err
			}
		}
	}
	if actor["status"] != "active" {
		if actor["status"] == status && actor["successor"] == successor {
			return actor, nil
		}
		return nil, errors.New("session is already retired with different successor or status")
	}
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET status=?,successor=?,updated_at=? WHERE id=? AND project_id=?`, status, successor, stamp(), sid, pid)
	if err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, sid, "session", sid, status, map[string]any{"successor": successor}); err != nil {
		return nil, err
	}
	return one(ctx, tx, `SELECT * FROM sessions WHERE id=? AND project_id=?`, sid, pid)
}
