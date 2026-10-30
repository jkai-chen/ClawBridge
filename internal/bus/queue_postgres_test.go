package bus

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

type pgQueue struct {
	t      *testing.T
	s      *Store
	owner  string
	w      string
	pid    string
	ids    map[string]string
	member string
}

// newPGQueue is a hosted workspace with sessions a, b and c of its owner and
// m of a project member, on the store opened from dsn.
func newPGQueue(t *testing.T, dsn string) *pgQueue {
	t.Helper()
	q := &pgQueue{t: t, owner: newID("owner") + "@queue-test.example", member: newID("member") + "@queue-test.example", w: newID("mq_"), ids: map[string]string{}}
	s, err := OpenPostgres(context.Background(), dsn, q.owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	q.s = s
	q.call(q.owner, "workspace_init", "", map[string]any{"mode": "server"})
	q.call(q.owner, "project_create", "", map[string]any{"slug": "p"})
	q.call(q.owner, "project_role_set", "", map[string]any{"principal": q.member, "role": "member"})
	for _, name := range []string{"a", "b", "c", "m"} {
		who := q.owner
		if name == "m" {
			who = q.member
		}
		r := q.call(who, "register_session", name, map[string]any{"role": "worker"}).(map[string]any)
		q.ids[name] = r["id"].(string)
		q.pid = r["project_id"].(string)
	}
	return q
}

func (q *pgQueue) exec(principal, op, session string, a map[string]any) (Result, error) {
	if a == nil {
		a = map[string]any{}
	}
	if session != "" {
		a["instance"] = session + "-inst"
	}
	return q.s.ExecuteAs(context.Background(), principal, Request{Operation: op, Workspace: q.w, Project: "p", Session: session, Args: a})
}

func (q *pgQueue) call(principal, op, session string, a map[string]any) any {
	q.t.Helper()
	r, err := q.exec(principal, op, session, a)
	if err != nil {
		q.t.Fatalf("%s: %v", op, err)
	}
	return r.Data
}

func (q *pgQueue) as(session string) string {
	if session == "m" {
		return q.member
	}
	return q.owner
}

func (q *pgQueue) do(op, session string, a map[string]any) any {
	q.t.Helper()
	return q.call(q.as(session), op, session, a)
}

func (q *pgQueue) lease(session string) []map[string]any {
	q.t.Helper()
	return q.do("lease", session, nil).(map[string]any)["messages"].([]map[string]any)
}

// M1 on Postgres: twenty transactions deliver to the same recipients in
// crossing orders and hold their counter rows a while before committing;
// none deadlocks, and every recipient's sequence is gap-free.
func TestPostgresCrossingDeliveries(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	q := newPGQueue(t, dsn)
	crossingDeliveries(t, q.s, q.pid, q.ids["a"], []string{q.ids["a"], q.ids["b"], q.ids["c"], q.ids["m"]}, `SELECT pg_sleep(0.005)`)
	assertGapFree(t, q.s.db, q.pid, false)
	var n int
	if err := q.s.db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE project_id=$1`, q.pid).Scan(&n); err != nil || n != 20*5*4 {
		t.Fatal(n, err)
	}
}

func TestPostgresQueue(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	q := newPGQueue(t, dsn)
	move := skew(t)
	q.s.queue.Attempts = 1
	q.do("coordinator_set", "c", map[string]any{"target_session": "c"})
	id := q.do("send", "a", map[string]any{"to": "b", "body": "hello", "operation_id": "one"}).(map[string]any)["id"]
	got := q.lease("b")
	if len(got) != 1 || got[0]["id"] != id || got[0]["recipient_seq"] != int64(1) || got[0]["attempt"] != int64(1) {
		t.Fatal(got)
	}
	// M2: another session's message is not_found, whoever owns it.
	for _, other := range []string{"c", "m"} {
		if o := outcomes(q.do("ack", other, ackOne(id, got[0]["lease_id"])), "acks"); !slices.Equal(o, []string{"not_found"}) {
			t.Fatal(other, o)
		}
	}
	// A second replica's sweep skips while one holds the sweeper's lock.
	holder, err := q.s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = holder.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('cb:sweeper',0))`); err != nil {
		t.Fatal(err)
	}
	move(11 * time.Minute)
	if res, err := q.s.SweepDeliveries(context.Background()); err != nil || !res.Skipped {
		t.Fatal(res, err)
	}
	holder.Rollback()
	// The lease dies through the session's own sweep, which lease runs
	// first. Not SweepDeliveries: it is global, and with this test's clock
	// 11 minutes ahead and one attempt it would kill the live leases of
	// other packages' tests sharing the database (the server's stream tests
	// run alongside under go test -p 2).
	if got := q.lease("b"); len(got) != 0 {
		t.Fatal("a dead letter was offered", got)
	}
	notice := q.do("inbox", "c", nil).(map[string]any)["messages"].([]map[string]any)
	if len(notice) != 1 || notice[0]["kind"] != deadLetterKind || strings.Contains(notice[0]["body"].(string), "hello") {
		t.Fatal(notice)
	}
	// A member who is neither the session nor the coordinator can't read or
	// settle another session's dead letters; the owner and the coordinator can.
	for _, op := range []string{"deadletters_list", "deadletters_requeue", "deadletters_dismiss"} {
		a := map[string]any{"target_session": "b", "message_id": id}
		if op == "deadletters_list" {
			delete(a, "message_id")
		}
		if _, err := q.exec(q.member, op, "m", a); err == nil || !strings.Contains(err.Error(), "only the session") {
			t.Fatal(op, err)
		}
	}
	if _, err := q.exec(q.member, "deadletters_list", "m", map[string]any{"target_session": "@all"}); err == nil {
		t.Fatal("a member listed the project's dead letters")
	}
	if list := q.do("deadletters_list", "a", map[string]any{"target_session": "@all"}).(map[string]any)["dead_letters"].([]map[string]any); len(list) != 1 || list[0]["body"] != nil {
		t.Fatal(list)
	}
	q.do("deadletters_requeue", "c", map[string]any{"target_session": "b", "message_id": id})
	var by string
	if err := q.s.db.QueryRow(`SELECT changed_by_session FROM history WHERE project_id=$1 AND entity='delivery' AND action='requeued'`, q.pid).Scan(&by); err != nil || by != q.ids["c"] {
		t.Fatal(by, err)
	}
	again := q.lease("b")
	if len(again) != 1 || again[0]["attempt"] != int64(1) {
		t.Fatal(again)
	}
	// M7 with the reply's ack in the same transaction.
	reply := map[string]any{"message_id": id, "body": "answer", "operation_id": "msg:one:1", "ack": map[string]any{"message_id": id, "lease_id": again[0]["lease_id"]}}
	first := q.do("reply", "b", reply).(map[string]any)
	replay := q.do("reply", "b", reply).(map[string]any)
	if first["ack"].(map[string]any)["outcome"] != "acked" || replay["id"] != first["id"] || replay["ack"].(map[string]any)["outcome"] != "already_acked" {
		t.Fatal(first, replay)
	}
	st := q.do("notification_status", "b", map[string]any{"queue": true}).(map[string]any)
	if st["unacked_count"] != int64(0) || st["dead_count"] != int64(0) {
		t.Fatal(st)
	}
	if plain := q.do("notification_status", "b", nil).(map[string]any); len(plain) != 7 {
		t.Fatal(plain)
	}
}

// N3 on Postgres: a schema-34 database with read and unread rows, in its
// own schema, upgrades with its deliveries numbered and its reads acked.
func TestPostgresQueueBackfillFromSchema34(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := strings.ReplaceAll(newID("mq"), "-", "_")
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	v := parsed.Query()
	v.Set("search_path", schema)
	parsed.RawQuery = v.Encode()
	q := newPGQueue(t, parsed.String())
	var ids []any
	for _, op := range []string{"o1", "o2", "o3"} {
		ids = append(ids, q.do("send", "a", map[string]any{"to": "b", "body": "old", "operation_id": op}).(map[string]any)["id"])
	}
	q.do("send", "a", map[string]any{"to": "@all", "body": "everyone", "operation_id": "all"})
	q.do("mark_read", "b", map[string]any{"ids": []any{ids[1]}})
	q.s.Close()
	if _, err = admin.ExecContext(ctx, markerOperationsDowngradePostgres(schema)+budgetKindsDowngradePostgres(schema)+webhookDowngradePostgres(schema)+budgetDowngradePostgres(schema)+maintenanceDowngradePostgres(schema)+ledgerDowngradePostgres(schema)+lanesDowngradePostgres(schema)+noticeDowngradePostgres(schema)+leadDowngradePostgres(schema)+rolesDowngradePostgres(schema)+kindDowngradePostgres(schema)+viewsDowngradePostgres(schema)+queueDowngradePostgres(schema)+`DELETE FROM `+schema+`.clawbridge_schema WHERE version>=35`); err != nil {
		t.Fatal(err)
	}
	if q.s, err = OpenPostgres(ctx, parsed.String(), q.owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.s.Close() })
	assertGapFree(t, q.s.db, q.pid, false)
	var acked, read sql.NullString
	if err = q.s.db.QueryRow(`SELECT acked_at,read_at FROM deliveries WHERE message_id=$1 AND recipient_id=$2`, ids[1], q.ids["b"]).Scan(&acked, &read); err != nil || !read.Valid || acked != read {
		t.Fatal(acked, read, err)
	}
	var nullable string
	if err = q.s.db.QueryRow(`SELECT is_nullable FROM information_schema.columns WHERE table_schema=$1 AND table_name='deliveries' AND column_name='recipient_seq'`, schema).Scan(&nullable); err != nil || nullable != "NO" {
		t.Fatal(nullable, err)
	}
	got := q.lease("b")
	if len(got) != 3 || got[0]["id"] != ids[0] || got[1]["id"] != ids[2] || got[1]["recipient_seq"] != int64(3) {
		t.Fatal(got)
	}
	next := q.do("send", "a", map[string]any{"to": "b", "body": "new", "operation_id": "new"}).(map[string]any)["id"]
	var seq int64
	if err = q.s.db.QueryRow(`SELECT recipient_seq FROM deliveries WHERE message_id=$1 AND recipient_id=$2`, next, q.ids["b"]).Scan(&seq); err != nil || seq != 5 {
		t.Fatal(seq, err)
	}
}
