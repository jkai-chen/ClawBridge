package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"github.com/jackc/pgx/v5"
)

// newOutboxFixture is an app writer fixture whose queued writes are purged
// when the test ends: one worker serves every project in the shared
// database, so a later test's worker would send them.
func newOutboxFixture(t *testing.T) *awFixture {
	f := newAWFixture(t, awOrg)
	pid := f.projectID()
	t.Cleanup(func() {
		if err := f.store.OutboxPurgeProject(context.Background(), pid); err != nil {
			t.Error(err)
		}
	})
	return f
}

// queueWrite is f.write from a client that accepts queueing.
func (f *awFixture) queueWrite(operation string, args map[string]any) (bus.Result, error) {
	return f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: "linear_write", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i", "operation": operation, "args": args, "accept_queued": true}})
}

// later runs the outbox worker once, d from now: past any wait Linear asked
// for.
func (f *awFixture) later(d time.Duration) {
	f.writer.Now = func() time.Time { return time.Now().Add(d) }
	f.writer.outboxCycle(context.Background(), "test-worker")
	f.writer.Now = nil
}

func (f *awFixture) projectID() string {
	a, err := f.writer.access(context.Background(), f.identity(), bus.Request{Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i"}})
	if err != nil {
		f.t.Fatal(err)
	}
	return a.ProjectID
}

func (f *awFixture) queued(op string) bus.OutboxWrite {
	f.t.Helper()
	a, err := f.writer.access(context.Background(), f.identity(), bus.Request{Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i"}})
	if err != nil {
		f.t.Fatal(err)
	}
	q, ok, err := f.store.OutboxGet(context.Background(), a.ProjectID, a.SessionID, op)
	if err != nil || !ok {
		f.t.Fatal("not queued:", op, err)
	}
	return q
}

// linearWriteNotices are the linear-write messages the session was sent.
func (f *awFixture) linearWriteNotices() []string {
	f.t.Helper()
	out, err := f.store.ExecuteAsIdentity(context.Background(), f.identity(), bus.Request{Operation: "inbox", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i"}})
	if err != nil {
		f.t.Fatal(err)
	}
	raw, _ := json.Marshal(out.Data)
	var inbox struct {
		Messages []struct {
			Kind string `json:"kind"`
			Body string `json:"body"`
		} `json:"messages"`
	}
	json.Unmarshal(raw, &inbox)
	var bodies []string
	for _, m := range inbox.Messages {
		if m.Kind == "linear-write" {
			bodies = append(bodies, m.Body)
		}
	}
	return bodies
}

const queuedBody = "synthetic queued comment text"

// A write Linear rate-limits is queued for a client that accepts it,
// answered with typed fields only; a repeat answers the same; the worker
// sends it once Linear allows, the session is told, and a later repeat
// gets the receipt without sending again. An older client is refused as
// before.
func TestPostgresOutboxQueuesARateLimitedWrite(t *testing.T) {
	f := newOutboxFixture(t)
	// An older client: refused as before, nothing queued.
	f.mcp.refuse = 1
	_, err := f.write("comment", map[string]any{"id": awIssue, "body": "old client", "operation_id": "old-1"})
	if code, status := serviceError(err); err == nil || code != "rate_limited" || status != 429 || !strings.Contains(responseMessage(code, err), "nothing was sent") {
		t.Fatal("an old client's rate-limited write was not refused as before:", err)
	}
	if a, e := f.writer.access(context.Background(), f.identity(), bus.Request{Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i"}}); e != nil {
		t.Fatal(e)
	} else if _, queued, e := f.store.OutboxGet(context.Background(), a.ProjectID, a.SessionID, "old-1"); e != nil || queued {
		t.Fatal("an old client's refused write was queued", e)
	}
	f.mcp.refuse = 1
	args := map[string]any{"id": awIssue, "body": queuedBody, "operation_id": "q-1"}
	res, err := f.queueWrite("comment", args)
	if err != nil {
		t.Fatal(err)
	}
	got := awJSON(t, res.Data)
	raw, _ := json.Marshal(res.Data)
	if got["outcome"] != "queued" || got["reason"] != reasonRateLimit || got["position"] != float64(1) || got["operation_id"] != "q-1" || strings.Contains(string(raw), queuedBody) {
		t.Fatal("not queued, or the answer shows the text:", string(raw))
	}
	if n := f.mcp.count("save_comment"); n != 0 {
		t.Fatal("save_comment ran:", n)
	}
	if again, err := f.queueWrite("comment", args); err != nil || awJSON(t, again.Data)["outcome"] != "queued" {
		t.Fatal("a repeat was not answered from the queue:", again, err)
	}
	status, err := f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: "linear_write_status", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i", "operation_id": "q-1"}})
	raw, _ = json.Marshal(status.Data)
	if err != nil || !strings.Contains(string(raw), `"operation_id":"q-1"`) || !strings.Contains(string(raw), `"state":"queued"`) || strings.Contains(string(raw), queuedBody) {
		t.Fatal("write-status:", string(raw), err)
	}
	other := map[string]any{"id": awIssue, "body": "other", "operation_id": "q-1"}
	if _, err := f.queueWrite("comment", other); err == nil || !strings.Contains(err.Error(), "different write") {
		t.Fatal("an operation_id reused for another write was accepted:", err)
	}
	f.mcp.refuse = 0
	f.later(2 * time.Minute)
	if q := f.queued("q-1"); q.State != bus.OutboxDone || !strings.HasPrefix(q.Result, "comment") {
		t.Fatal("the queued write was not sent:", q.State, q.Reason, q.Result)
	}
	if n := f.mcp.count("save_comment"); n != 1 {
		t.Fatal("save_comment sent", n, "times")
	}
	notices := f.linearWriteNotices()
	if len(notices) != 1 || !strings.Contains(notices[0], "q-1") || !strings.Contains(notices[0], ": done.") || strings.Contains(notices[0], queuedBody) {
		t.Fatal("the session was not told once, typed:", notices)
	}
	again, err := f.queueWrite("comment", args)
	if err != nil || awJSON(t, again.Data)["receipt"] == nil || f.mcp.count("save_comment") != 1 {
		t.Fatal("a repeat after the send did not replay the receipt:", again, err)
	}
	f.later(3 * time.Minute)
	if n := len(f.linearWriteNotices()); n != 1 {
		t.Fatal("the notice was sent again:", n)
	}
}

// A write on an issue with a queued write queues behind it, though Linear
// would take it now, and the two go in order (§4.3): the second once the
// head is sent, in the same cycle.
func TestPostgresOutboxKeepsAnIssuesOrder(t *testing.T) {
	f := newOutboxFixture(t)
	f.mcp.refuse = 1
	f.queueWrite("comment", map[string]any{"id": awIssue, "body": "first", "operation_id": "o-1"})
	res, err := f.queueWrite("comment", map[string]any{"id": awIssue, "body": "second", "operation_id": "o-2"})
	if got := awJSON(t, res.Data); err != nil || got["outcome"] != "queued" || got["reason"] != reasonOrdered || got["position"] != float64(2) {
		t.Fatal("the second write overtook or was not queued:", res, err)
	}
	f.later(2 * time.Minute)
	bodies := f.mcp.savedBodies()
	if len(bodies) != 2 || !strings.HasPrefix(bodies[0], "first") || !strings.HasPrefix(bodies[1], "second") {
		t.Fatal("not sent in order:", bodies)
	}
}

// A queued write is sent only if its session is still active (R1): one
// retired while it waited fails, and nothing is sent.
func TestPostgresOutboxRechecksTheSession(t *testing.T) {
	f := newOutboxFixture(t)
	f.mcp.refuse = 1
	f.queueWrite("comment", map[string]any{"id": awIssue, "body": "late", "operation_id": "r-1"})
	f.must(bus.Request{Operation: "retire_session", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i"}})
	f.later(2 * time.Minute)
	q := f.queued("r-1")
	if q.State != bus.OutboxFailed || f.mcp.count("save_comment") != 0 {
		t.Fatal("a retired session's write was sent:", q.State, q.Reason, f.mcp.count("save_comment"))
	}
	if n := len(f.linearWriteNotices()); n != 0 {
		t.Fatal("a retired session was sent a notice:", n)
	}
}

// A queued write is sent only while its person may still write (R1): one
// whose role was revoked while it waited fails, and nothing is sent.
func TestPostgresOutboxRechecksTheRole(t *testing.T) {
	f := newOutboxFixture(t)
	member := awUnique("member") + "@appwrite-test.example"
	f.must(bus.Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": member, "role": "member"}})
	as := bus.Identity{Principal: member, Issuer: bus.GoogleIssuer, Subject: "sub-member"}
	ctx := context.Background()
	if _, err := f.store.ExecuteAsIdentity(ctx, as, bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "member-s", Args: map[string]any{"instance": "member-i", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	f.mcp.refuse = 1
	res, err := f.writer.Execute(ctx, as, bus.Request{Operation: "linear_write", Workspace: f.workspace, Project: "p", Session: "member-s", Args: map[string]any{"instance": "member-i", "operation": "comment", "args": map[string]any{"id": awIssue, "body": "revoked", "operation_id": "m-1"}, "accept_queued": true}})
	if err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
		t.Fatal(res, err)
	}
	f.must(bus.Request{Operation: "project_role_revoke", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": member}})
	f.later(2 * time.Minute)
	if n := f.mcp.count("save_comment"); n != 0 {
		t.Fatal("a revoked member's queued write was sent:", n)
	}
	st, _ := f.store.OutboxStatusOf(ctx, f.projectID(), time.Now())
	if st.Queued != 0 || st.Sending != 0 {
		t.Fatalf("the revoked write did not settle: %+v", st)
	}
}

// Agent writes count against the project's hourly cap (§5): past it, a
// write is queued for a client that accepts it and refused, unsent, for an
// older one; the queued write waits without spending a try, and goes when
// the owner raises the cap.
func TestPostgresOutboxHonoursTheHourlyCap(t *testing.T) {
	f := newOutboxFixture(t)
	f.must(bus.Request{Operation: "comment_watch_budget_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"watch_budget": 1}})
	if _, err := f.queueWrite("comment", map[string]any{"id": awIssue, "body": "fits", "operation_id": "c-1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, used, limit, _, err := f.store.LinearBudgetRoom(ctx, f.projectID())
	if err != nil || used < 2 || limit != 1 {
		t.Fatal("the write's Linear calls were not charged:", used, limit, err)
	}
	// A budget the hour has used exactly is spent.
	f.must(bus.Request{Operation: "comment_watch_budget_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"watch_budget": used}})
	res, err := f.queueWrite("comment", map[string]any{"id": awIssue, "body": "over", "operation_id": "c-2"})
	if got := awJSON(t, res.Data); err != nil || got["outcome"] != "queued" || got["reason"] != reasonCap {
		t.Fatal("a write over the cap was not queued:", res, err)
	}
	if _, err := f.write("comment", map[string]any{"id": awIssue + "x", "body": "old", "operation_id": "c-3"}); err == nil || !strings.Contains(err.Error(), "budget for the hour is spent") || !strings.Contains(err.Error(), "nothing was sent") {
		t.Fatal("an older client's write over the cap was not refused:", err)
	}
	saves := f.mcp.count("save_comment")
	// Due, but the hour's budget is still spent: held again, without a try.
	f.later(2 * time.Hour)
	if q := f.queued("c-2"); q.State != bus.OutboxQueued || q.Attempts != 0 || q.Reason != reasonCap || f.mcp.count("save_comment") != saves {
		t.Fatal("a capped write was sent, or spent a try:", q.State, q.Attempts, q.Reason)
	}
	f.must(bus.Request{Operation: "comment_watch_budget_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"watch_budget": 600}})
	f.later(2 * time.Hour)
	if q := f.queued("c-2"); q.State != bus.OutboxDone {
		t.Fatal("the raise did not let the queued write go:", q.State, q.Reason)
	}
}

// The worker leases each write just before its send, so a cycle of slow
// sends never sends a write after its lease ran out (M1): another replica
// reclaiming during every send finds nothing of this cycle's to take, and
// each write settles as the one who leased it.
func TestPostgresOutboxLeasesEachWriteBeforeItsSend(t *testing.T) {
	f := newOutboxFixture(t)
	f.mcp.refuse = 1
	for _, op := range []string{"l-1", "l-2", "l-3"} {
		if res, err := f.queueWrite("comment", map[string]any{"id": awIssue, "body": op, "operation_id": op}); err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
			t.Fatal(op, res, err)
		}
	}
	var mu sync.Mutex
	at := time.Now().Add(2 * time.Minute)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return at }
	f.writer.Now = clock
	f.mcp.mu.Lock()
	f.mcp.onSave = func() {
		// Each send takes 50 s; three outlast one 90 s lease.
		mu.Lock()
		at = at.Add(50 * time.Second)
		mu.Unlock()
		if _, err := f.store.OutboxReclaim(context.Background(), clock()); err != nil {
			t.Error(err)
		}
	}
	f.mcp.mu.Unlock()
	f.writer.outboxCycle(context.Background(), "test-worker")
	f.writer.Now = nil
	f.mcp.mu.Lock()
	f.mcp.onSave = nil
	f.mcp.mu.Unlock()
	if bodies := f.mcp.savedBodies(); len(bodies) != 3 {
		t.Fatal("not every write was sent once:", bodies)
	}
	for _, op := range []string{"l-1", "l-2", "l-3"} {
		if q := f.queued(op); q.State != bus.OutboxDone {
			t.Fatal(op, "was reclaimed during its send:", q.State, q.Reason)
		}
	}
}

// A queued write proves nothing afresh (M2): once the owner releases the
// Google account that queued it, the write fails as identity_revoked,
// nothing is sent, and the account is not bound again, though the release
// allowed it to bind on its next sign-in.
func TestPostgresOutboxRechecksTheAccount(t *testing.T) {
	f := newOutboxFixture(t)
	member := awUnique("member") + "@appwrite-test.example"
	f.must(bus.Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": member, "role": "member"}})
	as := bus.Identity{Principal: member, Issuer: bus.GoogleIssuer, Subject: "sub-released"}
	ctx := context.Background()
	if _, err := f.store.ExecuteAsIdentity(ctx, as, bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "released-s", Args: map[string]any{"instance": "released-i", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	f.mcp.refuse = 1
	r := bus.Request{Operation: "linear_write", Workspace: f.workspace, Project: "p", Session: "released-s", Args: map[string]any{"instance": "released-i", "operation": "comment", "args": map[string]any{"id": awIssue, "body": "released", "operation_id": "a-1"}, "accept_queued": true}}
	res, err := f.writer.Execute(ctx, as, r)
	if err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
		t.Fatal("the write was not queued:", res, err)
	}
	access, err := f.writer.access(ctx, as, r)
	if err != nil {
		t.Fatal(err)
	}
	if live, err := f.store.IdentityLive(ctx, as); err != nil || !live {
		t.Fatal("the bound account is not live:", live, err)
	}
	if _, err := f.store.OperatorUnbindPrincipal(ctx, member, true); err != nil {
		t.Fatal(err)
	}
	f.later(2 * time.Minute)
	if n := f.mcp.count("save_comment"); n != 0 {
		t.Fatal("a released account's queued write was sent:", n)
	}
	q, ok, err := f.store.OutboxGet(ctx, access.ProjectID, access.SessionID, "a-1")
	if err != nil || !ok || q.State != bus.OutboxFailed || q.Reason != "identity_revoked" {
		t.Fatalf("the write did not fail as identity_revoked: %+v %v", q, err)
	}
	if live, _ := f.store.IdentityLive(ctx, as); live {
		t.Fatal("the queued write bound the released account again")
	}
	// What the send runs under refuses a released account, even past the
	// check above, and never binds it.
	if _, err := f.store.ExecuteAsIdentity(bus.WithoutBinding(ctx), as, bus.Request{Operation: "whoami", Workspace: f.workspace, Project: "p", Session: "released-s", Args: map[string]any{"instance": "released-i"}}); err == nil || !strings.Contains(err.Error(), "no longer bound") {
		t.Fatal("a released account ran without binding:", err)
	}
	if live, _ := f.store.IdentityLive(ctx, as); live {
		t.Fatal("WithoutBinding bound the released account again")
	}
}

// An account released while its queued write is being sent is not bound
// again by what the send records afterwards.
func TestPostgresOutboxNeverBindsDuringTheSend(t *testing.T) {
	f := newOutboxFixture(t)
	member := awUnique("member") + "@appwrite-test.example"
	f.must(bus.Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": member, "role": "member"}})
	as := bus.Identity{Principal: member, Issuer: bus.GoogleIssuer, Subject: "sub-mid-send"}
	ctx := context.Background()
	if _, err := f.store.ExecuteAsIdentity(ctx, as, bus.Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "mid-s", Args: map[string]any{"instance": "mid-i", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	f.mcp.refuse = 1
	if res, err := f.writer.Execute(ctx, as, bus.Request{Operation: "linear_write", Workspace: f.workspace, Project: "p", Session: "mid-s", Args: map[string]any{"instance": "mid-i", "operation": "comment", "args": map[string]any{"id": awIssue, "body": "mid", "operation_id": "b-1"}, "accept_queued": true}}); err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
		t.Fatal(res, err)
	}
	f.mcp.mu.Lock()
	f.mcp.onSave = func() {
		if _, err := f.store.OperatorUnbindPrincipal(ctx, member, true); err != nil {
			t.Error(err)
		}
	}
	f.mcp.mu.Unlock()
	f.later(2 * time.Minute)
	f.mcp.mu.Lock()
	f.mcp.onSave = nil
	f.mcp.mu.Unlock()
	if live, _ := f.store.IdentityLive(ctx, as); live {
		t.Fatal("the send bound the released account again")
	}
}

// linear_app_status shows the project's queue and hourly budget (§8):
// counts, the oldest's age, what most wait for and the calls used, never a
// write's text.
func TestPostgresAppStatusShowsTheQueueAndBudget(t *testing.T) {
	f := newOutboxFixture(t)
	status := func() map[string]any {
		t.Helper()
		res, err := f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: "linear_app_status", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i"}})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(res.Data)
		if strings.Contains(string(raw), queuedBody) {
			t.Fatal("the status shows a write's text:", string(raw))
		}
		return awJSON(t, res.Data)
	}
	first := status()
	if out, _ := first["outbox"].(map[string]any); out == nil || out["queued"] != float64(0) {
		t.Fatal("no empty queue:", out)
	}
	// A store that doesn't serve has no name, so the count is unknown.
	if n, _ := first["server_instances"].(map[string]any); n == nil || n["known"] != false {
		t.Fatal("server_instances:", first["server_instances"])
	}
	f.mcp.refuse = 1
	f.queueWrite("comment", map[string]any{"id": awIssue, "body": queuedBody, "operation_id": "s-1"})
	got := status()
	out, _ := got["outbox"].(map[string]any)
	budget, _ := got["budget"].(map[string]any)
	if out["queued"] != float64(1) || out["waiting"] != reasonRateLimit || out["dead_24h"] != float64(0) {
		t.Fatal("the queued write is not counted:", out)
	}
	if budget["limit"] != float64(bus.LinearBudgetDefault) || budget["source"] == "" || budget["window_resets_at"] == nil {
		t.Fatal("no budget:", budget)
	}
	before := budget["used"].(float64)
	// The project's queue view: any member, without a session, typed
	// fields only; a scope it doesn't know is refused.
	list, err := f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: "linear_write_status", Workspace: f.workspace, Project: "p", Args: map[string]any{"scope": "project"}})
	raw, _ := json.Marshal(list.Data)
	if err != nil || !strings.Contains(string(raw), `"operation_id":"s-1"`) || !strings.Contains(string(raw), `"state":"queued"`) || strings.Contains(string(raw), queuedBody) {
		t.Fatal("the project's queue:", string(raw), err)
	}
	if _, err := f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: "linear_write_status", Workspace: f.workspace, Project: "p", Args: map[string]any{"scope": "workspace"}}); err == nil {
		t.Fatal("an unknown scope was accepted")
	}
	f.mcp.refuse = 0
	f.later(2 * time.Minute)
	got = status()
	out, _ = got["outbox"].(map[string]any)
	if out["queued"] != float64(0) || got["budget"].(map[string]any)["used"].(float64) <= before {
		t.Fatal("the sent write is still queued or was not charged:", got)
	}
}

// serve names every connection it keeps open, its pool and the stream
// hub's listener, so one server counts as one known instance and two as
// two; an unnamed listener makes the count unknown. It runs as a role of
// its own, so other tests' connections don't count.
func TestPostgresServersCountThemselves(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	var super bool
	var me string
	if err = admin.QueryRow(`SELECT rolsuper,current_user FROM pg_roles WHERE rolname=current_user`).Scan(&super, &me); err != nil || !super {
		t.Skip("needs a superuser to make a role of its own", err)
	}
	var b [6]byte
	rand.Read(b[:])
	role, password := "cb_instances_"+hex.EncodeToString(b[:]), hex.EncodeToString(b[:])+"-pw"
	if _, err = admin.Exec(`CREATE ROLE ` + pgx.Identifier{role}.Sanitize() + ` LOGIN PASSWORD '` + password + `' IN ROLE ` + pgx.Identifier{me}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	roleDSN := u.String()
	var stops []func()
	t.Cleanup(func() {
		for _, stop := range stops {
			stop()
		}
		admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename=$1`, role)
		var err error
		for i := 0; i < 50; i++ {
			if _, err = admin.Exec(`DROP OWNED BY ` + pgx.Identifier{role}.Sanitize()); err == nil {
				if _, err = admin.Exec(`DROP ROLE ` + pgx.Identifier{role}.Sanitize()); err == nil {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("the test role was not dropped:", role, err)
	})
	serve := func(named bool) *bus.Store {
		t.Helper()
		store, err := openServingStore(ctx, roleDSN, "owner@instances-test.example")
		if err != nil {
			t.Fatal(err)
		}
		hubCtx, cancel := context.WithCancel(ctx)
		if named {
			startStreamHub(hubCtx, store, roleDSN)
		} else {
			go NewStreamHub().Listen(hubCtx, roleDSN)
		}
		stops = append(stops, func() { cancel(); store.Close() })
		return store
	}
	until := func(store *bus.Store, want bus.ServerInstances) {
		t.Helper()
		var got bus.ServerInstances
		for i := 0; i < 100; i++ {
			// The listener's connection is up once it shows.
			var listening int
			admin.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE usename=$1 AND query LIKE 'LISTEN%'`, role).Scan(&listening)
			if got, _ = store.LiveServerInstances(ctx); got == want && listening > 0 {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("got %+v, want %+v", got, want)
	}
	first := serve(true)
	until(first, bus.ServerInstances{Live: 1, Known: true})
	serve(true)
	until(first, bus.ServerInstances{Live: 2, Known: true})
	// A listener without the name, as before the fix: unknown, never 2.
	serve(false)
	until(first, bus.ServerInstances{Live: 3})
}

// A queued write is sent only while its session's lane still takes its
// team (X6): the send runs the lane check again on the session's current
// lane, so a lane taken away while the write waited fails it as
// outside_lane, and nothing is sent.
func TestPostgresOutboxRechecksTheLane(t *testing.T) {
	f := newOutboxFixture(t)
	binding := func(rev int, unpinned string) map[string]any {
		return map[string]any{
			"instance":               awSession + "-i",
			"expected_revision":      rev,
			"mode":                   "team_board",
			"linear_team_ids":        []string{awTeam},
			"linear_default_team_id": awTeam,
			"linear_project_ids":     []string{},
			"linear_workspace_id":    awOrg,
			"lanes":                  []any{map[string]any{"name": "work", "write_team_ids": []any{awTeam}, "default_team_id": awTeam}},
			"unpinned":               unpinned,
		}
	}
	f.must(bus.Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Session: awSession, Args: binding(1, "work")})
	f.mcp.refuse = 1
	res, err := f.queueWrite("comment", map[string]any{"id": awIssue, "body": "lane", "operation_id": "l-1"})
	if err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
		t.Fatal("not queued:", res, err)
	}
	f.must(bus.Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Session: awSession, Args: binding(2, "read_only")})
	f.later(2 * time.Minute)
	q := f.queued("l-1")
	if q.State != bus.OutboxFailed || !strings.Contains(q.Reason, "outside_lane") || f.mcp.count("save_comment") != 0 {
		t.Fatal("a write outside the session's lane was sent:", q.State, q.Reason, f.mcp.count("save_comment"))
	}
}

// While the server drains, the outbox sends nothing: a queued write waits
// for the swap's new server (ZERO-LOSS-DEPLOY §2.3), and goes once the
// drain is over.
func TestPostgresOutboxWaitsOutADrain(t *testing.T) {
	f := newOutboxFixture(t)
	f.mcp.refuse = 1
	res, err := f.queueWrite("comment", map[string]any{"id": awIssue, "body": "drain", "operation_id": "d-1"})
	if err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
		t.Fatal("not queued:", res, err)
	}
	draining, _, _ := newTestDrain(on(drainStart, 10*time.Minute))
	draining.Check(context.Background())
	f.writer.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	f.writer.runOutbox(ctx, 50*time.Millisecond, draining)
	cancel()
	f.writer.Now = nil
	if n := f.mcp.count("save_comment"); n != 0 || f.queued("d-1").State == bus.OutboxDone {
		t.Fatal("a draining server sent a queued write:", n)
	}
	// The drain over, the next cycle sends it.
	f.later(2 * time.Minute)
	if n := f.mcp.count("save_comment"); n != 1 || f.queued("d-1").State != bus.OutboxDone {
		t.Fatal("the write didn't go after the drain:", n, f.queued("d-1").State)
	}
}

// A queued create the worker sends settles with the new issue's identifier
// and UUID (FR16): write-status carries them apart and the session's notice
// names the issue. A comment's result carries neither field.
func TestPostgresOutboxQueuedCreateSettlesWithItsUUID(t *testing.T) {
	f := newOutboxFixture(t)
	f.mcp.refuse = 1
	res, err := f.queueWrite("create", map[string]any{"team_id": awTeam, "title": "synthetic created issue", "operation_id": "fr16-create"})
	if err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
		t.Fatal("the create was not queued:", res, err)
	}
	f.mcp.refuse = 0
	f.later(2 * time.Minute)
	q := f.queued("fr16-create")
	if want := "issue SYN-7 (" + awCreatedIssue + ")"; q.State != bus.OutboxDone || q.Result != want {
		t.Fatal("the create did not settle with both:", q.State, q.Reason, q.Result)
	}
	if n := f.mcp.count("save_issue"); n != 1 {
		t.Fatal("save_issue sent", n, "times")
	}
	status := func(op string) map[string]any {
		res, err := f.writer.Execute(context.Background(), f.identity(), bus.Request{Operation: "linear_write_status", Workspace: f.workspace, Project: "p", Session: awSession, Args: map[string]any{"instance": awSession + "-i", "operation_id": op}})
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := awJSON(t, res.Data)["writes"].([]any)
		if len(rows) != 1 {
			t.Fatal("write-status rows:", rows)
		}
		row, _ := rows[0].(map[string]any)
		return row
	}
	if got := status("fr16-create"); got["result_id"] != awCreatedIssue || got["result_identifier"] != "SYN-7" {
		t.Fatal("write-status of a create:", got)
	}
	notices := f.linearWriteNotices()
	if len(notices) != 1 || !strings.Contains(notices[0], "(operation_id fr16-create) settled: done. issue SYN-7 ("+awCreatedIssue+").") {
		t.Fatal("the create's notice does not name the issue:", notices)
	}
	// A comment's result has no UUID pair.
	f.mcp.refuse = 1
	if _, err := f.queueWrite("comment", map[string]any{"id": awIssue, "body": queuedBody, "operation_id": "fr16-comment"}); err != nil {
		t.Fatal(err)
	}
	f.mcp.refuse = 0
	f.later(4 * time.Minute)
	if got := status("fr16-comment"); got["result_id"] != nil || got["result_identifier"] != nil {
		t.Fatal("a comment carries the create's fields:", got)
	}
}

// A queued description update whose guard came with expected_updated_at
// (FR29) is checked again when the worker sends it: an issue unchanged
// since the client's read goes, although the app reads its mention tags
// differently; one edited while it waited fails, nothing sent. A repeat
// with another expected_updated_at is the same write.
func TestPostgresOutboxQueuedDescriptionGuard(t *testing.T) {
	f := newOutboxFixture(t)
	memberText := `See <issue id="00000000-0000-0000-0000-0000000000a1" href="https://linear.example.test/x/issue/T-1">T-1</issue> first.`
	sum := sha256.Sum256([]byte(memberText))
	f.mcp.mu.Lock()
	f.mcp.texts = map[string]string{awIssue: "See [T-1](https://linear.example.test/x/issue/T-1) first."}
	f.mcp.updated = map[string]string{awIssue: "2026-10-31T09:05:12.340Z"}
	f.mcp.mu.Unlock()
	update := func(op, at string) (bus.Result, error) {
		return f.queueWrite("update", map[string]any{"id": awIssue, "operation_id": op, "description": "synthetic new text", "expected_description_sha256": hex.EncodeToString(sum[:]), "expected_updated_at": at})
	}
	f.mcp.refuse = 1
	if res, err := update("fr29-unchanged", "2026-10-31T09:05:12.34+00:00"); err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
		t.Fatal("not queued:", res, err)
	}
	f.mcp.refuse = 0
	f.later(2 * time.Minute)
	if q := f.queued("fr29-unchanged"); q.State != bus.OutboxDone || f.mcp.count("save_issue") != 1 {
		t.Fatal("an unchanged mentioned description was refused from the queue:", q.State, q.Reason)
	}
	// The issue now reads as updated at 10:00; the next update is queued
	// with that time, and someone edits the issue while it waits.
	f.mcp.refuse = 1
	if res, err := update("fr29-edited", "2026-10-31T10:00:00.000Z"); err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
		t.Fatal("not queued:", res, err)
	}
	if res, err := update("fr29-edited", "2026-10-31T10:00:00Z"); err != nil || awJSON(t, res.Data)["outcome"] != "queued" {
		t.Fatal("a repeat with another expected_updated_at was a different write:", res, err)
	}
	f.mcp.mu.Lock()
	f.mcp.texts[awIssue], f.mcp.updated[awIssue] = "A person's edit.", "2026-10-31T10:30:00.000Z"
	f.mcp.mu.Unlock()
	f.mcp.refuse = 0
	f.later(4 * time.Minute)
	q := f.queued("fr29-edited")
	if q.State != bus.OutboxFailed || !strings.Contains(q.Reason, "differs from expected_description_sha256") || f.mcp.count("save_issue") != 1 {
		t.Fatal("an edit made while the write waited was overwritten:", q.State, q.Reason, f.mcp.count("save_issue"))
	}
}
