package bus

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// N2: insertDeliveries is the only code that writes deliveries rows, in the
// product, its tests and its migrations alike; the schema-35 backfill only
// updates them.
func TestOnlyInsertDeliveriesWritesDeliveries(t *testing.T) {
	writer := regexp.MustCompile(`(?i)\b(?:into|copy)\s+"?deliveries"?\b`)
	found := 0
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git") {
			return filepath.SkipDir
		}
		if d.IsDir() || !(strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".sql")) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A fixture of an old schema's database (testdata/NAME-vN….sql) is
		// the upgrade's input: the backfill numbers its rows.
		if historicalFixture.MatchString(filepath.ToSlash(path)) {
			return nil
		}
		for _, at := range writer.FindAllIndex(src, -1) {
			found++
			line := 1 + strings.Count(string(src[:at[0]]), "\n")
			if filepath.ToSlash(path) != "../../internal/bus/queue.go" || !strings.Contains(enclosingFunc(src, at[0]), "func insertDeliveries(") {
				t.Errorf("%s:%d writes deliveries; go through insertDeliveries", path, line)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("found %d writes of deliveries, want insertDeliveries' one: the scan is out of date", found)
	}
}

var historicalFixture = regexp.MustCompile(`/testdata/[a-z0-9-]+-v[0-9]+[a-z0-9.-]*\.sql$`)

// enclosingFunc is the source from the last top-level func before at.
func enclosingFunc(src []byte, at int) string {
	start := strings.LastIndex(string(src[:at]), "\nfunc ")
	return string(src[start+1 : at])
}

// skew moves the store's clock by d from now on, for the rest of the test.
func skew(t *testing.T) func(d time.Duration) {
	base := time.Now()
	var offset time.Duration
	var mu sync.Mutex
	hostClock = func() time.Time { mu.Lock(); defer mu.Unlock(); return base.Add(offset) }
	t.Cleanup(func() { hostClock = time.Now })
	return func(d time.Duration) { mu.Lock(); offset += d; mu.Unlock() }
}

func (h *harness) projectID() string {
	h.t.Helper()
	var pid string
	if err := h.s.db.QueryRow(`SELECT project_id FROM sessions WHERE id=?`, h.sessionID("a")).Scan(&pid); err != nil {
		h.t.Fatal(err)
	}
	return pid
}

func (h *harness) lease(session string, a map[string]any) []map[string]any {
	h.t.Helper()
	return h.call("lease", session, a).(map[string]any)["messages"].([]map[string]any)
}

func (h *harness) queueStatus(session string) (unacked, dead int64) {
	h.t.Helper()
	st := h.call("notification_status", session, map[string]any{"queue": true}).(map[string]any)
	return st["unacked_count"].(int64), st["dead_count"].(int64)
}

func ackOne(id, lease any) map[string]any {
	return map[string]any{"acks": []any{map[string]any{"message_id": id, "lease_id": lease}}}
}

func outcomes(v any, key string) []string {
	var out []string
	for _, x := range v.(map[string]any)[key].([]map[string]any) {
		out = append(out, x["outcome"].(string))
	}
	return out
}

func TestQueueLeasesInOrderAndAckIsHandled(t *testing.T) {
	h := newHarness(t)
	h.s.queue.Prefetch = 2
	var ids []any
	for i := range 3 {
		ids = append(ids, h.send(fmt.Sprintf("q%d", i))["id"])
	}
	got := h.lease("b", nil)
	if len(got) != 2 || got[0]["id"] != ids[0] || got[1]["id"] != ids[1] || got[0]["recipient_seq"] != int64(1) || got[1]["recipient_seq"] != int64(2) || got[0]["attempt"] != int64(1) || got[0]["lease_id"] == nil || got[0]["delivered_at"] == nil {
		t.Fatalf("not the first two in order: %v", got)
	}
	if more := h.lease("b", nil); len(more) != 0 {
		t.Fatal("leased past the prefetch", more)
	}
	if u, d := h.queueStatus("b"); u != 3 || d != 0 {
		t.Fatal(u, d)
	}
	if o := outcomes(h.call("ack", "b", ackOne(ids[0], got[0]["lease_id"])), "acks"); !slices.Equal(o, []string{"acked"}) {
		t.Fatal(o)
	}
	if o := outcomes(h.call("ack", "b", ackOne(ids[0], got[0]["lease_id"])), "acks"); !slices.Equal(o, []string{"already_acked"}) {
		t.Fatal("a second ack is fine and changes nothing", o)
	}
	next := h.lease("b", nil)
	if len(next) != 1 || next[0]["id"] != ids[2] || next[0]["recipient_seq"] != int64(3) {
		t.Fatal("an ack frees a prefetch slot for the next in order", next)
	}
	if u, _ := h.queueStatus("b"); u != 2 {
		t.Fatal(u)
	}
	// Acked is handled, so it is read: unread inbox no longer shows it.
	for _, m := range h.call("inbox", "b", nil).(map[string]any)["messages"].([]map[string]any) {
		if m["id"] == ids[0] {
			t.Fatal("an acked message is still unread")
		}
	}
	// Old clients take exactly seven fields: the counts come only when asked.
	if st := h.call("notification_status", "b", nil).(map[string]any); len(st) != 7 {
		t.Fatal("notification_status changed", st)
	}
	if st := h.call("inbox_status", "b", map[string]any{}).(map[string]any); st["unacked_count"] != nil {
		t.Fatal("inbox_status changed", st)
	}
	// A held session takes no lease.
	h.call("set_notification_policy", "c", map[string]any{"policy": "hold"})
	h.call("send", "a", map[string]any{"to": "c", "body": "held", "operation_id": "held"})
	if out := h.call("lease", "c", nil).(map[string]any); len(out["messages"].([]map[string]any)) != 0 || out["policy"] != "hold" {
		t.Fatal("a held session leased", out)
	}
}

func TestQueueRunOutLeasesAreOfferedAgainThenDead(t *testing.T) {
	h := newHarness(t)
	move := skew(t)
	h.s.queue.Attempts = 2
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	id := h.call("send", "a", map[string]any{"to": "b", "body": "the secret plan", "operation_id": "doomed"}).(map[string]any)["id"]
	first := h.lease("b", nil)
	move(11 * time.Minute)
	again := h.lease("b", nil)
	if len(again) != 1 || again[0]["id"] != id || again[0]["attempt"] != int64(2) || again[0]["lease_id"] == first[0]["lease_id"] {
		t.Fatal("a run-out lease is offered again with the attempt counted", again)
	}
	move(11 * time.Minute)
	if res, err := h.s.SweepDeliveries(context.Background()); err != nil || res.Dead != 1 || res.Reoffered != 0 {
		t.Fatal(res, err)
	}
	if got := h.lease("b", nil); len(got) != 0 {
		t.Fatal("a dead letter was offered", got)
	}
	if u, d := h.queueStatus("b"); u != 0 || d != 1 {
		t.Fatal(u, d)
	}
	// An ack after death is dead; the work isn't done.
	if o := outcomes(h.call("ack", "b", ackOne(id, again[0]["lease_id"])), "acks"); !slices.Equal(o, []string{"dead"}) {
		t.Fatal(o)
	}
	// The coordinator is told the count and the session, never the subject.
	notices := h.call("inbox", "c", nil).(map[string]any)["messages"].([]map[string]any)
	if len(notices) != 1 || notices[0]["kind"] != deadLetterKind || !strings.Contains(notices[0]["body"].(string), "1 ClawBridge message(s) to session b") || strings.Contains(notices[0]["body"].(string), "secret") || notices[0]["sender"] != SystemSessionName {
		t.Fatal(notices)
	}
	list := h.call("deadletters_list", "c", map[string]any{"target_session": "b"}).(map[string]any)["dead_letters"].([]map[string]any)
	if len(list) != 1 || list[0]["message_id"] != id || list[0]["attempts"] != int64(2) || list[0]["body"] != nil || list[0]["recipient"] != "b" {
		t.Fatal(list)
	}
	if all := h.call("deadletters_list", "c", map[string]any{"target_session": "@all"}).(map[string]any)["dead_letters"].([]map[string]any); len(all) != 1 {
		t.Fatal(all)
	}
	// Requeue starts it over; history names who did it (L5).
	h.s.queue.Lease, h.s.queue.Attempts = time.Minute, 1
	h.call("deadletters_requeue", "c", map[string]any{"target_session": "b", "message_id": id})
	back := h.lease("b", nil)
	if len(back) != 1 || back[0]["attempt"] != int64(1) {
		t.Fatal(back)
	}
	var by string
	if err := h.s.db.QueryRow(`SELECT changed_by_session FROM history WHERE entity='delivery' AND action='requeued'`).Scan(&by); err != nil || by != h.sessionID("c") {
		t.Fatal(by, err)
	}
	// Dying again within ten minutes of the notice makes no second one.
	move(2 * time.Minute)
	h.call("lease", "b", nil)
	if _, d := h.queueStatus("b"); d != 1 {
		t.Fatal("it didn't die again", d)
	}
	if n := len(h.call("inbox", "c", nil).(map[string]any)["messages"].([]map[string]any)); n != 1 {
		t.Fatal("the notice wasn't throttled", n)
	}
	h.call("deadletters_dismiss", "b", map[string]any{"message_id": id})
	if _, d := h.queueStatus("b"); d != 0 {
		t.Fatal("dismiss leaves it dead", d)
	}
	h.fail("deadletters_dismiss", "b", map[string]any{"message_id": id})
	// L1: the coordinator's own dead letter makes no notice in its queue,
	// and neither does a dead notice.
	h.call("send", "a", map[string]any{"to": "c", "body": "to the coordinator", "operation_id": "own"})
	h.lease("c", nil)
	move(2 * time.Minute)
	h.call("lease", "c", nil)
	// It held the earlier notice too, unacked: both die, and neither makes a
	// notice.
	if _, d := h.queueStatus("c"); d != 2 {
		t.Fatal("the coordinator's messages didn't die", d)
	}
	for _, m := range h.call("inbox", "c", nil).(map[string]any)["messages"].([]map[string]any) {
		if strings.Contains(m["body"].(string), "to session c ") {
			t.Fatal("the coordinator was told of its own dead letter", m["body"])
		}
	}
}

func TestQueueNackAndExtend(t *testing.T) {
	h := newHarness(t)
	move := skew(t)
	id := h.send("n")["id"]
	l := h.lease("b", nil)[0]["lease_id"]
	if out := h.call("nack", "b", map[string]any{"message_id": id, "lease_id": "lse_other"}).(map[string]any); out["outcome"] != "stale" {
		t.Fatal(out)
	}
	if out := h.call("nack", "b", map[string]any{"message_id": id, "lease_id": l, "delay_seconds": 60}).(map[string]any); out["outcome"] != "released" || out["available_at"] == nil {
		t.Fatal(out)
	}
	if got := h.lease("b", nil); len(got) != 0 {
		t.Fatal("offered before its delay", got)
	}
	move(61 * time.Second)
	got := h.lease("b", nil)
	if len(got) != 1 || got[0]["attempt"] != int64(1) {
		t.Fatal("a nack counted an attempt", got)
	}
	ext := h.call("extend", "b", map[string]any{"leases": []any{got[0]["lease_id"], "lse_unknown"}, "seconds": 3600}).(map[string]any)
	if o := outcomes(ext, "leases"); !slices.Equal(o, []string{"extended", "not_found"}) {
		t.Fatal(o)
	}
	move(30 * time.Minute)
	if res, _ := h.s.SweepDeliveries(context.Background()); res.Reoffered != 0 {
		t.Fatal("an extended lease ran out early")
	}
	move(31 * time.Minute)
	if out := h.call("extend", "b", map[string]any{"leases": []any{got[0]["lease_id"]}, "seconds": 60}).(map[string]any); !slices.Equal(outcomes(out, "leases"), []string{"not_found"}) {
		t.Fatal("a run-out lease was extended", out)
	}
	h.fail("extend", "b", map[string]any{"leases": []any{got[0]["lease_id"]}, "seconds": 3601})
	h.fail("nack", "b", map[string]any{"message_id": id, "lease_id": l, "delay_seconds": 3601})
	h.fail("nack", "b", map[string]any{"message_id": id, "lease_id": l, "delay_seconds": -1})
}

// M2: only the recipient, at its current instance, acks, nacks and extends;
// another session's message or lease is not_found.
func TestQueueAcksOnlyTheRecipientsOwn(t *testing.T) {
	h := newHarness(t)
	id := h.send("own")["id"]
	l := h.lease("b", nil)[0]["lease_id"]
	if o := outcomes(h.call("ack", "c", ackOne(id, l)), "acks"); !slices.Equal(o, []string{"not_found"}) {
		t.Fatal(o)
	}
	if out := h.call("nack", "c", map[string]any{"message_id": id, "lease_id": l}).(map[string]any); out["outcome"] != "not_found" {
		t.Fatal(out)
	}
	if o := outcomes(h.call("extend", "c", map[string]any{"leases": []any{l}, "seconds": 60}), "leases"); !slices.Equal(o, []string{"not_found"}) {
		t.Fatal(o)
	}
	next := h.call("register_session", "b", map[string]any{"instance": "b-instance", "resume": true}).(map[string]any)
	for _, op := range []string{"ack", "nack", "extend", "lease"} {
		h.fail(op, "b", map[string]any{"acks": []any{map[string]any{"message_id": id}}})
	}
	// The live instance acks, even with the lease its predecessor took.
	if o := outcomes(h.call("ack", "b", map[string]any{"instance": next["instance"], "acks": []any{map[string]any{"message_id": id, "lease_id": l}}}), "acks"); !slices.Equal(o, []string{"acked"}) {
		t.Fatal(o)
	}
	h.fail("ack", "b", map[string]any{"instance": next["instance"], "acks": []any{map[string]any{"message_id": id, "extra": 1}}})
	h.fail("ack", "b", map[string]any{"instance": next["instance"], "acks": []any{}})
}

// M7: a reply's replay still applies its ack. The first reply went without
// one; the message ran out and came back; the same reply with the new lease
// acks it, once, with no second reply.
func TestReplyReplayStillAcks(t *testing.T) {
	h := newHarness(t)
	move := skew(t)
	id := h.send("ask")["id"]
	h.lease("b", nil)
	reply := map[string]any{"message_id": id, "body": "answer", "operation_id": fmt.Sprintf("msg:%s:1", id)}
	first := h.call("reply", "b", reply).(map[string]any)
	move(11 * time.Minute)
	again := h.lease("b", nil)
	if len(again) != 1 || again[0]["attempt"] != int64(2) {
		t.Fatal(again)
	}
	reply["ack"] = map[string]any{"message_id": id, "lease_id": again[0]["lease_id"]}
	replay := h.call("reply", "b", reply).(map[string]any)
	if replay["id"] != first["id"] || replay["ack"].(map[string]any)["outcome"] != "acked" {
		t.Fatal(replay)
	}
	if replay = h.call("reply", "b", reply).(map[string]any); replay["ack"].(map[string]any)["outcome"] != "already_acked" {
		t.Fatal(replay)
	}
	var replies int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE sender_id=?`, h.sessionID("b")).Scan(&replies); err != nil || replies != 1 {
		t.Fatal(replies, err)
	}
	if u, _ := h.queueStatus("b"); u != 0 {
		t.Fatal(u)
	}
	// Without an ack the result is as it was.
	if plain := h.call("send", "a", map[string]any{"to": "b", "body": "plain", "operation_id": "plain"}).(map[string]any); plain["ack"] != nil {
		t.Fatal(plain)
	}
	h.fail("reply", "b", map[string]any{"message_id": id, "body": "x", "operation_id": "bad-ack", "ack": map[string]any{"message_id": "msg_missing"}})
}

// A reply and the ack of what it answers commit together or not at all.
func TestReplyWithAckIsAtomic(t *testing.T) {
	h := newHarness(t)
	id := h.send("atomic")["id"]
	l := h.lease("b", nil)[0]["lease_id"]
	if _, err := h.s.db.Exec(`CREATE TRIGGER fail_ack BEFORE UPDATE OF acked_at ON deliveries BEGIN SELECT RAISE(ABORT,'injected ack failure'); END`); err != nil {
		t.Fatal(err)
	}
	reply := map[string]any{"message_id": id, "body": "answer", "operation_id": "atomic-reply", "ack": map[string]any{"message_id": id, "lease_id": l}}
	h.fail("reply", "b", reply)
	var replies int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE sender_id=?`, h.sessionID("b")).Scan(&replies); err != nil || replies != 0 {
		t.Fatal("the reply committed without its ack", replies, err)
	}
	if _, err := h.s.db.Exec(`DROP TRIGGER fail_ack`); err != nil {
		t.Fatal(err)
	}
	if out := h.call("reply", "b", reply).(map[string]any); out["ack"].(map[string]any)["outcome"] != "acked" {
		t.Fatal(out)
	}
}

func TestMarkReadAcks(t *testing.T) {
	h := newHarness(t)
	id := h.send("read")["id"]
	h.call("mark_read", "b", map[string]any{"ids": []any{id}})
	if u, _ := h.queueStatus("b"); u != 0 {
		t.Fatal(u)
	}
	if got := h.lease("b", nil); len(got) != 0 {
		t.Fatal("a read message was offered", got)
	}
}

// Each recipient's deliveries are numbered 1..n with no gap, in commit
// order, however sends interleave.
func TestRecipientSeqIsGapFreeUnderConcurrentSends(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for g := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			from := []string{"a", "b", "c"}[g%3]
			for i := range 5 {
				r := h.request("send", from, map[string]any{"to": "@all", "body": "burst", "operation_id": fmt.Sprintf("g%d-%d", g, i)})
				if _, err := h.s.Execute(context.Background(), r); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	assertGapFree(t, h.s.db, h.projectID(), true)
}

// assertGapFree checks each recipient's recipient_seq in project pid is
// 1..n, matches its counter, and, when ordered, follows message order.
func assertGapFree(t *testing.T, db *sql.DB, pid string, ordered bool) {
	t.Helper()
	if !regexp.MustCompile(`^prj_[0-9a-f]{32}$`).MatchString(pid) {
		t.Fatal("not a project ID", pid)
	}
	rows, err := db.Query(`SELECT d.project_id,d.recipient_id,d.recipient_seq,c.seq FROM deliveries d JOIN messages m ON m.project_id=d.project_id AND m.id=d.message_id JOIN delivery_counters c ON c.project_id=d.project_id AND c.recipient_id=d.recipient_id WHERE d.project_id='` + pid + `' ORDER BY d.project_id,d.recipient_id,m.sequence`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string][]int64{}
	counter := map[string]int64{}
	for rows.Next() {
		var pid, rid string
		var seq, c int64
		if err := rows.Scan(&pid, &rid, &seq, &c); err != nil {
			t.Fatal(err)
		}
		seen[pid+"/"+rid] = append(seen[pid+"/"+rid], seq)
		counter[pid+"/"+rid] = c
	}
	if len(seen) == 0 {
		t.Fatal("no deliveries")
	}
	for k, seqs := range seen {
		if !ordered {
			slices.Sort(seqs)
		}
		for i, s := range seqs {
			if s != int64(i+1) {
				t.Fatalf("%s: %v is not 1..%d in message order", k, seqs, len(seqs))
			}
		}
		if counter[k] != int64(len(seqs)) {
			t.Fatalf("%s: counter %d for %d deliveries", k, counter[k], len(seqs))
		}
	}
}

// insertDeliveries itself, from 20 transactions taking crossing recipient
// orders, on whatever backend db is.
func crossingDeliveries(t *testing.T, s *Store, pid, sender string, recipients []string, hold string) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for g := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order := slices.Clone(recipients)
			rand.New(rand.NewSource(int64(g))).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			if g%2 == 1 {
				slices.Reverse(order)
			}
			for range 5 {
				if err := func() error {
					raw, err := s.db.BeginTx(context.Background(), nil)
					if err != nil {
						return err
					}
					tx := &transaction{Tx: raw, postgres: s.postgres}
					defer tx.Rollback()
					id, now := newID("msg_"), stamp()
					if _, err = tx.ExecContext(context.Background(), `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'message','crossing',?,?,?,?,?,NULL)`, id, pid, pid, sender, "@all", id, now, layout(time.Now().AddDate(0, 0, 1)), systemPrincipal, newID("x_"), digestOf("crossing")); err != nil {
						return err
					}
					if err = insertDeliveries(context.Background(), tx, pid, id, order); err != nil {
						return err
					}
					if hold != "" {
						if _, err = tx.ExecContext(context.Background(), hold); err != nil {
							return err
						}
					}
					return tx.Commit()
				}(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestCrossingDeliveriesSQLite(t *testing.T) {
	h := newHarness(t)
	crossingDeliveries(t, h.s, h.projectID(), h.sessionID("a"), []string{h.sessionID("a"), h.sessionID("b"), h.sessionID("c")}, "")
	assertGapFree(t, h.s.db, h.projectID(), false)
}

// queueDowngradeSQLite returns a schema-36 database to schema 34's shape:
// the stream's schema, then the queue's.
const queueDowngradeSQLite = `DROP TABLE stream_tickets;
ALTER TABLE deliveries DROP COLUMN lease_consumer;
DROP INDEX deliveries_recipient_seq;
DROP INDEX deliveries_lease_due;
ALTER TABLE deliveries DROP COLUMN recipient_seq;
ALTER TABLE deliveries DROP COLUMN lease_id;
ALTER TABLE deliveries DROP COLUMN lease_until;
ALTER TABLE deliveries DROP COLUMN lease_instance;
ALTER TABLE deliveries DROP COLUMN attempts;
ALTER TABLE deliveries DROP COLUMN acked_at;
ALTER TABLE deliveries DROP COLUMN dead_at;
ALTER TABLE deliveries DROP COLUMN ack_operation_id;
DROP TABLE delivery_counters;
`

// queueDowngradePostgres returns a schema-36 database in schema to schema
// 34's shape; dropping the columns drops their indexes.
func queueDowngradePostgres(schema string) string {
	return `DROP TABLE ` + schema + `.stream_tickets; ALTER TABLE ` + schema + `.deliveries DROP COLUMN lease_consumer, DROP COLUMN recipient_seq, DROP COLUMN lease_id, DROP COLUMN lease_until, DROP COLUMN lease_instance, DROP COLUMN attempts, DROP COLUMN acked_at, DROP COLUMN dead_at, DROP COLUMN ack_operation_id; DROP TABLE ` + schema + `.delivery_counters; `
}

// N3: a schema-34 store with read and unread rows upgrades: deliveries are
// numbered in message order, what was read is acked, and new sends follow on.
func TestQueueBackfillFromSchema34(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t, s}
	h.call("workspace_init", "", map[string]any{"mode": "local"})
	h.call("project_create", "", map[string]any{"slug": "p"})
	for _, name := range []string{"a", "b", "c"} {
		h.call("register_session", name, map[string]any{"instance": name + "-instance", "role": "worker"})
	}
	var ids []any
	for i := range 3 {
		ids = append(ids, h.send(fmt.Sprintf("old%d", i))["id"])
	}
	h.call("send", "a", map[string]any{"to": "@all", "body": "everyone", "operation_id": "all"})
	h.call("mark_read", "b", map[string]any{"ids": []any{ids[1]}})
	if _, err = s.db.Exec(budgetDowngradeSQLite + maintenanceDowngradeSQLite + ledgerDowngradeSQLite + lanesDowngradeSQLite + noticeDowngradeSQLite + leadDowngradeSQLite + rolesDowngradeSQLite + kindDowngradeSQLite + viewsDowngradeSQLite + queueDowngradeSQLite + `PRAGMA user_version=34;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h = &harness{t, s}
	assertGapFree(t, s.db, h.projectID(), true)
	var acked sql.NullString
	var read string
	if err = s.db.QueryRow(`SELECT acked_at,read_at FROM deliveries WHERE message_id=? AND recipient_id=?`, ids[1], h.sessionID("b")).Scan(&acked, &read); err != nil || acked.String != read {
		t.Fatal("a read delivery isn't acked as of its read", acked, read, err)
	}
	if u, d := h.queueStatus("b"); u != 3 || d != 0 {
		t.Fatal(u, d)
	}
	got := h.lease("b", nil)
	if len(got) != 3 || got[0]["id"] != ids[0] || got[1]["id"] != ids[2] || got[1]["recipient_seq"] != int64(3) {
		t.Fatal(got)
	}
	next := h.send("new")["id"]
	var seq int64
	if err = s.db.QueryRow(`SELECT recipient_seq FROM deliveries WHERE message_id=? AND recipient_id=?`, next, h.sessionID("b")).Scan(&seq); err != nil || seq != 5 {
		t.Fatal("a new send doesn't follow the backfill", seq, err)
	}
}
