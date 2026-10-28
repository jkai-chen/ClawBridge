package bus

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// outboxFixture is a project on the reviewer's Postgres and a clock.
func outboxFixture(t *testing.T) (*pgIdentityFixture, string, time.Time) {
	t.Helper()
	f := newPGIdentityFixture(t)
	var pid string
	if err := f.s.db.QueryRowContext(context.Background(), `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	// Its worker sees only this project, at a time no other package's
	// worker reaches: the database is shared.
	f.s.outboxOnly = pid
	return f, pid, time.Now().UTC().AddDate(50, 0, 0).Truncate(time.Millisecond)
}

// mine is leased writes of project pid: the database is shared with
// other tests' projects, and one worker serves every project.
func mine(ws []OutboxWrite, pid string) []OutboxWrite {
	var out []OutboxWrite
	for _, w := range ws {
		if w.ProjectID == pid {
			out = append(out, w)
		}
	}
	return out
}

// secretBody stands for a write's text: it must never leave the request.
const secretBody = "synthetic body that must never be shown"

func queued(pid, op, key string, now time.Time) OutboxWrite {
	request, _ := json.Marshal(map[string]any{"args": map[string]any{"body": secretBody}})
	return OutboxWrite{ProjectID: pid, Principal: "person@identity-test.example", OperationID: op, Digest: "d-" + op, Request: string(request), Operation: "comment", Issue: key, OrderKey: key, Reason: "linear_rate_limit", NextAttemptAt: now, CreatedAt: now, ExpiresAt: now.Add(3 * time.Hour)}
}

// A write is queued once per operation_id: the same request returns the
// queued write, a different one is refused; writes on one issue go in
// order, each only once the one before it settled.
func TestPostgresOutboxQueuesInOrder(t *testing.T) {
	f, pid, now := outboxFixture(t)
	ctx := context.Background()
	first, created, err := f.s.OutboxEnqueue(ctx, queued(pid, "op-1", "issue-a", now))
	if err != nil || !created || first.Position != 1 || first.State != OutboxQueued {
		t.Fatal(first, created, err)
	}
	second, _, err := f.s.OutboxEnqueue(ctx, queued(pid, "op-2", "issue-a", now.Add(time.Millisecond)))
	if err != nil || second.Position != 2 {
		t.Fatal(second, err)
	}
	if again, created, err := f.s.OutboxEnqueue(ctx, queued(pid, "op-1", "issue-a", now.Add(time.Second))); err != nil || created || again.ID != first.ID || again.Position != 1 {
		t.Fatal("a repeat was queued again:", again, created, err)
	}
	other := queued(pid, "op-1", "issue-a", now)
	other.Digest = "different"
	if _, _, err := f.s.OutboxEnqueue(ctx, other); err == nil || !strings.Contains(err.Error(), "different write") {
		t.Fatal("an operation_id reused for another write was queued:", err)
	}
	if _, _, err := f.s.OutboxEnqueue(ctx, queued(pid, "op-3", "issue-b", now)); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.OutboxBehind(ctx, pid, "issue-a"); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	leased, err := f.s.OutboxLease(ctx, "w1", now.Add(time.Second), 90*time.Second, 20)
	leased = mine(leased, pid)
	if err != nil || len(leased) != 2 || leased[0].OperationID == "op-2" || leased[1].OperationID == "op-2" {
		t.Fatal("not just each key's head was leased:", leased, err)
	}
	if again, _ := f.s.OutboxLease(ctx, "w2", now.Add(time.Second), 90*time.Second, 20); len(mine(again, pid)) != 0 {
		t.Fatal("a leased write, or one behind it, was leased again:", again)
	}
	var head OutboxWrite
	for _, w := range leased {
		if w.OperationID == "op-1" {
			head = w
		}
	}
	if ok, err := f.s.OutboxSettle(ctx, head.ID, "w2", OutboxDone, "", "comment c-1", now); err != nil || ok {
		t.Fatal("a write was settled without its lease:", ok, err)
	}
	if ok, err := f.s.OutboxSettle(ctx, head.ID, "w1", OutboxDone, "", "comment c-1", now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	next, err := f.s.OutboxLease(ctx, "w1", now.Add(time.Second), 90*time.Second, 20)
	next = mine(next, pid)
	if err != nil || len(next) != 1 || next[0].OperationID != "op-2" {
		t.Fatal("the next write on the issue did not follow:", next, err)
	}
}

// A failed or uncertain head unblocks its key, as a done one does (Q3).
func TestPostgresOutboxFailedHeadUnblocks(t *testing.T) {
	f, pid, now := outboxFixture(t)
	ctx := context.Background()
	for i, op := range []string{"op-f", "op-u"} {
		f.s.OutboxEnqueue(ctx, queued(pid, op, "issue-x", now.Add(time.Duration(i)*time.Millisecond)))
	}
	f.s.OutboxEnqueue(ctx, queued(pid, "op-last", "issue-x", now.Add(time.Second)))
	for _, state := range []string{OutboxFailed, OutboxUncertain} {
		leased, _ := f.s.OutboxLease(ctx, "w", now.Add(time.Minute), 90*time.Second, 20)
		if leased = mine(leased, pid); len(leased) != 1 {
			t.Fatal(state, leased)
		}
		if ok, _ := f.s.OutboxSettle(ctx, leased[0].ID, "w", state, "refused", "", now); !ok {
			t.Fatal(state)
		}
	}
	if leased, _ := f.s.OutboxLease(ctx, "w", now.Add(time.Minute), 90*time.Second, 20); len(mine(leased, pid)) != 1 || mine(leased, pid)[0].OperationID != "op-last" {
		t.Fatal("a failed or uncertain head kept its key blocked:", leased)
	}
}

// A write not sent goes back in the queue until its next attempt; after
// OutboxMaxAttempts it is dead. A wait doesn't count as an attempt.
func TestPostgresOutboxRetriesThenDead(t *testing.T) {
	f, pid, now := outboxFixture(t)
	ctx := context.Background()
	w, _, _ := f.s.OutboxEnqueue(ctx, queued(pid, "op-r", "issue-r", now))
	f.s.OutboxLease(ctx, "w", now, 90*time.Second, 20)
	if state, err := f.s.OutboxRetry(ctx, w.ID, "w", "project_hourly_cap", now.Add(time.Hour), false, now); err != nil || state != OutboxQueued {
		t.Fatal(state, err)
	}
	if leased, _ := f.s.OutboxLease(ctx, "w", now.Add(time.Minute), 90*time.Second, 20); len(mine(leased, pid)) != 0 {
		t.Fatal("a write was leased before its next attempt:", leased)
	}
	at := now.Add(time.Hour)
	for i := 1; i <= OutboxMaxAttempts; i++ {
		if leased, _ := f.s.OutboxLease(ctx, "w", at, 90*time.Second, 20); len(mine(leased, pid)) != 1 {
			t.Fatal(i, leased)
		}
		state, err := f.s.OutboxRetry(ctx, w.ID, "w", "linear_rate_limit", at, true, at)
		want := OutboxQueued
		if i == OutboxMaxAttempts {
			want = OutboxDead
		}
		if err != nil || state != want {
			t.Fatal(i, state, err)
		}
	}
	got, _, _ := f.s.OutboxGet(ctx, pid, "", "op-r")
	if got.State != OutboxDead || got.Attempts != OutboxMaxAttempts || got.Reason != "linear_rate_limit" {
		t.Fatal(got)
	}
}

// A lease that ran out puts the write back in the queue; a queued write
// past its expiry is expired and never leased.
func TestPostgresOutboxReclaimAndExpire(t *testing.T) {
	f, pid, now := outboxFixture(t)
	ctx := context.Background()
	f.s.OutboxEnqueue(ctx, queued(pid, "op-l", "issue-l", now))
	if leased, _ := f.s.OutboxLease(ctx, "gone", now, 90*time.Second, 20); len(mine(leased, pid)) != 1 {
		t.Fatal(leased)
	}
	f.s.OutboxReclaim(ctx, now.Add(89*time.Second))
	if got, _, _ := f.s.OutboxGet(ctx, pid, "", "op-l"); got.State != OutboxSending {
		t.Fatal("a live lease was reclaimed:", got.State)
	}
	if n, err := f.s.OutboxReclaim(ctx, now.Add(90*time.Second)); err != nil || n < 1 {
		t.Fatal(n, err)
	}
	if got, _, _ := f.s.OutboxGet(ctx, pid, "", "op-l"); got.State != OutboxQueued {
		t.Fatal("a run-out lease was not reclaimed:", got.State)
	}
	late := queued(pid, "op-e", "issue-e", now)
	late.ExpiresAt = now.Add(time.Minute)
	f.s.OutboxEnqueue(ctx, late)
	if _, err := f.s.OutboxExpire(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := f.s.OutboxGet(ctx, pid, "", "op-e")
	if got.State != OutboxExpired {
		t.Fatal(got)
	}
	st, err := f.s.OutboxStatusOf(ctx, pid, now.Add(2*time.Minute))
	if err != nil || st.Queued != 1 || st.Expired24h != 1 || st.OldestQueued != 120 || st.Waiting != "linear_rate_limit" {
		t.Fatalf("%+v %v", st, err)
	}
}

// What anyone sees of a queued write is typed fields only: its text stays
// in the request, out of the view, the notice and the audit (R3). A
// sessionless write's notice is recorded, not sent, and only once.
func TestPostgresOutboxNeverShowsTheBody(t *testing.T) {
	f, pid, now := outboxFixture(t)
	ctx := context.Background()
	w, _, _ := f.s.OutboxEnqueue(ctx, queued(pid, "op-n", "issue-n", now))
	f.s.OutboxLease(ctx, "w", now, 90*time.Second, 20)
	f.s.OutboxSettle(ctx, w.ID, "w", OutboxDone, "", "comment c-9", now)
	list, err := f.s.OutboxList(ctx, pid, "", "person@identity-test.example", 10)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	view, _ := json.Marshal(list[0].View())
	if strings.Contains(string(view), secretBody) || strings.Contains(outboxNotice(list[0]), secretBody) {
		t.Fatal("a queued write's text was shown:", string(view))
	}
	if others, _ := f.s.OutboxList(ctx, pid, "", "someone-else@identity-test.example", 10); len(others) != 0 {
		t.Fatal("another person saw a sessionless write:", others)
	}
	if err = f.s.OutboxNotify(ctx, w.ID, now); err != nil {
		t.Fatal(err)
	}
	if err = f.s.OutboxNotify(ctx, w.ID, now); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT COUNT(*) FROM history WHERE entity='linear_write' AND entity_key='op-n' AND details NOT LIKE '%`+secretBody+`%' AND changed_by_principal=$1`, systemPrincipal); n != 1 {
		t.Fatal("the settle was not audited once, typed:", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM messages WHERE kind='linear-write' AND project_id=$1`, pid); n != 0 {
		t.Fatal("a sessionless write sent a notice:", n)
	}
	if ids, _ := f.s.OutboxUnnotified(ctx, 1000); slices.Contains(ids, w.ID) {
		t.Fatal("a notified write is still owed its notice")
	}
	if err = f.s.OutboxPrune(ctx, now.Add(OutboxKeep+time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.s.OutboxGet(ctx, pid, "", "op-n"); ok {
		t.Fatal("a write settled past OutboxKeep was kept")
	}
}

// A dead write tells its session and the project's coordinator; the
// coordinator hears of dead writes at most once in 10 minutes.
func TestPostgresOutboxDeadTellsTheCoordinatorOnce(t *testing.T) {
	f, pid, _ := outboxFixture(t)
	// History is stamped on the host's clock, which the throttle reads.
	now := time.Now().UTC()
	ctx := context.Background()
	for _, s := range []string{"agent", "coord"} {
		f.must(f.owner, "sub-owner", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: s, Args: map[string]any{"instance": s + "-i", "role": "worker"}})
	}
	f.must(f.owner, "sub-owner", Request{Operation: "coordinator_set", Workspace: f.workspace, Project: "p", Session: "coord", Args: map[string]any{"instance": "coord-i", "target_session": "coord"}})
	var agent string
	if err := f.s.db.QueryRowContext(ctx, `SELECT id FROM sessions WHERE project_id=$1 AND name='agent'`, pid).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"d-1", "d-2"} {
		w := queued(pid, op, "issue-"+op, now)
		w.SessionID = agent
		q, _, err := f.s.OutboxEnqueue(ctx, w)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.db.ExecContext(ctx, `UPDATE linear_write_outbox SET state='dead',attempts=8,settled_at=$1 WHERE id=$2`, layout(now), q.ID); err != nil {
			t.Fatal(err)
		}
		if err = f.s.OutboxNotify(ctx, q.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]int{}
	rows, err := f.s.db.QueryContext(ctx, `SELECT s.name FROM messages m JOIN deliveries d ON d.message_id=m.id JOIN sessions s ON s.id=d.recipient_id WHERE m.project_id=$1 AND m.kind='linear-write'`, pid)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		rows.Scan(&name)
		got[name]++
	}
	rows.Close()
	if got["agent"] != 2 || got["coord"] != 1 {
		t.Fatal("dead notices:", got)
	}
}
