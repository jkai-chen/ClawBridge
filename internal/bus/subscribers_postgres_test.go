package bus

import (
	"context"
	"strings"
	"testing"
)

const (
	subscriberOne = "00000000-0000-4000-8000-0000000000d1"
	subscriberTwo = "00000000-0000-4000-8000-0000000000d2"
	subscribeIss  = "00000000-0000-4000-8000-0000000000e1"
	subscribeIss2 = "00000000-0000-4000-8000-0000000000e2"
	subscribeIss3 = "00000000-0000-4000-8000-0000000000e3"
)

// subscriberFixture is a linked project with a coordinator and a worker,
// and the system session's lease for the watcher's ops.
type subscriberFixture struct {
	*pgIdentityFixture
	pid   string
	lease map[string]any
}

func newSubscriberFixture(t *testing.T) *subscriberFixture {
	t.Helper()
	f := &subscriberFixture{pgIdentityFixture: newPGIdentityFixture(t)}
	for _, name := range []string{"coord", "worker"} {
		f.worker(name)
	}
	if _, err := f.in("coordinator_set", "coord", map[string]any{"target_session": "coord"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.in("taskboard_set_v2", "coord", taskboardArgsV2(0, projectOne)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRowContext(context.Background(), `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, f.workspace).Scan(&f.pid); err != nil {
		t.Fatal(err)
	}
	f.lease = f.sys("comment_watch_lease", map[string]any{"action": "acquire"})
	return f
}

func (f *subscriberFixture) sys(op string, a map[string]any) map[string]any {
	f.t.Helper()
	r, err := f.s.ExecuteSystem(context.Background(), f.pid, Request{Operation: op, Args: a})
	if err != nil {
		f.t.Fatal(op, err)
	}
	return r.Data.(map[string]any)
}

func (f *subscriberFixture) fenced(a map[string]any) map[string]any {
	a["lease_token"], a["generation"] = f.lease["lease_token"], f.lease["generation"]
	return a
}

func (f *subscriberFixture) setting(principal, subject string) map[string]any {
	f.t.Helper()
	return f.must(principal, subject, Request{Operation: "issue_subscribers_get", Workspace: f.workspace, Project: "p", Args: map[string]any{}}).Data.(map[string]any)
}

func (f *subscriberFixture) set(a map[string]any) (Result, error) {
	return f.as(f.owner, "sub-owner", Request{Operation: "issue_subscribers_set", Workspace: f.workspace, Project: "p", Args: a})
}

// The setting: on by default for a project linked after schema 41, but
// nothing runs, and nothing turns on, before the server is proven (B2).
// Only the owner or a lead sets it, never a session, at the revision read.
func TestPostgresIssueSubscribersSetting(t *testing.T) {
	f := newSubscriberFixture(t)
	got := f.setting(f.owner, "sub-owner")
	if got["default"] != true || got["enabled"] != true || got["on"] != false || got["proven"] != false {
		t.Fatalf("%+v", got)
	}
	if _, err := f.set(map[string]any{"expected_revision": 3, "owner": false}); err == nil || !strings.Contains(err.Error(), "current revision is 0") {
		t.Fatal("a default setting took a stale revision:", err)
	}
	if _, err := f.set(map[string]any{"expected_revision": 0, "enabled": true}); err == nil || !strings.Contains(err.Error(), "not yet proven") {
		t.Fatal("turned on unproven:", err)
	}
	if q := f.sys("subscribe_queue", map[string]any{"issue_uuid": subscribeIss, "reason": "create"}); q["queued"] != false {
		t.Fatal("queued unproven:", q)
	}
	f.s.SetIssueSubscribersProven(true)
	if got = f.setting(f.owner, "sub-owner"); got["on"] != true {
		t.Fatalf("%+v", got)
	}
	member := newID("m") + "@identity-test.example"
	f.member(member)
	if _, err := f.as(member, "sub-member", Request{Operation: "issue_subscribers_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": 0, "enabled": false}}); err == nil {
		t.Fatal("a member set it")
	}
	if _, err := f.in("issue_subscribers_set", "coord", map[string]any{"expected_revision": 0, "enabled": false}); err == nil {
		t.Fatal("a session set it")
	}
	if got = f.setting(member, "sub-member"); got["owner"] != true {
		t.Fatal("a member can't read it:", got)
	}
	out, err := f.set(map[string]any{"expected_revision": 0, "user_ids": []any{strings.ToUpper(subscriberOne), subscriberTwo}})
	if err != nil {
		t.Fatal(err)
	}
	if d := out.Data.(map[string]any); d["revision"] != 1 || len(d["user_ids"].([]string)) != 2 || d["user_ids"].([]string)[0] != subscriberOne || d["default"] != false {
		t.Fatalf("%+v", d)
	}
	if _, err = f.set(map[string]any{"expected_revision": 0, "enabled": false}); err == nil {
		t.Fatal("a stale revision")
	}
	if _, err = f.set(map[string]any{"expected_revision": 1, "user_ids": []any{subscriberOne, subscriberOne}}); err == nil {
		t.Fatal("a duplicate ID")
	}
	six := []any{}
	for i := 0; i < MaxIssueSubscribers+1; i++ {
		six = append(six, strings.Replace(subscriberOne, "d1", "f"+string(rune('0'+i)), 1))
	}
	if _, err = f.set(map[string]any{"expected_revision": 1, "user_ids": six}); err == nil {
		t.Fatal("more than the cap")
	}
	// B4: history names the people by Linear ID, never an email.
	var details string
	if err = f.s.db.QueryRowContext(context.Background(), `SELECT details FROM history WHERE project_id=$1 AND entity='issue_subscribers' ORDER BY sequence DESC LIMIT 1`, f.pid).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(details, "@") || !strings.Contains(details, subscriberOne) {
		t.Fatal(details)
	}
}

// A claim and a create queue one row per issue; the watcher is handed it
// with the owner's email and the people, charged up front; a done row is
// never queued again (B3); off drops what is queued.
func TestPostgresIssueSubscribersQueueAndPass(t *testing.T) {
	f := newSubscriberFixture(t)
	f.s.SetIssueSubscribersProven(true)
	proposed, err := f.in("assignment_propose", "worker", map[string]any{"issue_uuid": subscribeIss, "issue_identifier": "FIX-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.in("assignment_confirm", "coord", map[string]any{"target_session": "worker", "revision": proposed.Data.(map[string]any)["assignment"].(map[string]any)["revision"]}); err != nil {
		t.Fatal(err)
	}
	if q := f.sys("subscribe_queue", map[string]any{"issue_uuid": subscribeIss, "reason": "create"}); q["queued"] != false {
		t.Fatal("a claimed issue queued twice:", q)
	}
	// The backfill's claims (§10): confirmed since a time, by identifier.
	claims := f.sys("subscribe_claims", map[string]any{"since": "2000-01-01T00:00:00.000000000Z"})["claims"].([]map[string]any)
	if len(claims) != 1 || claims[0]["issue_uuid"] != subscribeIss || claims[0]["issue_identifier"] != "FIX-1" {
		t.Fatal(claims)
	}
	if later := f.sys("subscribe_claims", map[string]any{"since": "2999-01-01T00:00:00.000000000Z"})["claims"].([]map[string]any); len(later) != 0 {
		t.Fatal(later)
	}
	due := f.sys("subscribe_due", f.fenced(map[string]any{}))
	rows := due["rows"].([]any)
	if len(rows) != 1 {
		t.Fatal(due)
	}
	row := rows[0].(map[string]any)
	if row["issue_uuid"] != subscribeIss || row["owner_email"] != f.owner || row["charged"] != SubscribeRowCalls(1) {
		t.Fatalf("%+v", row)
	}
	if again := f.sys("subscribe_due", f.fenced(map[string]any{})); len(again["rows"].([]any)) != 1 {
		t.Fatal("a row stays queued until it settles:", again)
	}
	settled := f.sys("subscribe_settle", f.fenced(map[string]any{"issue_uuid": subscribeIss, "outcome": "done", "reason": "", "calls": 2, "charged": row["charged"]}))
	if settled["state"] != "done" {
		t.Fatal(settled)
	}
	if q := f.sys("subscribe_queue", map[string]any{"issue_uuid": subscribeIss, "reason": "claim"}); q["queued"] != false {
		t.Fatal("a done issue queued again")
	}
	if got := f.setting(f.owner, "sub-owner")["rows"].(map[string]int); got["done"] != 1 {
		t.Fatal(got)
	}
	// A retry waits; Linear refusing the owner's email holds owner-only rows.
	f.sys("subscribe_queue", map[string]any{"issue_uuid": subscribeIss2, "reason": "create"})
	row = f.sys("subscribe_due", f.fenced(map[string]any{}))["rows"].([]any)[0].(map[string]any)
	f.sys("subscribe_settle", f.fenced(map[string]any{"issue_uuid": subscribeIss2, "outcome": "retry", "reason": "owner_not_in_linear", "owner_missing": true, "calls": 2, "charged": row["charged"]}))
	if _, err = f.s.db.ExecContext(context.Background(), `UPDATE issue_subscriptions SET due_at='2000-01-01T00:00:00.000000000Z' WHERE project_id=$1`, f.pid); err != nil {
		t.Fatal(err)
	}
	if held := f.sys("subscribe_due", f.fenced(map[string]any{})); held["stopped"] != "owner_not_in_linear" || len(held["rows"].([]any)) != 0 {
		t.Fatal("an owner-only row went out with the owner refused:", held)
	}
	if got := f.setting(f.owner, "sub-owner"); got["owner_not_in_linear_until"] == nil || strings.Contains(strings.Join(mapKeys(got), ","), "email") {
		t.Fatalf("%+v", got)
	}
	// A row tried subscribeAttempts times fails.
	f.sys("subscribe_queue", map[string]any{"issue_uuid": subscribeIss3, "reason": "create"})
	for i := 0; i < subscribeAttempts; i++ {
		got := f.sys("subscribe_settle", f.fenced(map[string]any{"issue_uuid": subscribeIss3, "outcome": "retry", "reason": "error", "calls": 1, "charged": 2}))
		if want := map[bool]string{true: "failed", false: "queued"}[i == subscribeAttempts-1]; got["state"] != want {
			t.Fatal(i, got)
		}
	}
	// With an extra person the row goes out without the owner.
	if _, err = f.set(map[string]any{"expected_revision": 0, "user_ids": []any{subscriberOne}}); err != nil {
		t.Fatal(err)
	}
	row = f.sys("subscribe_due", f.fenced(map[string]any{}))["rows"].([]any)[0].(map[string]any)
	if row["owner_email"] != nil || row["charged"] != SubscribeRowCalls(1) {
		t.Fatalf("%+v", row)
	}
	// The hour's budget bounds the pass.
	if _, err = f.s.db.ExecContext(context.Background(), `UPDATE watch_lease SET budget_limit=1 WHERE project_id=$1`, f.pid); err != nil {
		t.Fatal(err)
	}
	if over := f.sys("subscribe_due", f.fenced(map[string]any{})); over["stopped"] != "budget" {
		t.Fatal("over budget:", over)
	}
	// Off drops the queue.
	if _, err = f.set(map[string]any{"expected_revision": 1, "enabled": false}); err != nil {
		t.Fatal(err)
	}
	var state, outcome string
	if err = f.s.db.QueryRowContext(context.Background(), `SELECT state,outcome FROM issue_subscriptions WHERE project_id=$1 AND issue_uuid=$2`, f.pid, subscribeIss2).Scan(&state, &outcome); err != nil || state != "skipped" || outcome != "off" {
		t.Fatal(state, outcome, err)
	}
	if off := f.sys("subscribe_due", f.fenced(map[string]any{})); off["stopped"] != "off" {
		t.Fatal(off)
	}
	// None of it is a client's: the ops answer only through ExecuteSystem.
	if _, err = f.in("subscribe_queue", "coord", map[string]any{"issue_uuid": subscribeIss, "reason": "create"}); err == nil {
		t.Fatal("a session queued a subscription")
	}
}

func mapKeys(m map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
