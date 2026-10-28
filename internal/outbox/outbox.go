// Package outbox is the client's queue of the writes it made while the
// server was unreachable or in maintenance (ZERO-LOSS-DEPLOY §1). Only
// writes that a replay can't change queue: a send or reply of kind message
// or reply without coordination, an ack, and mark_read. Each is replayed
// verbatim, with the same operation_id, so the server's dedupe makes it
// exactly-once. The queue is a SQLite file beside the profile's mirror,
// mode 0600.
package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"clawbridge/internal/bus"
	_ "modernc.org/sqlite"
)

// Bounds, per profile (§1.3).
const (
	MaxItems = 1000
	MaxBytes = 8 << 20
	// Expiry is how long a write waits before it becomes a dead letter.
	Expiry = 24 * time.Hour
	// WarnAge is when doctor starts warning about a waiting write.
	WarnAge = 10 * time.Minute
	// deadKeep is how long a dead letter is kept.
	deadKeep = 7 * 24 * time.Hour
	// leaseFor is how long a drainer holds a queue without renewing it.
	leaseFor = 30 * time.Second
	// maxBackoff caps a queue's wait after a transient failure.
	maxBackoff = 60 * time.Second
)

// Dead-letter classes the drainer sets itself; a refusal's class is the
// server's code.
const (
	ClassExpired        = "expired"
	ClassAccountChanged = "account_changed"
	ClassInstance       = "instance_changed"
	ClassAckRefused     = "ack_refused"
	// ClassAckedAfterDead is an ack that landed on a delivery that had died
	// meanwhile: done, not an error, and listed once (§1.8).
	ClassAckedAfterDead = "acked_after_dead"
)

// ErrFull refuses a write past the bounds.
var ErrFull = errors.New("the local outbox is full")

// Disabled is the kill switch: CLAWBRIDGE_CLIENT_OUTBOX=off queues nothing.
func Disabled() bool { return strings.EqualFold(os.Getenv("CLAWBRIDGE_CLIENT_OUTBOX"), "off") }

// PathFor is the outbox beside a profile's mirror cache.
func PathFor(cachePath string) string {
	if cachePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cachePath), "outbox.db")
}

// Queueable reports whether r may wait in the outbox (§1.1). Anything else
// fails fast as before: reads, time-bound writes (heartbeat, extend,
// lease, nack), compare-and-set writes, session, admin and Linear writes,
// and a message whose meaning depends on when it is made (coordination,
// or a decision, handoff or routing-change).
func Queueable(r bus.Request) bool {
	switch r.Operation {
	case "send", "reply":
		if _, ok := r.Args["coordination"]; ok {
			return false
		}
		kind, _ := r.Args["kind"].(string)
		op, _ := r.Args["operation_id"].(string)
		return (kind == "" || kind == "message" || kind == "reply") && op != ""
	case "ack":
		return len(ackEntries(r.Args["acks"])) > 0
	case "mark_read":
		return true
	}
	return false
}

// Key is one queue: order is kept per sender (§1.4).
type Key struct {
	Server, Workspace, Project, Session, Instance string
}

// KeyOf is r's queue on server.
func KeyOf(server string, r bus.Request) Key {
	instance, _ := r.Args["instance"].(string)
	return Key{Server: server, Workspace: r.Workspace, Project: r.Project, Session: r.Session, Instance: instance}
}

func (k Key) id() string {
	raw, _ := json.Marshal([]string{k.Server, k.Workspace, k.Project, k.Session, k.Instance})
	return string(raw)
}

// Scope is who a write was made as: it is only ever sent as them (§1.6).
type Scope struct {
	Principal, Credential string
}

// Store is one profile's outbox.
type Store struct {
	db    *sql.DB
	owner string
	now   func() time.Time
	// jitter spreads a backoff; tests replace it.
	jitter func(time.Duration) time.Duration
}

const schema = `
CREATE TABLE IF NOT EXISTS client_outbox(
 seq INTEGER PRIMARY KEY AUTOINCREMENT,
 qkey TEXT NOT NULL,
 server TEXT NOT NULL, workspace TEXT NOT NULL, project TEXT NOT NULL, session TEXT NOT NULL, instance TEXT NOT NULL,
 principal TEXT NOT NULL, credential TEXT NOT NULL,
 operation TEXT NOT NULL, operation_id TEXT NOT NULL, recipient TEXT NOT NULL, acks TEXT NOT NULL,
 request TEXT NOT NULL, size INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('queued','dead')),
 first INTEGER NOT NULL DEFAULT 0,
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, dead_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS client_outbox_queue ON client_outbox(state, qkey, first, seq);
CREATE TABLE IF NOT EXISTS client_outbox_leases(qkey TEXT PRIMARY KEY, owner TEXT NOT NULL, until INTEGER NOT NULL);
`

// Open opens (creating) the outbox at path: a 0600 file in a 0700
// directory.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("outbox path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err = os.Chmod(abs, 0600); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: bus.SQLiteURIPath(runtime.GOOS, abs)}
	u.RawQuery = url.Values{"_pragma": []string{"busy_timeout(5000)", "journal_mode(WAL)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, owner: newOwner(), now: time.Now}, nil
}

func newOwner() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b))
}

// Close closes the outbox.
func (s *Store) Close() error { return s.db.Close() }

// SetNow replaces the clock, for tests.
func (s *Store) SetNow(now func() time.Time) { s.now = now }

func (s *Store) at() int64 { return s.now().UnixNano() }

// Item is a waiting or dead write, without its body.
type Item struct {
	ID          int64         `json:"id"`
	Key         Key           `json:"-"`
	Workspace   string        `json:"workspace"`
	Project     string        `json:"project"`
	Session     string        `json:"session,omitempty"`
	Operation   string        `json:"operation"`
	OperationID string        `json:"operation_id"`
	To          string        `json:"to,omitempty"`
	Size        int           `json:"size"`
	Age         time.Duration `json:"-"`
	AgeSeconds  int64         `json:"age_seconds"`
	State       string        `json:"state"`
	Attempts    int           `json:"attempts"`
	Error       string        `json:"error,omitempty"`
	request     bus.Request
	scope       Scope
	acks        []string
}

// Queued is Enqueue's answer.
type Queued struct {
	ID       int64
	Position int
}

// Enqueue stores r, made as scope, at the back of its queue. It commits
// before it returns, so a caller told queued_local can't lose it.
func (s *Store) Enqueue(ctx context.Context, server string, scope Scope, r bus.Request) (Queued, error) {
	if !Queueable(r) {
		return Queued{}, errors.New("this operation never waits in the outbox")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return Queued{}, err
	}
	key := KeyOf(server, r)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Queued{}, err
	}
	defer tx.Rollback()
	now := s.at()
	if err = expire(ctx, tx, now); err != nil {
		return Queued{}, err
	}
	var count, size int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(size),0) FROM client_outbox WHERE state='queued'`).Scan(&count, &size); err != nil {
		return Queued{}, err
	}
	if count+1 > MaxItems || size+len(raw) > MaxBytes {
		return Queued{}, ErrFull
	}
	op, _ := r.Args["operation_id"].(string)
	to, _ := r.Args["to"].(string)
	if r.Operation == "reply" {
		to, _ = r.Args["message_id"].(string)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO client_outbox(qkey,server,workspace,project,session,instance,principal,credential,operation,operation_id,recipient,acks,request,size,state,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,'queued',?,?)`,
		key.id(), key.Server, key.Workspace, key.Project, key.Session, key.Instance, scope.Principal, scope.Credential, r.Operation, op, to, ackList(r), string(raw), len(raw), now, now+int64(Expiry))
	if err != nil {
		return Queued{}, err
	}
	id, _ := res.LastInsertId()
	var position int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_outbox WHERE state='queued' AND qkey=?`, key.id()).Scan(&position); err != nil {
		return Queued{}, err
	}
	return Queued{ID: id, Position: position}, tx.Commit()
}

// ackList is the message IDs a write acks, newline-framed for lookup: an
// ack's items, or a reply's piggybacked ack.
func ackList(r bus.Request) string {
	ids := []string{}
	switch r.Operation {
	case "ack":
		for _, m := range ackEntries(r.Args["acks"]) {
			if id, _ := m["message_id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	case "send", "reply":
		if m, ok := r.Args["ack"].(map[string]any); ok {
			if id, _ := m["message_id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return "\n" + strings.Join(ids, "\n") + "\n"
}

// ackEntries reads an ack's acks in either shape the bus takes: decoded
// JSON ([]any, from the MCP tool) or a Go caller's []map[string]any.
func ackEntries(v any) []map[string]any {
	switch list := v.(type) {
	case []map[string]any:
		return list
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, x := range list {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// expire turns writes past their expiry into dead letters and deletes
// dead letters past their keep.
func expire(ctx context.Context, tx *sql.Tx, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE client_outbox SET state='dead',last_error=?,dead_at=? WHERE state='queued' AND expires_at<=?`, ClassExpired, now, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM client_outbox WHERE state='dead' AND dead_at<=?`, now-int64(deadKeep))
	return err
}

// Waiting counts key's queued writes.
func (s *Store) Waiting(ctx context.Context, key Key) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_outbox WHERE state='queued' AND qkey=?`, key.id()).Scan(&n)
	return n, err
}

// PendingAck reports whether key's queue holds an ack of messageID (§1.8).
func (s *Store) PendingAck(ctx context.Context, key Key, messageID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_outbox WHERE state='queued' AND qkey=? AND instr(acks,?)>0`, key.id(), "\n"+messageID+"\n").Scan(&n)
	return n > 0, err
}

// Promote moves key's queued writes that ack messageID to the head of its
// queue, due now: the message is being offered again (§1.8). Acks commute
// with sends, so only an ack may overtake; a reply carrying the ack keeps
// its place.
func (s *Store) Promote(ctx context.Context, key Key, messageID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE client_outbox SET first=1,next_attempt_at=0 WHERE state='queued' AND qkey=? AND operation='ack' AND instr(acks,?)>0`, key.id(), "\n"+messageID+"\n")
	return err
}

// Status is the outbox for doctor, whoami and the TUI.
type Status struct {
	Waiting int           `json:"waiting"`
	Bytes   int           `json:"bytes"`
	Oldest  time.Duration `json:"-"`
	// OldestSeconds is Oldest for JSON.
	OldestSeconds int64 `json:"oldest_age_seconds"`
	// Dead counts the dead letters of the last day that need a look.
	Dead int `json:"dead"`
	// Expired is whether one of them expired.
	Expired bool `json:"expired"`
}

// Full reports whether a bound is reached.
func (st Status) Full() bool { return st.Waiting >= MaxItems || st.Bytes >= MaxBytes }

// Status reads the outbox.
func (s *Store) Status(ctx context.Context) (Status, error) {
	now := s.at()
	var st Status
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(size),0),MIN(created_at) FROM client_outbox WHERE state='queued'`).Scan(&st.Waiting, &st.Bytes, &oldest); err != nil {
		return Status{}, err
	}
	if oldest.Valid {
		st.Oldest = time.Duration(now - oldest.Int64)
		st.OldestSeconds = int64(st.Oldest / time.Second)
	}
	var expired int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(last_error=?),0) FROM client_outbox WHERE state='dead' AND last_error<>? AND dead_at>?`, ClassExpired, ClassAckedAfterDead, now-int64(24*time.Hour)).Scan(&st.Dead, &expired); err != nil {
		return Status{}, err
	}
	st.Expired = expired > 0
	return st, nil
}

// List is the waiting writes, or the dead letters, oldest first. Listing
// the dead letters shows an acked_after_dead once, then forgets it.
func (s *Store) List(ctx context.Context, dead bool) ([]Item, error) {
	state := "queued"
	if dead {
		state = "dead"
	}
	items, err := s.items(ctx, `state=? ORDER BY qkey,first DESC,seq`, state)
	if err != nil || !dead {
		return items, err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM client_outbox WHERE state='dead' AND last_error=?`, ClassAckedAfterDead)
	return items, err
}

func (s *Store) items(ctx context.Context, where string, args ...any) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,server,workspace,project,session,instance,principal,credential,operation,operation_id,recipient,acks,request,size,state,attempts,last_error,created_at FROM client_outbox WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.at()
	out := []Item{}
	for rows.Next() {
		var i Item
		var acks, raw string
		var created int64
		if err = rows.Scan(&i.ID, &i.Key.Server, &i.Key.Workspace, &i.Key.Project, &i.Key.Session, &i.Key.Instance, &i.scope.Principal, &i.scope.Credential, &i.Operation, &i.OperationID, &i.To, &acks, &raw, &i.Size, &i.State, &i.Attempts, &i.Error, &created); err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if err = decoder.Decode(&i.request); err != nil {
			return nil, err
		}
		i.Workspace, i.Project, i.Session = i.Key.Workspace, i.Key.Project, i.Key.Session
		i.Age = time.Duration(now - created)
		i.AgeSeconds = int64(i.Age / time.Second)
		i.acks = strings.Fields(acks)
		out = append(out, i)
	}
	return out, rows.Err()
}

// Retry puts dead letter id back at the head of its queue, with a new
// expiry.
func (s *Store) Retry(ctx context.Context, id int64) error {
	now := s.at()
	res, err := s.db.ExecContext(ctx, `UPDATE client_outbox SET state='queued',first=1,attempts=0,next_attempt_at=0,last_error='',dead_at=0,expires_at=? WHERE seq=? AND state='dead' AND last_error<>?`, now+int64(Expiry), id, ClassAckedAfterDead)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no dead letter %d", id)
	}
	return nil
}

// Drop deletes write id, waiting or dead.
func (s *Store) Drop(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM client_outbox WHERE seq=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no outbox item %d", id)
	}
	return nil
}

// Answer is what one attempt at a write came to, as the client reads it.
type Answer struct {
	Kind AnswerKind
	// Data is an accepted write's result.
	Data any
	// Class is a refusal's code, or a transient failure's kind.
	Class string
	// RetryAfter is the server's Retry-After, when it gave one.
	RetryAfter time.Duration
}

// AnswerKind sorts an attempt's answer (§1.2, §2.4).
type AnswerKind int

const (
	// Accepted: the server committed the write (or had already).
	Accepted AnswerKind = iota
	// Transient: unreachable, or a 502/503/504; the outcome may be
	// uncertain, and the identical replay is safe.
	Transient
	// Maintenance: the server is draining and sent nothing; the attempt
	// doesn't count.
	Maintenance
	// NoCredential: no token could be had; the write waits for a sign-in.
	NoCredential
	// Refused: a definitive answer; the write becomes a dead letter.
	Refused
)

// Sender makes one attempt at r with fresh credentials.
type Sender func(context.Context, bus.Request) Answer

// Report is what a drain did.
type Report struct {
	Sent int
	Dead int
	// Waiting is what is still queued in the drained queues.
	Waiting int
	// SignIn is set when the drain stopped for want of a credential.
	SignIn bool
}

// Drain sends server's due queues, each in order, as scope. A queue stops
// at its first transient failure (head of line); a refusal moves its head
// to the dead letters and the queue goes on. With only set, it drains that
// one queue. Writes made as another account become dead letters.
func (s *Store) Drain(ctx context.Context, server string, scope Scope, only *Key, send Sender) (Report, error) {
	var report Report
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	if err = expire(ctx, tx, s.at()); err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err != nil {
		return report, err
	}
	query, args := `SELECT DISTINCT qkey FROM client_outbox WHERE state='queued' AND server=?`, []any{server}
	if only != nil {
		query, args = query+` AND qkey=?`, append(args, only.id())
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return report, err
	}
	keys := []string{}
	for rows.Next() {
		var k string
		if err = rows.Scan(&k); err != nil {
			rows.Close()
			return report, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		if err = s.drainQueue(ctx, k, scope, send, &report); err != nil {
			return report, err
		}
		if report.SignIn {
			break
		}
	}
	for _, k := range keys {
		var n int
		if s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM client_outbox WHERE state='queued' AND qkey=?`, k).Scan(&n) == nil {
			report.Waiting += n
		}
	}
	return report, nil
}

// claim takes (or renews) the drain lease on queue k.
func (s *Store) claim(ctx context.Context, k string) (bool, error) {
	now := s.at()
	res, err := s.db.ExecContext(ctx, `INSERT INTO client_outbox_leases(qkey,owner,until) VALUES(?,?,?) ON CONFLICT(qkey) DO UPDATE SET owner=excluded.owner,until=excluded.until WHERE client_outbox_leases.owner=excluded.owner OR client_outbox_leases.until<=?`, k, s.owner, now+int64(leaseFor), now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) release(k string) {
	s.db.Exec(`DELETE FROM client_outbox_leases WHERE qkey=? AND owner=?`, k, s.owner)
}

func (s *Store) drainQueue(ctx context.Context, k string, scope Scope, send Sender, report *Report) error {
	ok, err := s.claim(ctx, k)
	if err != nil || !ok {
		return err
	}
	defer s.release(k)
	for ctx.Err() == nil {
		if ok, err = s.claim(ctx, k); err != nil || !ok {
			return err
		}
		heads, err := s.items(ctx, `state='queued' AND qkey=? ORDER BY first DESC,seq LIMIT 1`, k)
		if err != nil || len(heads) == 0 {
			return err
		}
		head := heads[0]
		var due int64
		if err = s.db.QueryRowContext(ctx, `SELECT next_attempt_at FROM client_outbox WHERE seq=?`, head.ID).Scan(&due); err != nil {
			return err
		}
		if due > s.at() {
			return nil
		}
		if head.scope != scope {
			// Never sent as another account (§1.6).
			if err = s.dead(ctx, head.ID, ClassAccountChanged); err != nil {
				return err
			}
			report.Dead++
			continue
		}
		answer := send(ctx, head.request)
		switch answer.Kind {
		case Accepted:
			if err = s.accepted(ctx, head, answer.Data); err != nil {
				return err
			}
			report.Sent++
		case Maintenance:
			// Draining: nothing was sent, and the attempt doesn't count.
			return s.later(ctx, head.ID, max(answer.RetryAfter, time.Second), false)
		case Transient:
			wait := answer.RetryAfter
			if wait <= 0 {
				wait = s.backoff(head.Attempts + 1)
			}
			return s.later(ctx, head.ID, wait, true)
		case NoCredential:
			report.SignIn = true
			return nil
		default:
			dead, stop, err := s.refused(ctx, head, answer, send)
			if err != nil || stop {
				return err
			}
			report.Dead += dead
			if dead == 0 {
				report.Sent++
			}
		}
	}
	return nil
}

// accepted deletes a sent write; an ack that landed on a delivery that had
// died meanwhile is kept once, to be listed (§1.8).
func (s *Store) accepted(ctx context.Context, head Item, data any) error {
	if ackedDead(head.Operation, data) {
		return s.dead(ctx, head.ID, ClassAckedAfterDead)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM client_outbox WHERE seq=?`, head.ID)
	return err
}

func ackedDead(operation string, data any) bool {
	m, _ := data.(map[string]any)
	switch operation {
	case "ack":
		acks, _ := m["acks"].([]any)
		for _, a := range acks {
			if r, ok := a.(map[string]any); ok && r["outcome"] == "dead" {
				return true
			}
		}
	case "send", "reply":
		if a, ok := m["ack"].(map[string]any); ok && a["outcome"] == "dead" {
			return true
		}
	}
	return false
}

// refused handles a definitive answer. A reply refused with its
// piggybacked ack is sent once more bare, under the same operation_id
// (the ack isn't in its digest): if that lands, only the ack becomes a
// dead letter (D2'). Otherwise the write is the dead letter. dead counts
// the dead letters made; stop ends the queue's drain for now.
func (s *Store) refused(ctx context.Context, head Item, answer Answer, send Sender) (dead int, stop bool, err error) {
	if _, carries := head.request.Args["ack"]; carries && (head.Operation == "reply" || head.Operation == "send") {
		bare := head.request
		bare.Args = map[string]any{}
		for k, v := range head.request.Args {
			if k != "ack" {
				bare.Args[k] = v
			}
		}
		second := send(ctx, bare)
		switch second.Kind {
		case Accepted:
			return 1, false, s.splitAck(ctx, head, bare, answer.Class, true)
		case Refused:
			// The reply itself is refused.
			return 1, false, s.dead(ctx, head.ID, second.Class)
		case NoCredential:
			return 0, true, nil
		default:
			// Unknown yet: keep the reply, bare, at the head; its ack is
			// the dead letter.
			if err = s.splitAck(ctx, head, bare, answer.Class, false); err != nil {
				return 0, true, err
			}
			wait := second.RetryAfter
			if wait <= 0 {
				wait = s.backoff(head.Attempts + 1)
			}
			return 1, true, s.later(ctx, head.ID, wait, second.Kind == Transient)
		}
	}
	return 1, false, s.dead(ctx, head.ID, answer.Class)
}

// splitAck records head's piggybacked ack as its own dead letter, and
// either deletes the reply (sent) or keeps it bare in its place.
func (s *Store) splitAck(ctx context.Context, head Item, bare bus.Request, class string, sent bool) error {
	ack := bus.Request{Operation: "ack", Workspace: head.request.Workspace, Project: head.request.Project, Session: head.request.Session, Args: map[string]any{"acks": []any{head.request.Args["ack"]}, "operation_id": head.OperationID + ":ack"}}
	if instance, ok := head.request.Args["instance"]; ok {
		ack.Args["instance"] = instance
	}
	rawAck, err := json.Marshal(ack)
	if err != nil {
		return err
	}
	rawBare, err := json.Marshal(bare)
	if err != nil {
		return err
	}
	now := s.at()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO client_outbox(qkey,server,workspace,project,session,instance,principal,credential,operation,operation_id,recipient,acks,request,size,state,last_error,created_at,expires_at,dead_at) SELECT qkey,server,workspace,project,session,instance,principal,credential,'ack',operation_id||':ack','',acks,?,?,'dead',?,created_at,expires_at,? FROM client_outbox WHERE seq=?`,
		string(rawAck), len(rawAck), ClassAckRefused+":"+class, now, head.ID); err != nil {
		return err
	}
	if sent {
		_, err = tx.ExecContext(ctx, `DELETE FROM client_outbox WHERE seq=?`, head.ID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE client_outbox SET request=?,size=?,acks='' WHERE seq=?`, string(rawBare), len(rawBare), head.ID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) dead(ctx context.Context, id int64, class string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE client_outbox SET state='dead',first=0,last_error=?,dead_at=? WHERE seq=?`, class, s.at(), id)
	return err
}

// later puts id off by wait, counting the attempt when counted.
func (s *Store) later(ctx context.Context, id int64, wait time.Duration, counted bool) error {
	add := 0
	if counted {
		add = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE client_outbox SET attempts=attempts+?,next_attempt_at=? WHERE seq=?`, add, s.at()+int64(wait), id)
	return err
}

// backoff is 1 s doubling to 60 s, with up to a quarter of jitter.
func (s *Store) backoff(attempt int) time.Duration {
	wait := time.Second
	for i := 1; i < attempt && wait < maxBackoff; i++ {
		wait *= 2
	}
	wait = min(wait, maxBackoff)
	if s.jitter != nil {
		return s.jitter(wait)
	}
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(wait/4)+1)); err == nil {
		wait += time.Duration(n.Int64())
	}
	return wait
}
