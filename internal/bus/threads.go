package bus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	"clawbridge/internal/principal"
)

// The project owner's read of the project's threads
// (live/PROJECT-THREADS-PLAN.md, r1). Only messages sent from the cutoff on
// are readable: threads_from, stamped on every project that existed when
// this schema landed, or the project's created_at for one created later.

// threadsFromSchemaV34 adds the cutoff; the migration stamps it.
const threadsFromSchemaV34 = `
ALTER TABLE projects ADD COLUMN threads_from TEXT;
CREATE INDEX IF NOT EXISTS history_entity ON history(project_id,entity,changed_at);
`

// threadsPerMinute bounds one principal's thread reads per project. It is
// counted from the audit rows, so concurrent calls may pass it by a few.
const threadsPerMinute = 30

var threadIDPattern = regexp.MustCompile(`^msg_[0-9a-f]{32}$`)

// threadsCursor is the list's position: the last row's activity and ID.
type threadsCursor struct {
	LastAt   string `json:"last_at"`
	ThreadID string `json:"thread_id"`
}

func encodeThreadsCursor(c threadsCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeThreadsCursor accepts only what encodeThreadsCursor writes: exactly
// the two keys, an RFC 3339 time and a message ID.
func decodeThreadsCursor(raw string) (threadsCursor, error) {
	bad := invalid("before must be a cursor from project_threads")
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return threadsCursor{}, bad
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(b, &keys) != nil || len(keys) != 2 {
		return threadsCursor{}, bad
	}
	var c threadsCursor
	if json.Unmarshal(keys["last_at"], &c.LastAt) != nil || json.Unmarshal(keys["thread_id"], &c.ThreadID) != nil {
		return threadsCursor{}, bad
	}
	if _, err = time.Parse(time.RFC3339Nano, c.LastAt); err != nil || !threadIDPattern.MatchString(c.ThreadID) {
		return threadsCursor{}, bad
	}
	return c, nil
}

// threadLimit reads limit within 1..max, def when absent.
func threadLimit(a map[string]any, def, max int) (int, error) {
	v, ok := a["limit"]
	if !ok {
		return def, nil
	}
	n, err := integer(v)
	if err != nil || n < 1 || n > max {
		return 0, invalid("limit must be 1–" + strconv.Itoa(max))
	}
	return n, nil
}

// threadsOp runs project_threads and project_thread: the project owner's,
// sessionless, and on the hosted authority only from a person's device key.
func (s *Store) threadsOp(ctx context.Context, tx *transaction, wid string, p map[string]any, projectRole string, r Request) (any, error) {
	if r.Session != "" || str(r.Args, "instance") != "" {
		return nil, scoped("a session can't read the project's threads; run it yourself, without a session", "forbidden")
	}
	if projectRole != "owner" {
		return nil, scoped("project owner permission required", "forbidden")
	}
	if s.postgres && (s.identity == nil || s.identity.Issuer != DeviceIssuer || principal.CI(s.principal)) {
		return nil, scoped("the project's threads are read only from a person's own signed-in device", "forbidden")
	}
	pid := p["id"].(string)
	from := str(p, "threads_from")
	if from == "" {
		from = str(p, "created_at")
	}
	now := stamp()
	var recent int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM history WHERE project_id=? AND entity='threads' AND changed_at>? AND changed_by_principal=?`, pid, layout(at(now).Add(-time.Minute)), s.principal).Scan(&recent); err != nil {
		return nil, err
	}
	if recent >= threadsPerMinute {
		return nil, scoped("thread reads are limited to 30 a minute; try again shortly", "rate_limited")
	}
	if r.Operation == "project_threads" {
		return s.projectThreads(ctx, tx, wid, pid, from, now, r.Args)
	}
	return s.projectThread(ctx, tx, wid, pid, from, now, r.Args)
}

// inScope is the scope every thread query repeats on its messages, whatever
// a cursor says: this project, unexpired, sent from the cutoff on. Its
// arguments are the project, now and the cutoff.
func inScope(m string) string {
	return m + `.project_id=? AND ` + m + `.expires_at>? AND ` + m + `.created_at>=?`
}

func (s *Store) projectThreads(ctx context.Context, tx *transaction, wid, pid, from, now string, a map[string]any) (any, error) {
	n, err := threadLimit(a, 50, 100)
	if err != nil {
		return nil, err
	}
	var before threadsCursor
	if v, ok := a["before"]; ok {
		// validateArgs already requires a string; this never panics either.
		raw, _ := v.(string)
		if before, err = decodeThreadsCursor(raw); err != nil {
			return nil, err
		}
	}
	// A thread is in scope only if its root is; its count and activity are
	// those of its messages in scope.
	rows, err := query(ctx, tx, `SELECT r.id AS thread_id,r.kind,r.created_at AS started_at,MAX(m.created_at) AS last_at,COUNT(*) AS messages FROM messages r JOIN messages m ON m.project_id=r.project_id AND m.thread_id=r.id AND m.expires_at>? AND m.created_at>=? WHERE `+inScope("r")+` AND r.id=r.thread_id GROUP BY r.id,r.kind,r.created_at HAVING ?='' OR MAX(m.created_at)<? OR (MAX(m.created_at)=? AND r.id<?) ORDER BY last_at DESC,thread_id DESC LIMIT ?`,
		now, from, pid, now, from, before.ThreadID, before.LastAt, before.LastAt, before.ThreadID, n+1)
	if err != nil {
		return nil, err
	}
	var next any
	if len(rows) > n {
		rows = rows[:n]
		last := rows[n-1]
		next = encodeThreadsCursor(threadsCursor{LastAt: last["last_at"].(string), ThreadID: last["thread_id"].(string)})
	}
	for _, t := range rows {
		names, err := query(ctx, tx, `SELECT DISTINCT s.name FROM messages m JOIN sessions s ON s.id=m.sender_id AND s.project_id=m.source_project_id WHERE `+inScope("m")+` AND m.thread_id=? UNION SELECT DISTINCT s.name FROM messages m JOIN deliveries d ON d.project_id=m.project_id AND d.message_id=m.id JOIN sessions s ON s.id=d.recipient_id AND s.project_id=d.project_id WHERE `+inScope("m")+` AND m.thread_id=? ORDER BY name`,
			pid, now, from, t["thread_id"], pid, now, from, t["thread_id"])
		if err != nil {
			return nil, err
		}
		participants := []string{}
		for _, row := range names {
			participants = append(participants, row["name"].(string))
		}
		if len(participants) > 20 {
			t["participants_more"] = len(participants) - 20
			participants = participants[:20]
		}
		t["participants"] = participants
	}
	if err = s.audit(ctx, tx, wid, pid, "", "threads", pid, "project_threads", map[string]any{"count": len(rows), "limit": n}); err != nil {
		return nil, err
	}
	return map[string]any{"threads": rows, "next": next, "readable_from": from}, nil
}

func (s *Store) projectThread(ctx context.Context, tx *transaction, wid, pid, from, now string, a map[string]any) (any, error) {
	id := str(a, "thread_id")
	if !threadIDPattern.MatchString(id) {
		return nil, invalid("thread_id must be a message ID")
	}
	n, err := threadLimit(a, 100, 200)
	if err != nil {
		return nil, err
	}
	after, err := cursor(a)
	if err != nil {
		return nil, err
	}
	// One answer for every thread out of reach: another project's, expired,
	// from before the cutoff, a reply's ID, or none at all.
	if _, err = one(ctx, tx, `SELECT r.id FROM messages r WHERE `+inScope("r")+` AND r.id=? AND r.thread_id=r.id`, pid, now, from, id); err != nil {
		return nil, scopedError(err, "no such thread", "not_found")
	}
	messages, err := query(ctx, tx, `SELECT m.sequence,m.id,s.name AS sender,m.address,m.reply_to,m.kind,m.body,m.coordination,m.created_at,m.expires_at FROM messages m JOIN sessions s ON s.id=m.sender_id AND s.project_id=m.source_project_id WHERE `+inScope("m")+` AND m.thread_id=? AND m.sequence>? ORDER BY m.sequence LIMIT ?`,
		pid, now, from, id, after, n+1)
	if err != nil {
		return nil, err
	}
	var next any
	if len(messages) > n {
		messages = messages[:n]
		next = messages[n-1]["sequence"]
	}
	for _, m := range messages {
		recipients, err := query(ctx, tx, `SELECT s.name,d.delivered_at,d.read_at FROM deliveries d JOIN sessions s ON s.id=d.recipient_id AND s.project_id=d.project_id WHERE d.project_id=? AND d.message_id=? ORDER BY s.name`, pid, m["id"])
		if err != nil {
			return nil, err
		}
		m["recipients"] = recipients
	}
	if err = s.audit(ctx, tx, wid, pid, "", "threads", id, "project_thread", map[string]any{"count": len(messages), "limit": n, "thread_id": id}); err != nil {
		return nil, err
	}
	return map[string]any{"thread_id": id, "messages": messages, "next": next, "readable_from": from}, nil
}
