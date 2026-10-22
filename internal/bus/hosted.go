package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

func (s *Store) projectRole(ctx context.Context, tx *transaction, w map[string]any, pid string) (string, error) {
	if w["owner_principal"] == s.principal {
		return "owner", nil
	}
	row, err := one(ctx, tx, `SELECT role FROM project_roles WHERE project_id=? AND principal=?`, pid, s.principal)
	if err != nil {
		return "", scopedError(err, "project membership required", "forbidden")
	}
	return row["role"].(string), nil
}
func (s *Store) setProjectRole(ctx context.Context, tx *transaction, wid, pid string, a map[string]any) (any, error) {
	principal, role := normalPrincipal(str(a, "principal")), str(a, "role")
	if !validPrincipal(principal) {
		return nil, errors.New("principal must be an email")
	}
	switch role {
	case "owner", "lead", "member", "reader":
	default:
		return nil, errors.New("role must be owner, lead, member or reader")
	}
	now := stamp()
	if _, err := tx.ExecContext(ctx, `INSERT INTO members(workspace_id,principal,added_by,added_at) VALUES(?,?,?,?) ON CONFLICT(workspace_id,principal) DO NOTHING`, wid, principal, s.principal, now); err != nil {
		return nil, err
	}
	old, err := query(ctx, tx, `SELECT role FROM project_roles WHERE project_id=? AND principal=?`, pid, principal)
	if err != nil {
		return nil, err
	}
	if len(old) > 0 && old[0]["role"] == role {
		return one(ctx, tx, `SELECT * FROM project_roles WHERE project_id=? AND principal=?`, pid, principal)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO project_roles(project_id,principal,role,granted_by,granted_at) VALUES(?,?,?,?,?) ON CONFLICT(project_id,principal) DO UPDATE SET role=excluded.role,granted_by=excluded.granted_by,granted_at=excluded.granted_at`, pid, principal, role, s.principal, now); err != nil {
		return nil, err
	}
	var before any
	if len(old) > 0 {
		before = old[0]["role"]
	}
	if err = s.audit(ctx, tx, wid, pid, "", "project_role", principal, "set", map[string]any{"old": before, "role": role, "principal": principal}); err != nil {
		return nil, err
	}
	return one(ctx, tx, `SELECT * FROM project_roles WHERE project_id=? AND principal=?`, pid, principal)
}

// leadMayAdminister limits a lead's peer administration of another principal's
// session: only sessions owned by member or reader principals, never the
// owner's, another lead's or an unassigned principal's, and never a change of
// that session's routing role. Owners keep full peer administration.
func (s *Store) leadMayAdminister(ctx context.Context, tx *transaction, w map[string]any, pid string, target map[string]any, a map[string]any) error {
	principal, _ := target["owner_principal"].(string)
	role := ""
	if principal == w["owner_principal"] {
		role = "owner"
	} else {
		rows, err := query(ctx, tx, `SELECT role FROM project_roles WHERE project_id=? AND principal=?`, pid, principal)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			role, _ = rows[0]["role"].(string)
		}
	}
	if role != "member" && role != "reader" {
		return errors.New("project lead permission covers only its own sessions and those of members or readers")
	}
	if _, ok := a["role"]; ok {
		return errors.New("project lead permission cannot change the role of a session it does not own")
	}
	return nil
}

// revokeProjectRole removes a principal's access to one project: its role row,
// its active sessions there (retired, so messages to them fail), and its
// workspace membership once it holds no role in any project of the workspace.
func (s *Store) revokeProjectRole(ctx context.Context, tx *transaction, w map[string]any, wid, pid string, a map[string]any) (any, error) {
	principal := normalPrincipal(str(a, "principal"))
	if !validPrincipal(principal) {
		return nil, errors.New("principal must be an email")
	}
	if principal == w["owner_principal"] {
		return nil, errors.New("the workspace owner's access cannot be revoked")
	}
	if principal == s.principal {
		return nil, errors.New("owners cannot revoke their own access")
	}
	old, err := query(ctx, tx, `SELECT role FROM project_roles WHERE project_id=? AND principal=?`, pid, principal)
	if err != nil {
		return nil, err
	}
	if len(old) == 0 {
		return nil, errors.New("principal has no role in this project")
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM project_roles WHERE project_id=? AND principal=?`, pid, principal); err != nil {
		return nil, err
	}
	sessions, err := query(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND owner_principal=? AND status='active' ORDER BY id`, pid, principal)
	if err != nil {
		return nil, err
	}
	retired := []string{}
	for _, row := range sessions {
		sid := row["id"].(string)
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET status='retired',updated_at=? WHERE id=? AND project_id=?`, stamp(), sid, pid); err != nil {
			return nil, err
		}
		if err = s.audit(ctx, tx, wid, pid, "", "session", sid, "retired", map[string]any{"reason": "access_revoked"}); err != nil {
			return nil, err
		}
		if _, err = s.endSessionAssignment(ctx, tx, wid, pid, sid, "access_revoked", nil); err != nil {
			return nil, err
		}
		retired = append(retired, sid)
	}
	var remaining int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_roles pr JOIN projects p ON p.id=pr.project_id WHERE p.workspace_id=? AND pr.principal=?`, wid, principal).Scan(&remaining); err != nil {
		return nil, err
	}
	if remaining == 0 {
		if _, err = tx.ExecContext(ctx, `DELETE FROM members WHERE workspace_id=? AND principal=?`, wid, principal); err != nil {
			return nil, err
		}
	}
	result := map[string]any{"principal": principal, "revoked_role": old[0]["role"], "retired_sessions": retired, "membership_removed": remaining == 0}
	if err = s.audit(ctx, tx, wid, pid, "", "project_role", principal, "revoked", result); err != nil {
		return nil, err
	}
	return result, nil
}

// listProjects returns only projects visible to the verified caller. Hosted
// workspace owners see the whole workspace; other principals see explicit roles.
func (s *Store) listProjects(ctx context.Context, tx *transaction, w map[string]any) (any, error) {
	wid := w["id"].(string)
	q := `SELECT p.*,`
	args := []any{}
	if s.postgres {
		q += ` CASE WHEN ?=? THEN 'owner' ELSE pr.role END AS role FROM projects p LEFT JOIN project_roles pr ON pr.project_id=p.id AND pr.principal=? WHERE p.workspace_id=? AND (?=? OR pr.principal IS NOT NULL) ORDER BY p.created_at,p.id`
		args = append(args, w["owner_principal"], s.principal, s.principal, wid, w["owner_principal"], s.principal)
	} else {
		q += ` 'owner' AS role FROM projects p WHERE p.workspace_id=? ORDER BY p.created_at,p.id`
		args = append(args, wid)
	}
	rows, err := query(ctx, tx, q, args...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"projects": rows}, nil
}

// listMembers exposes project role assignments without session instance tokens.
// The workspace owner is included as the effective project owner even when it
// has no separate project_roles row.
func (s *Store) listMembers(ctx context.Context, tx *transaction, pid string) (any, error) {
	if !s.postgres {
		return map[string]any{"project_id": pid, "members": []map[string]any{}}, nil
	}
	rows, err := query(ctx, tx, `SELECT c.principal,c.display_name,c.role,c.granted_by,c.granted_at FROM (SELECT pr.principal,COALESCE(m.display_name,'') AS display_name,pr.role,pr.granted_by,pr.granted_at FROM project_roles pr LEFT JOIN members m ON m.workspace_id=(SELECT workspace_id FROM projects WHERE id=?) AND m.principal=pr.principal WHERE pr.project_id=? UNION ALL SELECT w.owner_principal AS principal,COALESCE(m.display_name,'') AS display_name,'owner' AS role,w.owner_principal AS granted_by,w.created_at AS granted_at FROM workspaces w JOIN projects p ON p.workspace_id=w.id LEFT JOIN members m ON m.workspace_id=w.id AND m.principal=w.owner_principal WHERE p.id=? AND NOT EXISTS(SELECT 1 FROM project_roles pr WHERE pr.project_id=p.id AND pr.principal=w.owner_principal)) c ORDER BY c.principal`, pid, pid, pid)
	if err != nil {
		return nil, err
	}
	return map[string]any{"project_id": pid, "members": rows}, nil
}

func (s *Store) resolveInTransaction(ctx context.Context, tx *transaction, r Request) (any, error) {
	q := `SELECT w.id AS workspace_id,p.id AS project_id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE `
	args := []any{}
	if s.postgres {
		q += `w.mode='server' AND EXISTS(SELECT 1 FROM members m WHERE m.workspace_id=w.id AND m.principal=?) AND (w.owner_principal=? OR EXISTS(SELECT 1 FROM project_roles pr WHERE pr.project_id=p.id AND pr.principal=?))`
		args = append(args, s.principal, s.principal, s.principal)
	} else {
		q += `w.mode='local' AND w.owner_principal=?`
		args = append(args, s.principal)
	}
	if r.Workspace != "" {
		q += ` AND (w.id=? OR w.name=?)`
		args = append(args, r.Workspace, r.Workspace)
	}
	if r.Project != "" {
		q += ` AND (p.id=? OR p.slug=?)`
		args = append(args, r.Project, r.Project)
	} else {
		remote, err := NormalizeRemote(str(r.Args, "git_remote"))
		if err != nil {
			return nil, err
		}
		if remote == "" {
			return nil, errors.New("project selector or registered git remote required")
		}
		q += ` AND p.git_remote=?`
		args = append(args, remote)
	}
	rows, err := query(ctx, tx, q, args...)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, errors.New("project not found, not authorized or ambiguous; select explicit workspace and project")
	}
	if r.Session != "" || str(r.Args, "instance") != "" {
		if err = s.touchReadActor(ctx, tx, rows[0]["project_id"].(string), r); err != nil {
			return nil, err
		}
	}
	return rows[0], nil
}
func (s *Store) snapshot(ctx context.Context, tx *transaction, w, p map[string]any, r Request) (any, error) {
	pid := p["id"].(string)
	if r.Session == "" || str(r.Args, "instance") == "" {
		return nil, errors.New("snapshot requires a bound session and instance")
	}
	match, args := sessionAddress(r.Session)
	actor, err := one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND `+match+` AND instance=? AND owner_principal=?`, append(append([]any{pid}, args...), str(r.Args, "instance"), s.principal)...)
	if err != nil {
		return nil, scopedError(err, "snapshot session ownership or instance mismatch", "forbidden")
	}
	sid := actor["id"].(string)
	now := stamp()
	if err = s.snapshotBudget(ctx, tx, w["id"].(string), pid, sid, now); err != nil {
		return nil, err
	}
	datasets := map[string][]map[string]any{}
	// Retired and moved sessions are never deleted, so only the most recent of
	// them are included; every active session always is.
	datasets["sessions"], err = query(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND (status='active' OR id IN (SELECT id FROM sessions WHERE project_id=? AND status<>'active' ORDER BY sequence DESC LIMIT `+fmt.Sprint(snapshotRetiredSessions)+`)) ORDER BY sequence`, pid, pid)
	if err != nil {
		return nil, err
	}
	var inactive int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE project_id=? AND status<>'active'`, pid).Scan(&inactive); err != nil {
		return nil, err
	}
	datasets["messages"], err = query(ctx, tx, `SELECT m.sequence,m.id,m.project_id,m.source_project_id,m.sender_id,s.name AS sender,m.address,m.thread_id,m.reply_to,m.kind,m.body,m.coordination,m.created_at,m.expires_at,m.created_by_principal,d.delivered_at,d.read_at FROM messages m JOIN deliveries d ON d.project_id=m.project_id AND d.message_id=m.id JOIN sessions s ON s.id=m.sender_id AND s.project_id=m.source_project_id WHERE d.project_id=? AND d.recipient_id=? AND m.expires_at>? ORDER BY m.sequence LIMIT 10001`, pid, sid, now)
	if err != nil {
		return nil, err
	}
	sent, err := query(ctx, tx, `SELECT id,sequence,project_id,sender_id,thread_id,reply_to,coordination,created_at,expires_at FROM messages WHERE project_id=? AND sender_id=? AND expires_at>? ORDER BY sequence LIMIT 10001`, pid, sid, now)
	if err != nil {
		return nil, err
	}
	byID := map[string]map[string]any{}
	for _, m := range sent {
		m["deliveries"] = []map[string]any{}
		byID[m["id"].(string)] = m
	}
	deliveryRows, err := query(ctx, tx, `SELECT m.id AS message_id,d.recipient_id,s.name AS recipient,d.delivered_at,d.read_at FROM messages m JOIN deliveries d ON d.project_id=m.project_id AND d.message_id=m.id JOIN sessions s ON s.project_id=d.project_id AND s.id=d.recipient_id WHERE m.project_id=? AND m.sender_id=? AND m.expires_at>? ORDER BY m.sequence,d.recipient_id`, pid, sid, now)
	if err != nil {
		return nil, err
	}
	for _, d := range deliveryRows {
		m := byID[d["message_id"].(string)]
		delete(d, "message_id")
		m["deliveries"] = append(m["deliveries"].([]map[string]any), d)
	}
	datasets["sent_messages"] = sent
	// History is append-only, so only its most recent window is included; one
	// extra row shows whether anything older was left out.
	datasets["events"], err = query(ctx, tx, `SELECT * FROM (SELECT * FROM history WHERE workspace_id=? AND project_id=? ORDER BY sequence DESC LIMIT `+fmt.Sprint(snapshotEvents+1)+`) recent ORDER BY sequence`, w["id"], pid)
	if err != nil {
		return nil, err
	}
	eventsTruncated := len(datasets["events"]) > snapshotEvents
	if eventsTruncated {
		datasets["events"] = datasets["events"][1:]
	}
	for _, name := range []string{"messages", "sent_messages"} {
		if len(datasets[name]) > 10000 {
			return nil, fmt.Errorf("snapshot exceeds 10000 %s; no complete snapshot produced", name)
		}
	}
	// Routing is discoverable, but instance tokens for peers are not exported by the hosted service.
	for _, row := range datasets["sessions"] {
		if row["id"] != sid {
			delete(row, "instance")
		}
	}
	result := map[string]any{"workspace": w, "project": p, "session": actor, "principal": s.principal, "verified": true, "identity_provenance": "google-id-token", "storage_mode": "cloud", "snapshot_at": now}
	for k, v := range datasets {
		result[k] = v
	}
	result["events_truncated"] = eventsTruncated
	if len(datasets["events"]) > 0 {
		result["events_oldest_sequence"] = datasets["events"][0]["sequence"]
	}
	result["sessions_truncated"] = inactive > snapshotRetiredSessions
	encoded, e := json.Marshal(result)
	if e != nil || len(encoded) > 12<<20 {
		return nil, errors.New("snapshot exceeds supported byte budget; no complete snapshot produced")
	}
	return result, nil
}
func (s *Store) purgeExpired(ctx context.Context, tx *transaction, wid, pid string) (any, error) {
	now := stamp()
	res, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE project_id=? AND message_id IN(SELECT id FROM messages WHERE project_id=? AND expires_at<=?)`, pid, pid, now)
	if err != nil {
		return nil, err
	}
	deliveries, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE messages SET body='',coordination=NULL,purged_at=? WHERE project_id=? AND expires_at<=? AND purged_at IS NULL`, now, pid, now)
	if err != nil {
		return nil, err
	}
	messages, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	result := map[string]any{"purged_messages": messages, "deleted_deliveries": deliveries}
	if messages > 0 || deliveries > 0 {
		if err = s.audit(ctx, tx, wid, pid, "", "retention", pid, "purged", result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func redactPeerInstances(v any, principal string) {
	switch value := v.(type) {
	case map[string]any:
		if owner, ok := value["owner_principal"].(string); ok && owner != principal {
			delete(value, "instance")
		}
		for _, child := range value {
			redactPeerInstances(child, principal)
		}
	case []map[string]any:
		for _, child := range value {
			redactPeerInstances(child, principal)
		}
	case []any:
		for _, child := range value {
			redactPeerInstances(child, principal)
		}
	}
}

// Audit records identify rotations by fingerprint, never by exporting resumable
// instance tokens. This also sanitizes records created by earlier schema-1 builds.
func hostedAuditDetails(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, child := range value {
			if k == "instance" || k == "prior_instance" {
				out[k+"_fingerprint"] = hashPayload(child)
			} else {
				out[k] = hostedAuditDetails(child)
			}
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(value))
		for i, child := range value {
			out[i] = hostedAuditDetails(child).(map[string]any)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = hostedAuditDetails(child)
		}
		return out
	default:
		return value
	}
}

// A snapshot holds the newest snapshotEvents history events and, besides every
// active session, the snapshotRetiredSessions most recent other sessions.
const (
	snapshotEvents          = 5000
	snapshotRetiredSessions = 1000
)

// Count and estimate before materializing text-heavy datasets. Row caps are
// independent; the lower byte cap keeps a single sync within HTTP/cache limits.
// Sessions and events are measured over the same windows the snapshot takes.
func (s *Store) snapshotBudget(ctx context.Context, tx *transaction, wid, pid, sid, now string) error {
	checks := []struct {
		name, sql string
		args      []any
	}{
		{"active sessions", `SELECT COUNT(*),COALESCE(SUM(octet_length(notes)+octet_length(transport)+2048),0)::BIGINT FROM sessions WHERE project_id=? AND status='active'`, []any{pid}},
		{"retired sessions", `SELECT COUNT(*),COALESCE(SUM(octet_length(notes)+octet_length(transport)+2048),0)::BIGINT FROM (SELECT notes,transport FROM sessions WHERE project_id=? AND status<>'active' ORDER BY sequence DESC LIMIT ` + fmt.Sprint(snapshotRetiredSessions) + `) recent`, []any{pid}},
		{"messages", `SELECT COUNT(*),COALESCE(SUM(octet_length(m.body)+COALESCE(octet_length(m.coordination),0)+2048),0)::BIGINT FROM messages m JOIN deliveries d ON d.project_id=m.project_id AND d.message_id=m.id WHERE d.project_id=? AND d.recipient_id=? AND m.expires_at>?`, []any{pid, sid, now}},
		{"sent_messages", `SELECT COUNT(*),COALESCE(SUM(2048+(SELECT COUNT(*)*512 FROM deliveries d WHERE d.project_id=m.project_id AND d.message_id=m.id)),0)::BIGINT FROM messages m WHERE project_id=? AND sender_id=? AND expires_at>?`, []any{pid, sid, now}},
		{"events", `SELECT COUNT(*),COALESCE(SUM(octet_length(details)+1024),0)::BIGINT FROM (SELECT details FROM history WHERE workspace_id=? AND project_id=? ORDER BY sequence DESC LIMIT ` + fmt.Sprint(snapshotEvents) + `) recent`, []any{wid, pid}},
	}
	var total int64
	for _, check := range checks {
		var count, size int64
		if err := tx.QueryRowContext(ctx, check.sql, check.args...).Scan(&count, &size); err != nil {
			return err
		}
		if count > 10000 {
			return fmt.Errorf("snapshot exceeds 10000 %s; no complete snapshot produced", check.name)
		}
		total += size
		if total > 8<<20 {
			return errors.New("snapshot exceeds supported byte budget; no complete snapshot produced")
		}
	}
	return nil
}
