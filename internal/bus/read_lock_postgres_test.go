package bus

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPureReadClassification(t *testing.T) {
	session := map[string]any{"instance": "me-inst"}
	instanceOnly := map[string]any{"instance": "me-inst"}
	for _, tc := range []struct {
		op, session string
		args        map[string]any
		pure        bool
	}{
		{"snapshot", "me", session, true},
		{"inbox_status", "me", session, true},
		{"notification_status", "me", session, true},
		{"taskboard_get", "me", session, true},
		{"taskboard_get_v2", "me", session, true},
		{"taskboard_access", "me", session, true},
		{"taskboard_access", "", nil, true},
		{"linear_comments_posted", "me", session, true},
		{"list_projects", "", nil, true},
		{"list_members", "", nil, true},
		{"list_sessions", "", nil, true},
		{"history", "", nil, true},
		{"whoami", "", nil, true},
		{"resolve_project", "", nil, true},
		// Naming a session with its instance is a heartbeat read: the read is
		// pure and the heartbeat follows in its own transaction.
		{"list_sessions", "me", session, true},
		{"history", "me", session, true},
		{"whoami", "me", session, true},
		// resolve_project still heartbeats inside its transaction.
		{"resolve_project", "me", session, false},
		// A session without an instance still names a session to heartbeat
		// (or to refuse), and an instance alone is refused in the transaction.
		{"list_sessions", "me", nil, false},
		{"whoami", "me", nil, false},
		{"resolve_project", "me", nil, false},
		{"history", "", instanceOnly, false},
		{"whoami", "", instanceOnly, false},
		{"inbox", "me", session, false},
		{"mark_read", "me", session, false},
		{"message_status", "me", session, false},
		{"send", "me", session, false},
		{"register_session", "me", session, false},
		{"taskboard_set_v2", "me", session, false},
	} {
		if got := pureRead(Request{Operation: tc.op, Session: tc.session, Args: tc.args}); got != tc.pure {
			t.Errorf("pureRead(%s, session %q, args %v) = %v, want %v", tc.op, tc.session, tc.args, got, tc.pure)
		}
	}
}

type readLockFixture struct {
	t     *testing.T
	s     *Store
	other *sql.DB
	owner string
	w     string // workspace name
	wid   string
	pid   string
}

func newReadLockFixture(t *testing.T) *readLockFixture {
	t.Helper()
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	f := &readLockFixture{t: t, owner: "owner-" + newID("") + "@read-lock-test.example", w: newID("unit_")}
	var err error
	if f.s, err = OpenPostgres(ctx, dsn, f.owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.s.Close() })
	if f.other, err = sql.Open("pgx", dsn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.other.Close() })
	if _, e := f.exec(ctx, "workspace_init", "", map[string]any{"mode": "server"}); e != nil {
		t.Fatal(e)
	}
	project, e := f.exec(ctx, "project_create", "", map[string]any{"slug": "p"})
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"me", "peer"} {
		if _, e = f.exec(ctx, "register_session", name, nil); e != nil {
			t.Fatal(e)
		}
	}
	f.pid = project.Data.(map[string]any)["id"].(string)
	if e = f.s.db.QueryRowContext(ctx, `SELECT workspace_id FROM projects WHERE id=$1`, f.pid).Scan(&f.wid); e != nil {
		t.Fatal(e)
	}
	return f
}

func (f *readLockFixture) exec(ctx context.Context, op, session string, a map[string]any) (Result, error) {
	if a == nil {
		a = map[string]any{}
	}
	if session != "" {
		a["instance"] = session + "-inst"
	}
	return f.s.ExecuteAs(ctx, f.owner, Request{Operation: op, Workspace: f.w, Project: "p", Session: session, Args: a})
}

// hold takes the advisory lock key on a separate connection. The returned
// release is idempotent and also runs at cleanup.
func (f *readLockFixture) hold(key string) func() {
	f.t.Helper()
	holder, e := f.other.BeginTx(context.Background(), nil)
	if e != nil {
		f.t.Fatal(e)
	}
	var once sync.Once
	release := func() { once.Do(func() { holder.Rollback() }) }
	f.t.Cleanup(release)
	if _, e = holder.Exec(`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); e != nil {
		release()
		f.t.Fatal(e)
	}
	return release
}

// waiters counts backends in this database waiting for exactly this advisory
// key. pg_advisory_xact_lock(bigint) splits the key into classid (high 32
// bits) and objid (low 32 bits) with objsubid 1.
func (f *readLockFixture) waiters(key string) int {
	f.t.Helper()
	var n int
	if e := f.other.QueryRow(`SELECT COUNT(*) FROM pg_locks l JOIN pg_database d ON d.oid=l.database WHERE d.datname=current_database() AND l.locktype='advisory' AND NOT l.granted AND l.objsubid=1 AND ((l.classid::bigint<<32)|l.objid::bigint)=hashtextextended($1,0)`, key).Scan(&n); e != nil {
		f.t.Fatal(e)
	}
	return n
}

// start runs a request in the background. At cleanup it releases the given
// lock first, so the request can finish, and then waits for it.
func (f *readLockFixture) start(release func(), op, session string, a map[string]any) <-chan error {
	done, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		_, e := f.exec(context.Background(), op, session, a)
		done <- e
	}()
	f.t.Cleanup(func() {
		release()
		select {
		case <-finished:
		case <-time.After(30 * time.Second):
		}
	})
	return done
}

// readsWithoutWaiting asserts each read finishes while key is held and is
// never seen queued on it. The 30s bound only stops a hung test.
func (f *readLockFixture) readsWithoutWaiting(key string, release func(), reads []readLockCase) {
	f.t.Helper()
	for _, read := range reads {
		done := f.start(release, read.op, read.session, read.args)
		deadline := time.Now().Add(30 * time.Second)
	poll:
		for {
			select {
			case e := <-done:
				if e != nil {
					f.t.Errorf("%s (session %q) failed while %s was held: %v", read.op, read.session, key, e)
				}
				break poll
			default:
			}
			if f.waiters(key) > 0 {
				f.t.Fatalf("%s (session %q) queued on %s", read.op, read.session, key)
			}
			if time.Now().After(deadline) {
				f.t.Fatalf("%s (session %q) did not finish", read.op, read.session)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// writeWaits asserts the request queues on key, not merely that it is slow,
// and succeeds once the lock is released.
func (f *readLockFixture) writeWaits(key string, write readLockCase) {
	f.t.Helper()
	release := f.hold(key)
	done := f.start(release, write.op, write.session, write.args)
	for deadline := time.Now().Add(30 * time.Second); f.waiters(key) == 0; time.Sleep(10 * time.Millisecond) {
		select {
		case e := <-done:
			f.t.Fatalf("%s (session %q) ran while %s was held: %v", write.op, write.session, key, e)
		default:
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("%s (session %q) never queued on %s", write.op, write.session, key)
		}
	}
	release()
	select {
	case e := <-done:
		if e != nil {
			f.t.Fatalf("%s failed after %s was released: %v", write.op, key, e)
		}
	case <-time.After(30 * time.Second):
		f.t.Fatalf("%s did not finish after %s was released", write.op, key)
	}
}

type readLockCase struct {
	op, session string
	args        map[string]any
}

var pureReadCases = []readLockCase{
	{"list_sessions", "", nil}, {"history", "", nil}, {"whoami", "", nil},
	{"list_projects", "", nil}, {"list_members", "", nil},
	{"snapshot", "me", nil}, {"inbox_status", "me", nil}, {"notification_status", "me", nil},
	{"taskboard_get", "me", nil}, {"taskboard_get_v2", "me", nil}, {"taskboard_access", "me", map[string]any{"ledger": 1}},
	{"linear_comments_posted", "me", map[string]any{"issue_uuid": "00000000-0000-4000-8000-000000000001", "comment_ids": []any{}}},
}

// Reads must not queue behind a held workspace lock (a 5s lock_timeout there
// turns into a 503 and a stale mirror), while every writing request, including
// a read that heartbeats its session, still waits for it.
func TestPostgresPureReadsSkipTheWorkspaceLock(t *testing.T) {
	f := newReadLockFixture(t)
	key := "workspace:" + f.wid
	release := f.hold(key)
	f.readsWithoutWaiting(key, release, pureReadCases)
	release()
	for _, write := range []readLockCase{
		{"inbox", "me", nil},
		{"send", "me", map[string]any{"to": "peer", "body": "after the lock", "operation_id": "serialised"}},
	} {
		f.writeWaits(key, write)
	}
}

// Pure reads also skip the principal lock that serializes one principal's
// rate-limit accounting; writes keep it.
func TestPostgresPureReadsSkipThePrincipalLock(t *testing.T) {
	f := newReadLockFixture(t)
	key := "principal:" + f.owner
	release := f.hold(key)
	// resolve_project never takes the workspace lock, only this one.
	f.readsWithoutWaiting(key, release, append([]readLockCase{{"resolve_project", "", nil}}, pureReadCases...))
	release()
	if resolved, e := f.exec(context.Background(), "resolve_project", "", nil); e != nil || resolved.Data.(map[string]any)["project_id"] != f.pid || resolved.Data.(map[string]any)["workspace_id"] != f.wid {
		t.Fatalf("resolve_project did not resolve this project: %v %v", resolved.Data, e)
	}
	f.writeWaits(key, readLockCase{"send", "me", map[string]any{"to": "peer", "body": "after the lock", "operation_id": "principal"}})
	f.writeWaits(key, readLockCase{"resolve_project", "me", nil})
}

func codeOf(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return ""
}

// SQLSTATE 25006 is a misclassified read only inside a pure read's read-only
// transaction; anywhere else the database itself is read-only.
func TestReadOnlyTransactionWriteIsInvalidOnlyForAPureRead(t *testing.T) {
	logged := []string{}
	old := logMisclassifiedRead
	logMisclassifiedRead = func(op string) { logged = append(logged, op) }
	t.Cleanup(func() { logMisclassifiedRead = old })
	r := Request{Operation: "history"}
	err := classify(misclassifiedRead(&transaction{pure: true}, r, stateError("25006")))
	if codeOf(err) != "invalid" || len(logged) != 1 || logged[0] != "history" {
		t.Fatalf("pure read write: %v, logged %v", err, logged)
	}
	if _, public := PublicMessage(err); public {
		t.Fatal("database text marked public")
	}
	for name, tc := range map[string]struct {
		tx    *transaction
		cause error
	}{
		"read-only database":  {&transaction{}, stateError("25006")},
		"other state, pure":   {&transaction{pure: true}, stateError("40001")},
		"timeout, pure":       {&transaction{pure: true}, stateError("57014")},
		"plain failure, pure": {&transaction{pure: true}, errors.New("authority unavailable")},
	} {
		if got := codeOf(classify(misclassifiedRead(tc.tx, r, tc.cause))); got != "unavailable" {
			t.Errorf("%s: %s, want unavailable", name, got)
		}
	}
	if len(logged) != 1 {
		t.Fatal("logged outside a pure read", logged)
	}
}

// Against Postgres: a read-only database makes writes unavailable (a mirror
// fallback is right), while pure reads still work; the real driver error
// carries 25006, which a pure transaction reports as invalid.
func TestPostgresReadOnlyTransactionWrites(t *testing.T) {
	f := newReadLockFixture(t)
	old := logMisclassifiedRead
	logMisclassifiedRead = func(string) {}
	t.Cleanup(func() { logMisclassifiedRead = old })
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	roDB, err := sql.Open("pgx", dsn+sep+"default_transaction_read_only=on")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roDB.Close() })
	ro := &Store{db: roDB, postgres: true, bootstrapOwner: f.owner}
	request := func(op, session string, a map[string]any) Request {
		if a == nil {
			a = map[string]any{}
		}
		if session != "" {
			a["instance"] = session + "-inst"
		}
		return Request{Operation: op, Workspace: f.w, Project: "p", Session: session, Args: a}
	}
	ctx := context.Background()
	if _, e := ro.ExecuteAs(ctx, f.owner, request("send", "me", map[string]any{"to": "peer", "body": "read-only", "operation_id": "ro"})); codeOf(e) != "unavailable" {
		t.Fatalf("write on a read-only database: %v (%s), want unavailable", e, codeOf(e))
	}
	if _, e := ro.ExecuteAs(ctx, f.owner, request("list_sessions", "", nil)); e != nil {
		t.Fatalf("pure read on a read-only database: %v", e)
	}
	tx, err := f.s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, write := tx.ExecContext(ctx, `UPDATE sessions SET last_seen=last_seen WHERE id='none'`)
	if got := codeOf(classify(misclassifiedRead(&transaction{pure: true}, Request{Operation: "whoami"}, write))); got != "invalid" {
		t.Fatalf("driver 25006 in a pure read: %v (%s), want invalid", write, got)
	}
	if got := codeOf(classify(misclassifiedRead(&transaction{}, Request{Operation: "send"}, write))); got != "unavailable" {
		t.Fatalf("driver 25006 outside a pure read: %s, want unavailable", got)
	}
}

func (f *readLockFixture) lastSeen(name string) string {
	f.t.Helper()
	var seen string
	if e := f.other.QueryRow(`SELECT last_seen FROM sessions WHERE project_id=$1 AND name=$2`, f.pid, name).Scan(&seen); e != nil {
		f.t.Fatal(e)
	}
	return seen
}

var heartbeatReadCases = []readLockCase{{"whoami", "me", nil}, {"list_sessions", "me", nil}, {"history", "me", nil}}

// Reads that name their session no longer queue behind a held workspace
// lock: the heartbeat after the read only tries the lock and is skipped while
// it is held, so last_seen stays put and whoami shows the stored value.
func TestPostgresHeartbeatReadsSkipABusyWorkspaceLock(t *testing.T) {
	f := newReadLockFixture(t)
	failures := []string{}
	old := logHeartbeatSkipped
	logHeartbeatSkipped = func(op string, err error) { failures = append(failures, op) }
	t.Cleanup(func() { logHeartbeatSkipped = old })
	key := "workspace:" + f.wid
	before := f.lastSeen("me")
	release := f.hold(key)
	f.readsWithoutWaiting(key, release, heartbeatReadCases)
	if after := f.lastSeen("me"); after != before {
		t.Fatalf("heartbeat written under a held workspace lock: %s -> %s", before, after)
	}
	who, e := f.exec(context.Background(), "whoami", "me", nil)
	if e != nil {
		t.Fatal(e)
	}
	if shown := who.Data.(map[string]any)["session"].(map[string]any)["last_seen"]; shown != before {
		t.Fatalf("whoami showed %v while its heartbeat was skipped, want the stored %s", shown, before)
	}
	if len(failures) != 0 {
		t.Fatal("a busy lock was logged as a failure", failures)
	}
	release()
	for _, read := range heartbeatReadCases {
		prior := f.lastSeen("me")
		time.Sleep(2 * time.Millisecond)
		result, e := f.exec(context.Background(), read.op, read.session, nil)
		if e != nil {
			t.Fatal(read.op, e)
		}
		now := f.lastSeen("me")
		if now <= prior {
			t.Fatalf("%s did not advance last_seen: %s -> %s", read.op, prior, now)
		}
		if read.op == "whoami" {
			session := result.Data.(map[string]any)["session"].(map[string]any)
			if session["last_seen"] != now || session["presence"] != "active" {
				t.Fatalf("whoami did not show its heartbeat: %v, stored %s", session, now)
			}
		}
	}
	if f.lastSeen("peer") == f.lastSeen("me") {
		t.Fatal("heartbeat touched another session")
	}
}

// A heartbeat that fails (here a row lock held past lock_timeout) is logged
// and skipped; the read still succeeds.
func TestPostgresHeartbeatFailureNeverFailsTheRead(t *testing.T) {
	f := newReadLockFixture(t)
	failures := []string{}
	old := logHeartbeatSkipped
	logHeartbeatSkipped = func(op string, err error) { failures = append(failures, op+": "+codeOf(classify(err))) }
	t.Cleanup(func() { logHeartbeatSkipped = old })
	before := f.lastSeen("me")
	locker, e := f.other.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { locker.Rollback() })
	if _, e = locker.Exec(`SELECT 1 FROM sessions WHERE project_id=$1 AND name='me' FOR UPDATE`, f.pid); e != nil {
		t.Fatal(e)
	}
	if _, e = f.exec(context.Background(), "list_sessions", "me", nil); e != nil {
		t.Fatal("heartbeat failure failed the read", e)
	}
	locker.Rollback()
	if len(failures) != 1 || failures[0] != "list_sessions: unavailable" || f.lastSeen("me") != before {
		t.Fatal("failed heartbeat not logged and skipped", failures)
	}
}
