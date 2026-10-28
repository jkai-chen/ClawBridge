package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Schema 40 (taken at landing): the hosted server's durable outbox for the
// Linear writes it makes as the app (live/LINEAR-QUEUE-DESIGN.md §4). A write
// that can't go now (Linear's rate limit, the project's hourly cap, or an
// earlier write on the same issue still queued) waits here and is sent by
// the server's worker through the same path a direct write takes, so the
// operation ledger stays the truth on whether it was sent. request holds the
// write as accepted, bodies included: it is read by the worker only and
// never shown (R3).
const linearOutboxSchema = `
CREATE TABLE linear_write_outbox(id TEXT PRIMARY KEY,project_id TEXT NOT NULL,session_id TEXT NOT NULL,principal TEXT NOT NULL,operation_id TEXT NOT NULL,digest TEXT NOT NULL,request TEXT NOT NULL,operation TEXT NOT NULL,issue TEXT NOT NULL,order_key TEXT NOT NULL,reason TEXT NOT NULL,state TEXT NOT NULL,attempts INTEGER NOT NULL DEFAULT 0,next_attempt_at TEXT NOT NULL,lease_owner TEXT,lease_until TEXT,created_at TEXT NOT NULL,expires_at TEXT NOT NULL,settled_at TEXT,result TEXT,notified_at TEXT,UNIQUE(project_id,session_id,operation_id));
CREATE INDEX linear_write_outbox_due ON linear_write_outbox(state,next_attempt_at);
CREATE INDEX linear_write_outbox_order ON linear_write_outbox(project_id,order_key,created_at);
`

// Outbox states. queued and sending are unsettled; the rest are final.
const (
	OutboxQueued    = "queued"
	OutboxSending   = "sending"
	OutboxDone      = "done"
	OutboxFailed    = "failed"
	OutboxUncertain = "uncertain"
	OutboxDead      = "dead"
	OutboxExpired   = "expired"
)

const (
	// OutboxMaxAttempts is how many "not sent" tries a write gets before it
	// is dead.
	OutboxMaxAttempts = 8
	// OutboxKeep is how long a settled write is kept for write-status.
	OutboxKeep = 7 * 24 * time.Hour
	// outboxDeadEvery is how often the coordinator hears of dead writes.
	outboxDeadEvery = 10 * time.Minute
)

// outboxScope limits the worker's queries to one project when set: tests
// share one database with other packages' workers.
func (s *Store) outboxScope(alias string) (string, []any) {
	if s.outboxOnly == "" {
		return "", nil
	}
	return " AND " + alias + "project_id=?", []any{s.outboxOnly}
}

// OutboxAvailable says whether this store can queue writes: the outbox
// is the hosted server's, on Postgres.
func (s *Store) OutboxAvailable() bool { return s.postgres }

// OutboxWrite is one queued Linear write. Request is the server's own
// record of the write, bodies included, and never leaves the server.
type OutboxWrite struct {
	ID, ProjectID, SessionID, Principal, OperationID, Digest string
	Request                                                  string
	Operation, Issue, OrderKey, Reason, State                string
	Attempts                                                 int
	NextAttemptAt, CreatedAt, ExpiresAt, SettledAt           time.Time
	// Result is the settled write's typed summary: the receipt's kind and
	// IDs, or the refusal's reason. Never a body.
	Result string
	// Position is the write's place on its issue: 1 is next.
	Position int
}

// View is the write's typed fields, for write-status and doctor: never its
// request (R3).
func (w OutboxWrite) View() map[string]any {
	v := map[string]any{"operation_id": w.OperationID, "operation": w.Operation, "issue": w.Issue, "state": w.State, "reason": w.Reason, "attempts": w.Attempts, "queued_at": w.CreatedAt.UTC().Format(time.RFC3339), "expires_at": w.ExpiresAt.UTC().Format(time.RFC3339)}
	if w.State == OutboxQueued || w.State == OutboxSending {
		v["position"] = w.Position
		v["next_attempt_at"] = w.NextAttemptAt.UTC().Format(time.RFC3339)
	} else {
		v["settled_at"] = w.SettledAt.UTC().Format(time.RFC3339)
		if w.Result != "" {
			v["result"] = w.Result
		}
	}
	return v
}

const outboxColumns = `id,project_id,session_id,principal,operation_id,digest,request,operation,issue,order_key,reason,state,attempts,next_attempt_at,created_at,expires_at,settled_at,result`

func outboxRow(r map[string]any) OutboxWrite {
	return OutboxWrite{ID: str(r, "id"), ProjectID: str(r, "project_id"), SessionID: str(r, "session_id"), Principal: str(r, "principal"), OperationID: str(r, "operation_id"), Digest: str(r, "digest"), Request: str(r, "request"), Operation: str(r, "operation"), Issue: str(r, "issue"), OrderKey: str(r, "order_key"), Reason: str(r, "reason"), State: str(r, "state"), Attempts: asInt(r["attempts"]), NextAttemptAt: at(str(r, "next_attempt_at")), CreatedAt: at(str(r, "created_at")), ExpiresAt: at(str(r, "expires_at")), SettledAt: at(str(r, "settled_at")), Result: str(r, "result")}
}

// unsettledAhead counts the unsettled writes ahead of w on its order key.
const unsettledAhead = `SELECT COUNT(*) AS n FROM linear_write_outbox o2 WHERE o2.project_id=? AND o2.order_key=? AND o2.state IN ('queued','sending') AND (o2.created_at<? OR (o2.created_at=? AND o2.id<?))`

func (s *Store) outboxPosition(ctx context.Context, tx *transaction, w *OutboxWrite) error {
	if w.State != OutboxQueued && w.State != OutboxSending {
		return nil
	}
	rows, err := query(ctx, tx, unsettledAhead, w.ProjectID, w.OrderKey, layout(w.CreatedAt), layout(w.CreatedAt), w.ID)
	if err != nil {
		return err
	}
	w.Position = asInt(rows[0]["n"]) + 1
	return nil
}

// OutboxEnqueue queues w, or returns the write already queued for its
// operation_id and reports created false. The same operation_id with a
// different request is refused, as the ledger refuses it.
func (s *Store) OutboxEnqueue(ctx context.Context, w OutboxWrite) (OutboxWrite, bool, error) {
	if !ledgerOperationID.MatchString(w.OperationID) || w.ProjectID == "" || w.Digest == "" || w.OrderKey == "" {
		return OutboxWrite{}, false, invalid("invalid queued write")
	}
	w.ID, w.State = newID("lwo_"), OutboxQueued
	created := false
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		r, err := tx.ExecContext(ctx, `INSERT INTO linear_write_outbox(id,project_id,session_id,principal,operation_id,digest,request,operation,issue,order_key,reason,state,attempts,next_attempt_at,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?) ON CONFLICT(project_id,session_id,operation_id) DO NOTHING`,
			w.ID, w.ProjectID, w.SessionID, w.Principal, w.OperationID, w.Digest, w.Request, w.Operation, w.Issue, w.OrderKey, w.Reason, w.State, layout(w.NextAttemptAt), layout(w.CreatedAt), layout(w.ExpiresAt))
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 1 {
			created = true
			return s.outboxPosition(ctx, tx, &w)
		}
		got, ok, err := s.outboxGet(ctx, tx, w.ProjectID, w.SessionID, w.OperationID)
		if err != nil || !ok {
			return cmpErr(err, errors.New("queued write vanished"))
		}
		if got.Digest != w.Digest {
			return invalid("operation_id was already used for a different write; use a new operation_id")
		}
		w = got
		return nil
	})
	return w, created, err
}

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func (s *Store) outboxGet(ctx context.Context, tx *transaction, project, session, operationID string) (OutboxWrite, bool, error) {
	rows, err := query(ctx, tx, `SELECT `+outboxColumns+` FROM linear_write_outbox WHERE project_id=? AND session_id=? AND operation_id=?`, project, session, operationID)
	if err != nil || len(rows) == 0 {
		return OutboxWrite{}, false, err
	}
	w := outboxRow(rows[0])
	return w, true, s.outboxPosition(ctx, tx, &w)
}

// OutboxGet is the write queued for an operation_id, if any.
func (s *Store) OutboxGet(ctx context.Context, project, session, operationID string) (w OutboxWrite, ok bool, err error) {
	err = s.ledgerTx(ctx, func(tx *transaction) error {
		w, ok, err = s.outboxGet(ctx, tx, project, session, operationID)
		return err
	})
	return w, ok, err
}

// OutboxBehind is how many unsettled writes wait on an order key: a direct
// write on it queues behind them rather than overtake (§4.3).
func (s *Store) OutboxBehind(ctx context.Context, project, orderKey string) (n int, err error) {
	err = s.ledgerTx(ctx, func(tx *transaction) error {
		rows, err := query(ctx, tx, `SELECT COUNT(*) AS n FROM linear_write_outbox WHERE project_id=? AND order_key=? AND state IN ('queued','sending')`, project, orderKey)
		if err == nil {
			n = asInt(rows[0]["n"])
		}
		return err
	})
	return n, err
}

// OutboxLease takes up to limit due writes for owner until now+lease: only
// the head of each order key, so a key's writes go in order. Rows another
// worker holds are skipped, never waited for; no workspace lock is taken
// (§7).
func (s *Store) OutboxLease(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]OutboxWrite, error) {
	var out []OutboxWrite
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		scope, a := s.outboxScope("o.")
		rows, err := query(ctx, tx, `SELECT `+outboxColumns+` FROM linear_write_outbox o WHERE o.state='queued' AND o.next_attempt_at<=? AND o.expires_at>?`+scope+` AND NOT EXISTS(SELECT 1 FROM linear_write_outbox o2 WHERE o2.project_id=o.project_id AND o2.order_key=o.order_key AND o2.state IN ('queued','sending') AND (o2.created_at<o.created_at OR (o2.created_at=o.created_at AND o2.id<o.id))) ORDER BY o.created_at,o.id LIMIT ? FOR UPDATE SKIP LOCKED`, append(append([]any{layout(now), layout(now)}, a...), limit)...)
		if err != nil {
			return err
		}
		for _, r := range rows {
			w := outboxRow(r)
			if _, err = tx.ExecContext(ctx, `UPDATE linear_write_outbox SET state='sending',lease_owner=?,lease_until=? WHERE id=? AND state='queued'`, owner, layout(now.Add(lease)), w.ID); err != nil {
				return err
			}
			w.State, w.Position = OutboxSending, 1
			out = append(out, w)
		}
		return nil
	})
	return out, err
}

// OutboxSettle records a leased write's final state and its typed result.
// It reports false when owner no longer holds the lease.
func (s *Store) OutboxSettle(ctx context.Context, id, owner, state, reason, result string, now time.Time) (bool, error) {
	switch state {
	case OutboxDone, OutboxFailed, OutboxUncertain:
	default:
		return false, fmt.Errorf("not a settled state: %q", state)
	}
	return s.outboxUpdate(ctx, `UPDATE linear_write_outbox SET state=?,reason=?,result=?,settled_at=?,lease_owner=NULL,lease_until=NULL WHERE id=? AND state='sending' AND lease_owner=?`, state, reason, result, layout(now), id, owner)
}

// OutboxRetry puts a leased write that was not sent back in the queue
// until next. A retry counts against OutboxMaxAttempts and past it the
// write is dead; a wait (the cap or the pacer said not yet, before any
// send) doesn't count.
func (s *Store) OutboxRetry(ctx context.Context, id, owner, reason string, next time.Time, retry bool, now time.Time) (string, error) {
	state := OutboxQueued
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		rows, err := query(ctx, tx, `SELECT attempts FROM linear_write_outbox WHERE id=? AND state='sending' AND lease_owner=? FOR UPDATE`, id, owner)
		if err != nil || len(rows) == 0 {
			state = ""
			return err
		}
		attempts := asInt(rows[0]["attempts"])
		if retry {
			attempts++
		}
		if attempts >= OutboxMaxAttempts {
			state = OutboxDead
			_, err = tx.ExecContext(ctx, `UPDATE linear_write_outbox SET state='dead',attempts=?,reason=?,settled_at=?,lease_owner=NULL,lease_until=NULL WHERE id=?`, attempts, reason, layout(now), id)
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE linear_write_outbox SET state='queued',attempts=?,reason=?,next_attempt_at=?,lease_owner=NULL,lease_until=NULL WHERE id=?`, attempts, reason, layout(next), id)
		return err
	})
	return state, err
}

// OutboxReclaim puts writes whose lease ran out back in the queue. Sending
// again is safe: the lease (90 s) outlasts the send (60 s) and the
// ledger's in-progress window (75 s), so the ledger by then answers a
// started send as accepted, refused or uncertain and sends nothing (R4).
func (s *Store) OutboxReclaim(ctx context.Context, now time.Time) (int, error) {
	var n int64
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		scope, a := s.outboxScope("")
		r, err := tx.ExecContext(ctx, `UPDATE linear_write_outbox SET state='queued',next_attempt_at=?,lease_owner=NULL,lease_until=NULL WHERE state='sending' AND lease_until<=?`+scope, append([]any{layout(now), layout(now)}, a...)...)
		if err == nil {
			n, _ = r.RowsAffected()
		}
		return err
	})
	return int(n), err
}

// OutboxExpire settles queued writes past their expiry as expired: a write
// is never sent after it, since the agent's intent may be stale. It
// returns their IDs for the notice.
func (s *Store) OutboxExpire(ctx context.Context, now time.Time) ([]string, error) {
	var ids []string
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		scope, a := s.outboxScope("")
		rows, err := query(ctx, tx, `SELECT id FROM linear_write_outbox WHERE state='queued' AND expires_at<=?`+scope+` FOR UPDATE SKIP LOCKED`, append([]any{layout(now)}, a...)...)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if _, err = tx.ExecContext(ctx, `UPDATE linear_write_outbox SET state='expired',reason='expired',settled_at=? WHERE id=? AND state='queued'`, layout(now), str(r, "id")); err != nil {
				return err
			}
			ids = append(ids, str(r, "id"))
		}
		return nil
	})
	return ids, err
}

// OutboxPurgeProject deletes every write of a project, queued or not, and
// with it their text (R3): for a project's removal.
func (s *Store) OutboxPurgeProject(ctx context.Context, project string) error {
	return s.ledgerTx(ctx, func(tx *transaction) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM linear_write_outbox WHERE project_id=?`, project)
		return err
	})
}

// OutboxPrune deletes writes settled longer than OutboxKeep ago.
func (s *Store) OutboxPrune(ctx context.Context, now time.Time) error {
	return s.ledgerTx(ctx, func(tx *transaction) error {
		scope, a := s.outboxScope("")
		_, err := tx.ExecContext(ctx, `DELETE FROM linear_write_outbox WHERE settled_at IS NOT NULL AND settled_at<?`+scope, append([]any{layout(now.Add(-OutboxKeep))}, a...)...)
		return err
	})
}

func (s *Store) outboxUpdate(ctx context.Context, q string, a ...any) (bool, error) {
	ok := false
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		r, err := tx.ExecContext(ctx, q, a...)
		if err == nil {
			n, _ := r.RowsAffected()
			ok = n == 1
		}
		return err
	})
	return ok, err
}

// OutboxList is a project's queued writes and those settled in the last
// OutboxKeep, newest first, for one session or (session "") one person's
// sessionless writes: typed fields only.
func (s *Store) OutboxList(ctx context.Context, project, session, principal string, limit int) ([]OutboxWrite, error) {
	var out []OutboxWrite
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		rows, err := query(ctx, tx, `SELECT `+outboxColumns+` FROM linear_write_outbox WHERE project_id=? AND session_id=? AND (session_id<>'' OR principal=?) ORDER BY created_at DESC,id DESC LIMIT ?`, project, session, principal, limit)
		if err != nil {
			return err
		}
		for _, r := range rows {
			w := outboxRow(r)
			if err = s.outboxPosition(ctx, tx, &w); err != nil {
				return err
			}
			out = append(out, w)
		}
		return nil
	})
	return out, err
}

// OutboxStatus is a project's outbox in numbers, for linear_app_status and
// doctor.
type OutboxStatus struct {
	Queued       int `json:"queued"`
	Sending      int `json:"sending"`
	OldestQueued int `json:"oldest_queued_age_s"`
	// Waiting is the reason most queued writes wait for.
	Waiting       string `json:"waiting,omitempty"`
	Dead24h       int    `json:"dead_24h"`
	Expired24h    int    `json:"expired_24h"`
	Uncertain24h  int    `json:"uncertain_24h"`
	oldestQueueAt string
}

// OutboxStatusOf counts a project's outbox at now.
func (s *Store) OutboxStatusOf(ctx context.Context, project string, now time.Time) (OutboxStatus, error) {
	var st OutboxStatus
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		rows, err := query(ctx, tx, `SELECT state,reason,COUNT(*) AS n,MIN(created_at) AS oldest FROM linear_write_outbox WHERE project_id=? AND (state IN ('queued','sending') OR settled_at>=?) GROUP BY state,reason`, project, layout(now.Add(-24*time.Hour)))
		if err != nil {
			return err
		}
		most := 0
		for _, r := range rows {
			n := asInt(r["n"])
			switch str(r, "state") {
			case OutboxQueued:
				st.Queued += n
				if n > most {
					most, st.Waiting = n, str(r, "reason")
				}
				if o := str(r, "oldest"); st.oldestQueueAt == "" || o < st.oldestQueueAt {
					st.oldestQueueAt = o
				}
			case OutboxSending:
				st.Sending += n
			case OutboxDead:
				st.Dead24h += n
			case OutboxExpired:
				st.Expired24h += n
			case OutboxUncertain:
				st.Uncertain24h += n
			}
		}
		if st.oldestQueueAt != "" {
			st.OldestQueued = int(now.Sub(at(st.oldestQueueAt)).Seconds())
		}
		return nil
	})
	return st, err
}

// OutboxNotify tells the session that asked for a settled write how it
// ended, once: a linear-write message of typed fields only, never the
// write's text (R3). A sessionless write, or one whose session is no
// longer active, is recorded only. A dead write also goes to the
// project's coordinator.
func (s *Store) OutboxNotify(ctx context.Context, id string, now time.Time) error {
	if !s.postgres {
		return errors.New("the Linear write outbox needs a hosted server")
	}
	rows, err := s.outboxNotifyRow(ctx, id)
	if err != nil || len(rows) == 0 {
		return err
	}
	w := outboxRow(rows[0])
	tx, p, err := s.systemTx(ctx, w.ProjectID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Claimed in this transaction, so a notice is sent once.
	r, err := tx.ExecContext(ctx, `UPDATE linear_write_outbox SET notified_at=? WHERE id=? AND notified_at IS NULL AND settled_at IS NOT NULL`, layout(now), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return nil
	}
	wid, pid := str(p, "workspace_id"), w.ProjectID
	if err = s.ensureSystemSession(ctx, tx, wid, pid); err != nil {
		return err
	}
	sys, err := one(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, SystemSessionName)
	if err != nil {
		return err
	}
	var recipients []map[string]any
	if w.SessionID != "" {
		if recipients, err = query(ctx, tx, `SELECT id,name FROM sessions WHERE project_id=? AND id=? AND status='active'`, pid, w.SessionID); err != nil {
			return err
		}
	}
	coordinator := false
	if w.State == OutboxDead {
		// The coordinator hears of dead writes at most once in 10 minutes
		// per project, as of dead letters.
		told, err := query(ctx, tx, `SELECT 1 FROM history WHERE project_id=? AND entity='linear_write' AND action='dead' AND changed_at>=? AND details LIKE '%"coordinator":true%'`, pid, layout(now.Add(-outboxDeadEvery)))
		if err != nil {
			return err
		}
		if len(told) == 0 {
			coordinators, err := s.coordinatorTargets(ctx, tx, pid, "", nil)
			if err != nil {
				return err
			}
			for _, c := range coordinators {
				if str(c, "id") != w.SessionID {
					recipients, coordinator = append(recipients, c), true
				}
			}
		}
	}
	if len(recipients) > 0 {
		body := outboxNotice(w)
		w2, err := one(ctx, tx, `SELECT retention_days FROM workspaces WHERE id=?`, wid)
		if err != nil {
			return err
		}
		retention := asInt(w2["retention_days"])
		if retention < 1 {
			retention = 30
		}
		msg, names := newID("msg_"), []string{}
		for _, r := range recipients {
			names = append(names, str(r, "name"))
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'linear-write',?,?,?,?,?,?,NULL)`, msg, pid, pid, str(sys, "id"), strings.Join(names, ","), msg, body, layout(now), layout(now.AddDate(0, 0, retention)), systemPrincipal, newID("lwn_"), digestOf(body)); err != nil {
			return err
		}
		if err = insertDeliveries(ctx, tx, pid, msg, recipientIDsOf(recipients)); err != nil {
			return err
		}
	}
	scoped := *s
	scoped.principal = systemPrincipal
	if err = scoped.audit(ctx, tx, wid, pid, str(sys, "id"), "linear_write", w.OperationID, w.State, map[string]any{"operation": w.Operation, "issue": w.Issue, "reason": w.Reason, "recipient_count": len(recipients), "coordinator": coordinator}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) outboxNotifyRow(ctx context.Context, id string) (rows []map[string]any, err error) {
	err = s.ledgerTx(ctx, func(tx *transaction) error {
		rows, err = query(ctx, tx, `SELECT `+outboxColumns+` FROM linear_write_outbox WHERE id=? AND notified_at IS NULL AND settled_at IS NOT NULL`, id)
		return err
	})
	return rows, err
}

// OutboxUnnotified lists settled writes whose notice is still owed.
func (s *Store) OutboxUnnotified(ctx context.Context, limit int) (ids []string, err error) {
	err = s.ledgerTx(ctx, func(tx *transaction) error {
		scope, a := s.outboxScope("")
		rows, err := query(ctx, tx, `SELECT id FROM linear_write_outbox WHERE settled_at IS NOT NULL AND notified_at IS NULL`+scope+` ORDER BY settled_at LIMIT ?`, append(a, limit)...)
		for _, r := range rows {
			ids = append(ids, str(r, "id"))
		}
		return err
	})
	return ids, err
}

// outboxNotice is a settled write's notice: typed fields only.
func outboxNotice(w OutboxWrite) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your queued Linear %s (operation_id %s) on %s: %s.", w.Operation, w.OperationID, w.Issue, w.State)
	switch w.State {
	case OutboxDone:
		if w.Result != "" {
			fmt.Fprintf(&b, " %s.", w.Result)
		}
	case OutboxUncertain:
		b.WriteString(" It may have reached Linear: read the issue back before doing anything else; the same operation_id sends nothing again.")
	case OutboxFailed:
		fmt.Fprintf(&b, " Nothing was sent: %s.", w.Reason)
	case OutboxDead:
		fmt.Fprintf(&b, " Nothing was sent after %d tries (%s).", w.Attempts, w.Reason)
	case OutboxExpired:
		b.WriteString(" Nothing was sent: it waited past its expiry. Decide again whether it is still wanted; if so, use a new operation_id.")
	}
	b.WriteString("\nDetails: clawbridge linear write-status " + w.OperationID)
	b.WriteString("\n" + noticeLine)
	return b.String()
}
