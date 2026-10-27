package bus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The message queue's state (MESSAGE-QUEUE-DESIGN §2–§6). A delivery is done
// when its recipient acks it, not when it is fetched. A lease offers it for a
// while; a lease that runs out unacked offers it again, and after
// QueueLimits.Attempts such leases it is a dead letter, kept and listed, and
// the project's coordinator is told. Each recipient numbers its deliveries
// with recipient_seq, gap-free in commit order.

// Schema 35: the queue's columns on deliveries and its counters. The backfill
// numbers existing deliveries by message order and counts what was read as
// handled, so nothing old is offered again or dead-lettered.
const queueSchemaV35 = `CREATE TABLE delivery_counters(project_id TEXT NOT NULL,recipient_id TEXT NOT NULL,seq BIGINT NOT NULL,PRIMARY KEY(project_id,recipient_id));
ALTER TABLE deliveries ADD COLUMN recipient_seq BIGINT;
ALTER TABLE deliveries ADD COLUMN lease_id TEXT;
ALTER TABLE deliveries ADD COLUMN lease_until TEXT;
ALTER TABLE deliveries ADD COLUMN lease_instance TEXT;
ALTER TABLE deliveries ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deliveries ADD COLUMN acked_at TEXT;
ALTER TABLE deliveries ADD COLUMN dead_at TEXT;
ALTER TABLE deliveries ADD COLUMN ack_operation_id TEXT;
UPDATE deliveries SET recipient_seq=n.seq FROM (SELECT d.message_id,d.project_id,d.recipient_id,row_number() OVER (PARTITION BY d.project_id,d.recipient_id ORDER BY m.sequence) AS seq FROM deliveries d JOIN messages m ON m.project_id=d.project_id AND m.id=d.message_id) n WHERE deliveries.message_id=n.message_id AND deliveries.project_id=n.project_id AND deliveries.recipient_id=n.recipient_id;
UPDATE deliveries SET acked_at=read_at WHERE read_at IS NOT NULL;
INSERT INTO delivery_counters(project_id,recipient_id,seq) SELECT project_id,recipient_id,MAX(recipient_seq) FROM deliveries GROUP BY project_id,recipient_id;
CREATE UNIQUE INDEX deliveries_recipient_seq ON deliveries(project_id,recipient_id,recipient_seq);
CREATE INDEX deliveries_lease_due ON deliveries(lease_until) WHERE acked_at IS NULL AND dead_at IS NULL AND lease_id IS NOT NULL;
`

// queueSchemaV35Postgres follows queueSchemaV35 on Postgres, once the backfill
// has numbered every row. SQLite can't add the constraint to a column; there
// insertDeliveries, its only writer, always sets it.
const queueSchemaV35Postgres = `ALTER TABLE deliveries ALTER COLUMN recipient_seq SET NOT NULL;
`

// QueueLimits bounds the queue. Zero fields take the defaults.
type QueueLimits struct {
	Lease    time.Duration // how long a lease offers a delivery (10 min)
	Attempts int           // leases that run out before a delivery is dead (5)
	Prefetch int           // unacked leases a session holds at once (20)
}

// SetQueueLimits sets the queue's bounds; zero fields keep the defaults.
func (s *Store) SetQueueLimits(l QueueLimits) { s.queue = l }

func (s *Store) queueLimits() QueueLimits {
	l := s.queue
	if l.Lease <= 0 {
		l.Lease = 10 * time.Minute
	}
	if l.Attempts <= 0 {
		l.Attempts = 5
	}
	if l.Prefetch <= 0 {
		l.Prefetch = 20
	}
	return l
}

// insertDeliveries is the only writer of deliveries; a test holds every other
// site to it. It numbers each recipient's deliveries from delivery_counters
// in one upsert over the recipients sorted by ID, so crossing multi-recipient
// sends take the counter rows in the same order and wait instead of
// deadlocking. The rows stay locked to commit, so a recipient's sequence is
// its commit order, with no gaps (M1). The counters are their own table so
// heartbeats, which update sessions, never wait on a send.
func insertDeliveries(ctx context.Context, tx *transaction, pid, messageID string, recipients []string) error {
	ids := slices.Clone(recipients)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return nil
	}
	var q strings.Builder
	args := make([]any, 0, 2*len(ids))
	q.WriteString(`INSERT INTO delivery_counters(project_id,recipient_id,seq) VALUES`)
	for i, id := range ids {
		if i > 0 {
			q.WriteByte(',')
		}
		q.WriteString(`(?,?,1)`)
		args = append(args, pid, id)
	}
	q.WriteString(` ON CONFLICT(project_id,recipient_id) DO UPDATE SET seq=delivery_counters.seq+1 RETURNING recipient_id,seq`)
	rows, err := query(ctx, tx, q.String(), args...)
	if err != nil {
		return err
	}
	seq := map[string]int64{}
	for _, r := range rows {
		seq[str(r, "recipient_id")] = r["seq"].(int64)
	}
	for _, id := range ids {
		if seq[id] == 0 {
			return errors.New("delivery counter missing")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO deliveries(message_id,project_id,recipient_id,recipient_seq) VALUES(?,?,?,?)`, messageID, pid, id, seq[id]); err != nil {
			return err
		}
		if err = queueNotify(ctx, tx, pid, id); err != nil {
			return err
		}
	}
	return nil
}

// DeliveryChannel is the Postgres channel a queue change notifies, with the
// payload "<project_id>:<recipient_id>". It says only "look": a woken stream
// reads its queue from the table.
const DeliveryChannel = "cb_delivery"

// queueNotify tells the server's streams that a recipient's queue changed.
// NOTIFY is transactional: a rolled-back change notifies no one, and a
// committed one only after its commit; Postgres folds a transaction's
// repeats into one. SQLite has no streams.
func queueNotify(ctx context.Context, tx *transaction, pid, rid string) error {
	if !tx.postgres {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_notify(?,?)`, DeliveryChannel, pid+":"+rid)
	return err
}

// recipientIDsOf is the id of each session row.
func recipientIDsOf(rows []map[string]any) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, str(r, "id"))
	}
	return ids
}

func queueOperation(op string) bool {
	switch op {
	case "lease", "ack", "nack", "extend", "deadletters_list", "deadletters_requeue", "deadletters_dismiss", "stream_ticket":
		return true
	}
	return false
}

// queueOp runs a queue operation for the actor, the recipient session at its
// current instance: Execute has already refused any other instance (M2).
func (s *Store) queueOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor map[string]any, r Request) (any, error) {
	if actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	a := r.Args
	switch r.Operation {
	case "lease":
		return s.lease(ctx, tx, wid, pid, actor, a)
	case "ack":
		return s.ack(ctx, tx, wid, pid, actor, a)
	case "nack":
		return s.nack(ctx, tx, wid, pid, actor, a)
	case "extend":
		return s.extend(ctx, tx, wid, pid, actor, a)
	case "deadletters_list":
		return s.deadLetters(ctx, tx, pid, projectRole, actor, a)
	case "deadletters_requeue", "deadletters_dismiss":
		return s.settleDeadLetter(ctx, tx, wid, pid, projectRole, actor, r.Operation, a)
	case "stream_ticket":
		return s.streamTicket(ctx, tx, pid, actor, r)
	}
	return nil, errors.New("unsupported operation")
}

// lease offers the session its next deliveries: unacked, not dead, not
// leased and due, in recipient_seq order, up to the prefetch less what it
// already holds. A session whose policy isn't accept takes none, so a
// lease never exists that nobody can handle (§5).
//
// A consumer (the server's stream) also takes over the leases the session
// holds under any other consumer, or none: a stream that replaced another
// is offered what the old one held, under new lease IDs. They keep their
// lease_until (L6), so a takeover loop can't hold a delivery forever
// without an attempt being counted.
func (s *Store) lease(ctx context.Context, tx *transaction, wid, pid string, actor map[string]any, a map[string]any) (any, error) {
	limits := s.queueLimits()
	n := limits.Prefetch
	if v, ok := a["limit"]; ok {
		l, err := integer(v)
		if err != nil || l < 1 || l > 100 {
			return nil, invalid("limit must be 1–100")
		}
		n = min(n, l)
	}
	var consumer any
	if c := str(a, "consumer"); c != "" {
		if len(c) > 64 {
			return nil, invalid("consumer must be at most 64 characters")
		}
		consumer = c
	}
	sid := str(actor, "id")
	now := hostClock().UTC()
	if _, err := s.sweep(ctx, tx, pid, sid, now); err != nil {
		return nil, err
	}
	out := map[string]any{"messages": []map[string]any{}, "policy": actor["notification_policy"]}
	if actor["notification_policy"] != "accept" {
		return out, nil
	}
	nowRaw := layout(now)
	const offered = `SELECT m.sequence,m.id,m.project_id,m.source_project_id,m.sender_id,s.name AS sender,m.address,m.thread_id,m.reply_to,m.kind,m.body,m.coordination,m.created_at,m.expires_at,m.created_by_principal,d.delivered_at,d.read_at,d.recipient_seq,d.attempts,d.lease_until FROM deliveries d JOIN messages m ON m.project_id=d.project_id AND m.id=d.message_id JOIN sessions s ON s.id=m.sender_id AND s.project_id=m.source_project_id WHERE d.project_id=? AND d.recipient_id=? AND d.acked_at IS NULL AND d.dead_at IS NULL AND m.expires_at>? AND `
	var rows []map[string]any
	if consumer != nil {
		taken, err := query(ctx, tx, offered+`d.lease_id IS NOT NULL AND (d.lease_consumer IS NULL OR d.lease_consumer<>?) ORDER BY d.recipient_seq`, pid, sid, nowRaw, consumer)
		if err != nil {
			return nil, err
		}
		rows = taken
	}
	var held int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE project_id=? AND recipient_id=? AND lease_id IS NOT NULL AND acked_at IS NULL AND dead_at IS NULL`, pid, sid).Scan(&held); err != nil {
		return nil, err
	}
	if n -= held; n > 0 {
		fresh, err := query(ctx, tx, offered+`d.lease_id IS NULL AND (d.lease_until IS NULL OR d.lease_until<=?) ORDER BY d.recipient_seq LIMIT ?`, pid, sid, nowRaw, nowRaw, n)
		if err != nil {
			return nil, err
		}
		until := layout(now.Add(limits.Lease))
		for _, m := range fresh {
			m["lease_until"] = until
		}
		rows = append(rows, fresh...)
	}
	for _, m := range rows {
		id := newID("lse_")
		if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET lease_id=?,lease_until=?,lease_instance=?,lease_consumer=?,delivered_at=COALESCE(delivered_at,?) WHERE project_id=? AND message_id=? AND recipient_id=?`, id, m["lease_until"], actor["instance"], consumer, nowRaw, pid, m["id"], sid); err != nil {
			return nil, err
		}
		if m["delivered_at"] == nil {
			m["delivered_at"] = nowRaw
		}
		m["attempt"] = m["attempts"].(int64) + 1
		delete(m, "attempts")
		m["lease_id"] = id
	}
	if len(rows) > 0 {
		ids := make([]string, 0, len(rows))
		for _, m := range rows {
			ids = append(ids, str(m, "id"))
		}
		if err := s.audit(ctx, tx, wid, pid, sid, "delivery", sid, "leased", map[string]any{"ids": ids}); err != nil {
			return nil, err
		}
	}
	out["messages"] = rows
	return out, nil
}

// ackItem is one delivery to ack: a message ID, and the lease it was offered
// under, which may be stale.
type ackItem struct{ messageID, leaseID string }

func ackItems(v any) ([]ackItem, error) {
	list, ok := v.([]any)
	if maps, isMaps := v.([]map[string]any); isMaps {
		list, ok = make([]any, len(maps)), true
		for i, m := range maps {
			list[i] = m
		}
	}
	if !ok || len(list) < 1 || len(list) > 1000 {
		return nil, invalid("acks must hold 1–1000 entries of {message_id, lease_id}")
	}
	out := make([]ackItem, 0, len(list))
	for _, x := range list {
		item, err := ackItemOf(x)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func ackItemOf(v any) (ackItem, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return ackItem{}, invalid("an ack is {message_id, lease_id}")
	}
	for k := range m {
		if k != "message_id" && k != "lease_id" {
			return ackItem{}, invalid("an ack is {message_id, lease_id}")
		}
	}
	id, _ := m["message_id"].(string)
	lease, leaseOK := m["lease_id"].(string)
	if id == "" || (m["lease_id"] != nil && !leaseOK) {
		return ackItem{}, invalid("an ack needs a message_id; lease_id, when given, is text")
	}
	return ackItem{id, lease}, nil
}

// ackOperation reads the optional operation_id an ack is recorded with.
func ackOperation(a map[string]any) (string, error) {
	op := str(a, "operation_id")
	if len(op) > 128 || (a["operation_id"] != nil && op == "") {
		return "", invalid("operation_id must be 1–128 bytes")
	}
	return op, nil
}

func (s *Store) ack(ctx context.Context, tx *transaction, wid, pid string, actor map[string]any, a map[string]any) (any, error) {
	items, err := ackItems(a["acks"])
	if err != nil {
		return nil, err
	}
	op, err := ackOperation(a)
	if err != nil {
		return nil, err
	}
	sid := str(actor, "id")
	out := []map[string]any{}
	acked := []string{}
	for _, item := range items {
		outcome, err := ackDelivery(ctx, tx, pid, sid, item, op)
		if err != nil {
			return nil, err
		}
		if outcome == "acked" {
			acked = append(acked, item.messageID)
		}
		out = append(out, map[string]any{"message_id": item.messageID, "outcome": outcome})
	}
	if len(acked) > 0 {
		if err = s.audit(ctx, tx, wid, pid, sid, "delivery", sid, "acked", map[string]any{"ids": acked}); err != nil {
			return nil, err
		}
	}
	return map[string]any{"acks": out, "acked": len(acked)}, nil
}

// ackDelivery acks one of the session's deliveries: acked, already_acked,
// dead, or not_found for a message that isn't in its queue, another
// session's included (M2). Only the session's current instance gets here
// (Execute's fence), so lease_id is advisory: a stale lease, one that ran out
// and was offered again, is still accepted while the delivery isn't dead.
// The work was done, and handling it again would be the duplicate. Ack is
// handled, so it is also read.
func ackDelivery(ctx context.Context, tx *transaction, pid, sid string, item ackItem, op string) (string, error) {
	now := stamp()
	rows, err := query(ctx, tx, `SELECT d.acked_at,d.dead_at FROM deliveries d JOIN messages m ON m.id=d.message_id AND m.project_id=d.project_id WHERE d.project_id=? AND d.message_id=? AND d.recipient_id=? AND m.expires_at>?`, pid, item.messageID, sid, now)
	switch {
	case err != nil:
		return "", err
	case len(rows) == 0:
		return "not_found", nil
	case rows[0]["acked_at"] != nil:
		return "already_acked", nil
	case rows[0]["dead_at"] != nil:
		return "dead", nil
	}
	var opValue any
	if op != "" {
		opValue = op
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET acked_at=?,ack_operation_id=?,delivered_at=COALESCE(delivered_at,?),read_at=COALESCE(read_at,?) WHERE project_id=? AND message_id=? AND recipient_id=?`, now, opValue, now, now, pid, item.messageID, sid); err != nil {
		return "", err
	}
	// An ack frees a prefetch slot and changes the counts.
	return "acked", queueNotify(ctx, tx, pid, sid)
}

// nack gives a lease back early, offered again after delay_seconds, without
// counting an attempt: used before a compaction (§5). Only the current lease
// can be given back.
func (s *Store) nack(ctx context.Context, tx *transaction, wid, pid string, actor map[string]any, a map[string]any) (any, error) {
	id, lease := str(a, "message_id"), str(a, "lease_id")
	if id == "" || lease == "" {
		return nil, invalid("nack needs message_id and lease_id")
	}
	delay := 0
	if v, ok := a["delay_seconds"]; ok {
		d, err := integer(v)
		if err != nil || d < 0 || d > 3600 {
			return nil, invalid("delay_seconds must be 0–3600")
		}
		delay = d
	}
	sid := str(actor, "id")
	now := hostClock().UTC()
	if _, err := s.sweep(ctx, tx, pid, sid, now); err != nil {
		return nil, err
	}
	rows, err := query(ctx, tx, `SELECT d.lease_id,d.acked_at,d.dead_at FROM deliveries d JOIN messages m ON m.id=d.message_id AND m.project_id=d.project_id WHERE d.project_id=? AND d.message_id=? AND d.recipient_id=? AND m.expires_at>?`, pid, id, sid, layout(now))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"message_id": id}
	switch {
	case len(rows) == 0:
		out["outcome"] = "not_found"
	case rows[0]["acked_at"] != nil:
		out["outcome"] = "already_acked"
	case rows[0]["dead_at"] != nil:
		out["outcome"] = "dead"
	case rows[0]["lease_id"] != lease:
		out["outcome"] = "stale"
	default:
		var after any
		if delay > 0 {
			after = layout(now.Add(time.Duration(delay) * time.Second))
		}
		if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET lease_id=NULL,lease_instance=NULL,lease_consumer=NULL,lease_until=? WHERE project_id=? AND message_id=? AND recipient_id=?`, after, pid, id, sid); err != nil {
			return nil, err
		}
		// A delayed release is found by the stream's periodic re-read.
		if err = queueNotify(ctx, tx, pid, sid); err != nil {
			return nil, err
		}
		if err = s.audit(ctx, tx, wid, pid, sid, "delivery", sid, "released", map[string]any{"ids": []string{id}, "delay_seconds": delay}); err != nil {
			return nil, err
		}
		out["outcome"], out["available_at"] = "released", after
	}
	return out, nil
}

// extend renews the session's current leases for up to an hour each, for
// long handling. A lease that has run out was offered again, so it's gone.
func (s *Store) extend(ctx context.Context, tx *transaction, wid, pid string, actor map[string]any, a map[string]any) (any, error) {
	leases, isArray, ok := stringList(a["leases"])
	if !isArray || !ok || len(leases) < 1 || len(leases) > 1000 {
		return nil, invalid("leases must hold 1–1000 lease IDs")
	}
	seconds, err := integer(a["seconds"])
	if err != nil || seconds < 1 || seconds > 3600 {
		return nil, invalid("seconds must be 1–3600")
	}
	sid := str(actor, "id")
	now := hostClock().UTC()
	if _, err = s.sweep(ctx, tx, pid, sid, now); err != nil {
		return nil, err
	}
	until := layout(now.Add(time.Duration(seconds) * time.Second))
	out := []map[string]any{}
	for _, lease := range leases {
		res, err := tx.ExecContext(ctx, `UPDATE deliveries SET lease_until=? WHERE project_id=? AND recipient_id=? AND lease_id=? AND acked_at IS NULL AND dead_at IS NULL`, until, pid, sid, lease)
		if err != nil {
			return nil, err
		}
		item := map[string]any{"lease_id": lease, "outcome": "not_found"}
		if n, _ := res.RowsAffected(); n == 1 {
			item["outcome"], item["lease_until"] = "extended", until
		}
		out = append(out, item)
	}
	return map[string]any{"leases": out}, nil
}

// SweepResult is what one sweep did.
type SweepResult struct {
	Skipped   bool // another replica holds the sweep
	Reoffered int  // leases that ran out and are offered again
	Dead      int  // deliveries that became dead letters
}

// SweepDeliveries offers again every delivery whose lease ran out, or makes it
// a dead letter at the limit, in one transaction. On Postgres it runs under a
// transaction-scoped advisory lock, so a second replica skips the sweep and a
// pooled connection can neither keep the lock nor lose it (L2).
func (s *Store) SweepDeliveries(ctx context.Context) (SweepResult, error) {
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SweepResult{}, err
	}
	tx := &transaction{Tx: raw, postgres: s.postgres}
	defer tx.Rollback()
	if s.postgres {
		var got bool
		if err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('cb:sweeper',0))`).Scan(&got); err != nil {
			return SweepResult{}, err
		}
		if !got {
			return SweepResult{Skipped: true}, nil
		}
	}
	sweeper := *s
	sweeper.principal = systemPrincipal
	res, err := sweeper.sweep(ctx, tx, "", "", hostClock().UTC())
	if err != nil {
		return SweepResult{}, err
	}
	return res, tx.Commit()
}

// sweep handles the run-out leases of one session, or of every session when
// pid is empty. Each row is changed only while it is still due, so a sweep
// racing another never counts an attempt twice.
func (s *Store) sweep(ctx context.Context, tx *transaction, pid, sid string, now time.Time) (SweepResult, error) {
	var res SweepResult
	nowRaw := layout(now)
	q := `SELECT d.project_id,d.message_id,d.recipient_id,d.lease_id,d.attempts,m.kind FROM deliveries d JOIN messages m ON m.project_id=d.project_id AND m.id=d.message_id WHERE d.acked_at IS NULL AND d.dead_at IS NULL AND d.lease_id IS NOT NULL AND d.lease_until<=?`
	args := []any{nowRaw}
	if pid != "" {
		q += ` AND d.project_id=? AND d.recipient_id=?`
		args = append(args, pid, sid)
	}
	due, err := query(ctx, tx, q+` ORDER BY d.project_id,d.recipient_id,d.recipient_seq`, args...)
	if err != nil || len(due) == 0 {
		return res, err
	}
	limit := int64(s.queueLimits().Attempts)
	type queueKey struct{ pid, sid string }
	dead := map[queueKey][]map[string]any{}
	var order []queueKey
	for _, d := range due {
		attempts := d["attempts"].(int64) + 1
		var deadAt any
		if attempts >= limit {
			deadAt = nowRaw
		}
		r, err := tx.ExecContext(ctx, `UPDATE deliveries SET lease_id=NULL,lease_instance=NULL,lease_consumer=NULL,lease_until=NULL,attempts=?,dead_at=? WHERE project_id=? AND message_id=? AND recipient_id=? AND lease_id=? AND acked_at IS NULL AND dead_at IS NULL`, attempts, deadAt, d["project_id"], d["message_id"], d["recipient_id"], d["lease_id"])
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			continue
		}
		if err = queueNotify(ctx, tx, str(d, "project_id"), str(d, "recipient_id")); err != nil {
			return res, err
		}
		if deadAt == nil {
			res.Reoffered++
			continue
		}
		res.Dead++
		k := queueKey{str(d, "project_id"), str(d, "recipient_id")}
		if dead[k] == nil {
			order = append(order, k)
		}
		dead[k] = append(dead[k], d)
	}
	// The history and the notice are the server's, whoever's request swept.
	actor := tx.actorID
	tx.actorID = ""
	defer func() { tx.actorID = actor }()
	for _, k := range order {
		if err = s.deadLettered(ctx, tx, k.pid, k.sid, dead[k], now); err != nil {
			return res, err
		}
	}
	return res, nil
}

// deadLetterNoticeEvery throttles the coordinator's notices: one per
// recipient in this window, counting what died since.
const deadLetterNoticeEvery = 10 * time.Minute

// deadLettered records a recipient's new dead letters and tells the
// project's coordinator, with a count and the session's name only. The
// coordinator's own dead letters, and dead notices, make no notice (L1): the
// owner sees those in doctor and the TUI.
func (s *Store) deadLettered(ctx context.Context, tx *transaction, pid, sid string, rows []map[string]any, now time.Time) error {
	p, err := one(ctx, tx, `SELECT p.workspace_id,p.coordinator_session,w.retention_days,r.name FROM projects p JOIN workspaces w ON w.id=p.workspace_id JOIN sessions r ON r.project_id=p.id AND r.id=? WHERE p.id=?`, sid, pid)
	if err != nil {
		return err
	}
	wid := str(p, "workspace_id")
	ids, notify := []string{}, 0
	for _, d := range rows {
		ids = append(ids, str(d, "message_id"))
		if str(d, "kind") != deadLetterKind {
			notify++
		}
	}
	if err = s.audit(ctx, tx, wid, pid, "", "delivery", sid, "dead", map[string]any{"ids": ids}); err != nil {
		return err
	}
	coordinator := str(p, "coordinator_session")
	if notify == 0 || coordinator == "" || coordinator == sid {
		return nil
	}
	target, err := query(ctx, tx, `SELECT id,name FROM sessions WHERE project_id=? AND id=? AND status='active'`, pid, coordinator)
	if err != nil || len(target) == 0 {
		return err
	}
	recent, err := query(ctx, tx, `SELECT 1 FROM history WHERE workspace_id=? AND project_id=? AND entity='delivery' AND entity_key=? AND action='dead_letter_notice' AND changed_at>?`, wid, pid, sid, layout(now.Add(-deadLetterNoticeEvery)))
	if err != nil || len(recent) > 0 {
		return err
	}
	var waiting int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE project_id=? AND recipient_id=? AND dead_at IS NOT NULL AND acked_at IS NULL`, pid, sid).Scan(&waiting); err != nil {
		return err
	}
	if err = s.ensureSystemSession(ctx, tx, wid, pid); err != nil {
		return err
	}
	system, err := one(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, SystemSessionName)
	if err != nil {
		return err
	}
	name := str(p, "name")
	body := fmt.Sprintf("%d ClawBridge message(s) to session %s went unhandled after %d offers and are dead letters (%d waiting in all). They are kept: the session, you or a project owner can list them (deadletters_list), requeue or dismiss them.", len(ids), name, s.queueLimits().Attempts, waiting)
	retention := asInt(p["retention_days"])
	if retention < 1 {
		retention = 30
	}
	id := newID("msg_")
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,?,?,?,?,?,?,?,NULL)`, id, pid, pid, system["id"], str(target[0], "name"), id, deadLetterKind, body, layout(now), layout(now.AddDate(0, 0, retention)), systemPrincipal, newID("dead_"), digestOf(body)); err != nil {
		return err
	}
	if err = insertDeliveries(ctx, tx, pid, id, []string{coordinator}); err != nil {
		return err
	}
	return s.audit(ctx, tx, wid, pid, "", "delivery", sid, "dead_letter_notice", map[string]any{"message_id": id, "count": len(ids)})
}

// deadLetterKind is the kind of the coordinator's dead-letter notice.
const deadLetterKind = "dead-letter"

// deadLetterReader says whether the actor may see and settle the dead letters
// of the session sid: that session itself, the project's coordinator, or a
// project owner.
func deadLetterReader(ctx context.Context, tx *transaction, pid, projectRole string, actor map[string]any, sid string) (bool, error) {
	if sid == str(actor, "id") || projectRole == "owner" {
		return true, nil
	}
	rows, err := query(ctx, tx, `SELECT 1 FROM projects WHERE id=? AND coordinator_session=?`, pid, actor["id"])
	return len(rows) > 0, err
}

// deadLetterTarget resolves target_session: the actor when absent.
func deadLetterTarget(ctx context.Context, tx *transaction, pid string, actor map[string]any, a map[string]any) (string, error) {
	selector := str(a, "target_session")
	if selector == "" {
		return str(actor, "id"), nil
	}
	match, args := sessionAddress(selector)
	target, err := one(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND `+match, append([]any{pid}, args...)...)
	if err != nil {
		return "", scopedError(err, "session not found in this project", "not_found")
	}
	return str(target, "id"), nil
}

// deadLetters lists dead letters, oldest first: the session's own, another
// session's (target_session), or the whole project's (target_session @all).
// A row has no body: the coordinator or an owner may read another session's
// queue, and the notice carries no subject either.
func (s *Store) deadLetters(ctx context.Context, tx *transaction, pid, projectRole string, actor map[string]any, a map[string]any) (any, error) {
	n, err := limit(a)
	if err != nil {
		return nil, err
	}
	q := `SELECT d.message_id,d.recipient_id,r.name AS recipient,m.sender_id,s.name AS sender,m.kind,m.created_at,d.recipient_seq,d.attempts,d.dead_at FROM deliveries d JOIN messages m ON m.project_id=d.project_id AND m.id=d.message_id JOIN sessions r ON r.project_id=d.project_id AND r.id=d.recipient_id JOIN sessions s ON s.id=m.sender_id AND s.project_id=m.source_project_id WHERE d.project_id=? AND d.dead_at IS NOT NULL AND d.acked_at IS NULL AND m.expires_at>?`
	args := []any{pid, stamp()}
	if str(a, "target_session") == "@all" {
		if ok, err := deadLetterReader(ctx, tx, pid, projectRole, actor, ""); err != nil || !ok {
			return nil, deadLetterRefusal(err)
		}
	} else {
		sid, err := deadLetterTarget(ctx, tx, pid, actor, a)
		if err != nil {
			return nil, err
		}
		if ok, err := deadLetterReader(ctx, tx, pid, projectRole, actor, sid); err != nil || !ok {
			return nil, deadLetterRefusal(err)
		}
		q += ` AND d.recipient_id=?`
		args = append(args, sid)
	}
	rows, err := query(ctx, tx, q+` ORDER BY d.dead_at,d.recipient_id,d.recipient_seq LIMIT ?`, append(args, n+1)...)
	if err != nil {
		return nil, err
	}
	more := len(rows) > n
	if more {
		rows = rows[:n]
	}
	return map[string]any{"dead_letters": rows, "has_more": more}, nil
}

func deadLetterRefusal(err error) error {
	if err != nil {
		return err
	}
	return &serviceError{code: "forbidden", cause: errors.New("only the session, the project's coordinator or a project owner may see its dead letters"), public: true}
}

// settleDeadLetter requeues a dead letter, offered again from its first
// attempt, or dismisses it, which acks it. History records who did it,
// since it may be someone else's queue (L5).
func (s *Store) settleDeadLetter(ctx context.Context, tx *transaction, wid, pid, projectRole string, actor map[string]any, op string, a map[string]any) (any, error) {
	id := str(a, "message_id")
	if id == "" {
		return nil, invalid("message_id is required")
	}
	sid, err := deadLetterTarget(ctx, tx, pid, actor, a)
	if err != nil {
		return nil, err
	}
	if ok, err := deadLetterReader(ctx, tx, pid, projectRole, actor, sid); err != nil || !ok {
		return nil, deadLetterRefusal(err)
	}
	now := stamp()
	rows, err := query(ctx, tx, `SELECT d.dead_at,d.acked_at FROM deliveries d JOIN messages m ON m.id=d.message_id AND m.project_id=d.project_id WHERE d.project_id=? AND d.message_id=? AND d.recipient_id=? AND m.expires_at>?`, pid, id, sid, now)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0]["dead_at"] == nil || rows[0]["acked_at"] != nil {
		return nil, &serviceError{code: "not_found", cause: errors.New("no such dead letter in this session's queue"), public: true}
	}
	action, outcome := "requeued", "requeued"
	q := `UPDATE deliveries SET dead_at=NULL,attempts=0,lease_id=NULL,lease_instance=NULL,lease_consumer=NULL,lease_until=NULL WHERE project_id=? AND message_id=? AND recipient_id=?`
	args := []any{pid, id, sid}
	if op == "deadletters_dismiss" {
		action, outcome = "dismissed", "dismissed"
		q = `UPDATE deliveries SET acked_at=?,read_at=COALESCE(read_at,?) WHERE project_id=? AND message_id=? AND recipient_id=?`
		args = []any{now, now, pid, id, sid}
	}
	if _, err = tx.ExecContext(ctx, q, args...); err != nil {
		return nil, err
	}
	if err = queueNotify(ctx, tx, pid, sid); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, str(actor, "id"), "delivery", sid, action, map[string]any{"ids": []string{id}}); err != nil {
		return nil, err
	}
	return map[string]any{"message_id": id, "recipient_id": sid, "outcome": outcome}, nil
}

// queueCounts is a session's unacked and dead deliveries, for
// notification_status.
func queueCounts(ctx context.Context, tx *transaction, pid, sid string) (unacked, dead int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN d.dead_at IS NULL THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN d.dead_at IS NOT NULL THEN 1 ELSE 0 END),0) FROM deliveries d JOIN messages m ON m.project_id=d.project_id AND m.id=d.message_id WHERE d.project_id=? AND d.recipient_id=? AND d.acked_at IS NULL AND m.expires_at>?`, pid, sid, stamp()).Scan(&unacked, &dead)
	return unacked, dead, err
}
