package bus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	regexp.MustCompile(`\bsk-(?:proj-|ant-)?[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{30,}`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/-]{20,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?token|client[_-]?secret|password)\s*[:=]\s*["']?[A-Za-z0-9/+_.-]{20,}`),
}

func validateBody(body string) error {
	if !utf8.ValidString(body) || strings.TrimSpace(body) == "" {
		return errors.New("body must be nonempty UTF-8 text")
	}
	if len(body) > 65536 {
		return errors.New("message body exceeds 65536 bytes")
	}
	if strings.ContainsRune(body, 0) {
		return errors.New("body must not contain NUL")
	}
	for _, re := range secretPatterns {
		if re.MatchString(body) {
			return errors.New("message rejected: possible secret detected; remove credentials before sending")
		}
	}
	return nil
}
func (s *Store) send(ctx context.Context, tx *sql.Tx, wid, pid string, actor map[string]any, r Request) (any, error) {
	a := r.Args
	body := str(a, "body")
	if err := validateBody(body); err != nil {
		return nil, err
	}
	op := str(a, "operation_id")
	if op == "" || len(op) > 128 {
		return nil, errors.New("operation_id (1–128 bytes) is required for safe retries")
	}
	kind := str(a, "kind")
	if kind == "" {
		kind = "message"
		if r.Operation == "reply" {
			kind = "reply"
		}
	}
	switch kind {
	case "message", "reply", "decision", "handoff", "routing-change":
	default:
		return nil, errors.New("invalid message kind")
	}
	sid := actor["id"].(string)
	// Hash the supplied operation before resolving dynamic role membership or retirement.
	// A successful exact retry returns the original message even after recipients change.
	payload := map[string]any{"operation": r.Operation, "to": str(a, "to"), "body": body, "kind": kind, "thread_id": str(a, "thread_id"), "message_id": str(a, "message_id")}
	digest := hashPayload(payload)
	previous, err := query(ctx, tx, `SELECT id,payload_hash FROM messages WHERE project_id=? AND sender_id=? AND operation_id=?`, pid, sid, op)
	if err != nil {
		return nil, err
	}
	if len(previous) > 0 {
		if previous[0]["payload_hash"] != digest {
			return nil, errors.New("operation_id was already used with a different payload")
		}
		return s.message(ctx, tx, pid, previous[0]["id"].(string))
	}
	if actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; no new send is permitted")
	}
	to, thread, replyTo := str(a, "to"), str(a, "thread_id"), ""
	if r.Operation == "reply" {
		replyTo = str(a, "message_id")
		parent, e := s.participatingMessage(ctx, tx, pid, sid, replyTo)
		if e != nil {
			return nil, e
		}
		to = parent["sender_id"].(string)
		thread = parent["thread_id"].(string)
	} else if thread != "" {
		parent, e := s.participatingMessage(ctx, tx, pid, sid, thread)
		if e != nil {
			return nil, e
		}
		if parent["id"] != parent["thread_id"] {
			return nil, errors.New("thread_id must identify the root message")
		}
	}
	if strings.Contains(to, "/") {
		return nil, errors.New("cross-project messaging is not supported in local Phase 1")
	}
	var recipients []map[string]any
	switch {
	case to == "@all":
		recipients, err = query(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND status='active' AND id<>? ORDER BY id`, pid, sid)
	case strings.HasPrefix(to, "@role:"):
		role := strings.TrimPrefix(to, "@role:")
		if role == "" {
			return nil, errors.New("role address requires a role")
		}
		recipients, err = query(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND status='active' AND role=? AND id<>? ORDER BY id`, pid, role, sid)
	case to == "" || strings.HasPrefix(to, "@"):
		return nil, errors.New("invalid recipient address")
	default:
		recipients, err = query(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND (id=? OR name=?) AND status='active' ORDER BY id`, pid, to, to)
	}
	if err != nil {
		return nil, err
	}
	if len(recipients) > 1 && to != "@all" && !strings.HasPrefix(to, "@role:") {
		return nil, errors.New("recipient selector is ambiguous; use the immutable session ID")
	}
	if len(recipients) == 0 {
		return nil, errors.New("no active recipients; retired sessions are not forwarded automatically")
	}
	if len(recipients) > 1000 {
		return nil, errors.New("delivery expansion exceeds 1000 recipients")
	}
	var recent int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE sender_id=? AND created_at>=?`, sid, time.Now().UTC().Add(-time.Minute).Format(timestampLayout)).Scan(&recent); err != nil {
		return nil, err
	}
	if recent >= 120 {
		return nil, errors.New("session send rate limit (120/minute) reached; no write queued")
	}
	var retention int
	if err = tx.QueryRowContext(ctx, `SELECT retention_days FROM workspaces WHERE id=?`, wid).Scan(&retention); err != nil {
		return nil, err
	}
	id := newID("msg_")
	if thread == "" {
		thread = id
	}
	var parent any
	if replyTo != "" {
		parent = replyTo
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, pid, pid, sid, to, thread, parent, kind, body, now.Format(timestampLayout), now.AddDate(0, 0, retention).Format(timestampLayout), s.principal, op, digest)
	if err != nil {
		return nil, err
	}
	for _, recipient := range recipients {
		if _, err = tx.ExecContext(ctx, `INSERT INTO deliveries(message_id,project_id,recipient_id) VALUES(?,?,?)`, id, pid, recipient["id"]); err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET last_seen=? WHERE project_id=? AND id=?`, stamp(), pid, sid); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, sid, "message", id, "sent", map[string]any{"address": to, "recipient_count": len(recipients), "thread_id": thread, "reply_to": parent}); err != nil {
		return nil, err
	}
	return s.message(ctx, tx, pid, id)
}
func (s *Store) participatingMessage(ctx context.Context, tx *sql.Tx, pid, sid, id string) (map[string]any, error) {
	m, err := one(ctx, tx, `SELECT m.* FROM messages m WHERE m.project_id=? AND m.id=? AND m.expires_at>? AND (m.sender_id=? OR EXISTS(SELECT 1 FROM deliveries d WHERE d.project_id=m.project_id AND d.message_id=m.id AND d.recipient_id=?))`, pid, id, stamp(), sid, sid)
	if err != nil {
		return nil, errors.New("message not found in this session's scope")
	}
	return m, nil
}
func (s *Store) message(ctx context.Context, tx *sql.Tx, pid, id string) (any, error) {
	m, err := one(ctx, tx, `SELECT sequence,id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id FROM messages WHERE project_id=? AND id=?`, pid, id)
	if err != nil {
		return nil, err
	}
	d, err := query(ctx, tx, `SELECT recipient_id,delivered_at,read_at FROM deliveries WHERE project_id=? AND message_id=? ORDER BY recipient_id`, pid, id)
	if err != nil {
		return nil, err
	}
	m["deliveries"] = d
	return m, nil
}
func (s *Store) messageStatus(ctx context.Context, tx *sql.Tx, pid, sid, id string) (any, error) {
	m, err := one(ctx, tx, `SELECT id,sequence,project_id,sender_id,thread_id,reply_to,created_at,expires_at FROM messages WHERE project_id=? AND sender_id=? AND id=? AND expires_at>?`, pid, sid, id, stamp())
	if err != nil {
		return nil, errors.New("message status is available only to its sender in this project")
	}
	d, err := query(ctx, tx, `SELECT d.recipient_id,s.name AS recipient,d.delivered_at,d.read_at FROM deliveries d JOIN sessions s ON s.id=d.recipient_id AND s.project_id=d.project_id WHERE d.project_id=? AND d.message_id=? ORDER BY d.recipient_id`, pid, id)
	if err != nil {
		return nil, err
	}
	m["deliveries"] = d
	return m, nil
}
func (s *Store) inbox(ctx context.Context, tx *sql.Tx, wid, pid, sid string, a map[string]any) (any, error) {
	n, err := limit(a)
	if err != nil {
		return nil, err
	}
	after, err := cursor(a)
	if err != nil {
		return nil, err
	}
	unread := true
	if v, ok := a["unread_only"]; ok {
		unread = v.(bool)
	}
	messages, err := query(ctx, tx, `SELECT m.sequence,m.id,m.project_id,m.source_project_id,m.sender_id,s.name AS sender,m.address,m.thread_id,m.reply_to,m.kind,m.body,m.created_at,m.expires_at,m.created_by_principal,d.delivered_at,d.read_at FROM messages m JOIN deliveries d ON d.project_id=m.project_id AND d.message_id=m.id JOIN sessions s ON s.id=m.sender_id AND s.project_id=m.source_project_id WHERE d.project_id=? AND d.recipient_id=? AND (?=0 OR d.read_at IS NULL) AND m.expires_at>? AND m.sequence>? ORDER BY m.sequence LIMIT ?`, pid, sid, unread, stamp(), after, n+1)
	if err != nil {
		return nil, err
	}
	envelope := page("messages", messages, n, after)
	messages = envelope["messages"].([]map[string]any)
	now := stamp()
	changed := []string{}
	for _, m := range messages {
		if m["delivered_at"] == nil {
			_, err = tx.ExecContext(ctx, `UPDATE deliveries SET delivered_at=? WHERE project_id=? AND message_id=? AND recipient_id=? AND delivered_at IS NULL`, now, pid, m["id"], sid)
			if err != nil {
				return nil, err
			}
			m["delivered_at"] = now
			changed = append(changed, m["id"].(string))
		}
	}
	if len(changed) > 0 {
		if err = s.audit(ctx, tx, wid, pid, sid, "delivery", sid, "delivered", map[string]any{"ids": changed}); err != nil {
			return nil, err
		}
	}
	return envelope, nil
}
func (s *Store) markRead(ctx context.Context, tx *sql.Tx, wid, pid, sid string, a map[string]any) (any, error) {
	ids := []string{}
	switch v := a["ids"].(type) {
	case []string:
		ids = v
	case []any:
		for _, x := range v {
			t, ok := x.(string)
			if !ok {
				return nil, errors.New("ids must contain strings")
			}
			ids = append(ids, t)
		}
	}
	if len(ids) < 1 || len(ids) > 1000 {
		return nil, errors.New("ids must contain 1–1000 message IDs")
	}
	now := stamp()
	out := []map[string]any{}
	changed := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		d, err := one(ctx, tx, `SELECT d.message_id,d.recipient_id,d.delivered_at,d.read_at FROM deliveries d JOIN messages m ON m.id=d.message_id AND m.project_id=d.project_id WHERE d.project_id=? AND d.message_id=? AND d.recipient_id=? AND m.expires_at>?`, pid, id, sid, now)
		if err != nil {
			return nil, errors.New("message is not in this session's inbox")
		}
		if d["read_at"] == nil {
			_, err = tx.ExecContext(ctx, `UPDATE deliveries SET delivered_at=COALESCE(delivered_at,?),read_at=COALESCE(read_at,?) WHERE project_id=? AND message_id=? AND recipient_id=?`, now, now, pid, id, sid)
			if err != nil {
				return nil, err
			}
			if d["delivered_at"] == nil {
				d["delivered_at"] = now
			}
			d["read_at"] = now
			changed = append(changed, id)
		}
		out = append(out, d)
	}
	if len(changed) > 0 {
		if err := s.audit(ctx, tx, wid, pid, sid, "delivery", sid, "read", map[string]any{"ids": changed}); err != nil {
			return nil, err
		}
	}
	return map[string]any{"deliveries": out, "marked": len(changed)}, nil
}

// ResolveProject returns a unique immutable project identity without creating scope.
// Selectors take precedence over normalized remote fallback. Workspace may be omitted
// for remote fallback only when the remote resolves uniquely across owned workspaces.
func (s *Store) ResolveProject(ctx context.Context, workspace, project, remote string) (string, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	q := `SELECT w.id AS workspace,p.id AS project FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.mode='local' AND w.owner_principal=?`
	args := []any{s.principal}
	if workspace != "" {
		q += ` AND (w.id=? OR w.name=?)`
		args = append(args, workspace, workspace)
	}
	if project != "" {
		q += ` AND (p.id=? OR p.slug=?)`
		args = append(args, project, project)
	} else {
		normalized, e := NormalizeRemote(remote)
		if e != nil {
			return "", "", e
		}
		if normalized == "" {
			return "", "", errors.New("project unresolved; pass --project or create .clawbridge.toml at the checkout root")
		}
		q += ` AND p.git_remote=?`
		args = append(args, normalized)
	}
	rows, err := query(ctx, tx, q, args...)
	if err != nil {
		return "", "", err
	}
	if len(rows) != 1 {
		return "", "", fmt.Errorf("project resolution found %d matches; select --workspace and --project explicitly", len(rows))
	}
	return rows[0]["workspace"].(string), rows[0]["project"].(string), nil
}
