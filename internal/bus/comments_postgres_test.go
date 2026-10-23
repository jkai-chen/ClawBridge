package bus

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func (f *pgIdentityFixture) worker(name string) {
	f.t.Helper()
	f.must(f.owner, "sub-owner", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: name, Args: map[string]any{"instance": name + "-i", "role": "worker"}})
}

func (f *pgIdentityFixture) in(op, session string, a map[string]any) (Result, error) {
	a["instance"] = session + "-i"
	return f.as(f.owner, "sub-owner", Request{Operation: op, Workspace: f.workspace, Project: "p", Session: session, Args: a})
}

// Two sessions reserving the issue's last slot at once: the workspace lock
// lets exactly one through.
func TestPostgresCommentCapHoldsUnderConcurrentReserves(t *testing.T) {
	f := newPGIdentityFixture(t)
	for _, name := range []string{"x", "y"} {
		f.worker(name)
	}
	reserve := func(session, op string) error {
		_, err := f.in("linear_comment_reserve", session, map[string]any{"issue_uuid": watchIssue, "operation_id": op, "body_sha256": bodyDigest})
		return err
	}
	for _, op := range []string{"one", "two"} {
		if err := reserve("x", op); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, name := range []string{"x", "y"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = reserve(name, "race-"+name)
		}()
	}
	wg.Wait()
	won, capped := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case codeIs(err, "rate_limited", "next slot at"):
			capped++
		default:
			t.Fatal(err)
		}
	}
	if won != 1 || capped != 1 {
		t.Fatalf("concurrent reserves at the cap: %d won, %d capped", won, capped)
	}
}

// The whole watch loop on Postgres: confirm starts the watch, the lease
// holder's report notifies the assignee, and the answer is tied to it.
func TestPostgresCommentWatchNotifiesAndTiesTheAnswer(t *testing.T) {
	f := newPGIdentityFixture(t)
	for _, name := range []string{"coord", "worker"} {
		f.worker(name)
	}
	must := func(op, session string, a map[string]any) map[string]any {
		t.Helper()
		r, err := f.in(op, session, a)
		if err != nil {
			t.Fatal(op, err)
		}
		return r.Data.(map[string]any)
	}
	must("coordinator_set", "coord", map[string]any{"target_session": "coord"})
	proposed := must("assignment_propose", "worker", map[string]any{"issue_uuid": watchIssue, "issue_identifier": "FIX-1"})
	must("assignment_confirm", "coord", map[string]any{"target_session": "worker", "revision": proposed["assignment"].(map[string]any)["revision"]})
	lease := must("comment_watch_lease", "coord", map[string]any{"action": "acquire"})
	fence := func(a map[string]any) map[string]any {
		a["lease_token"], a["generation"] = lease["lease_token"], lease["generation"]
		return a
	}
	due := must("comment_watch_due", "coord", fence(map[string]any{}))
	if len(due["issues"].([]any)) != 1 {
		t.Fatal("confirmed issue not due:", due)
	}
	report := must("comment_watch_report", "coord", fence(map[string]any{"issue_uuid": watchIssue, "comments": []any{comment(humanOne, "", time.Now())}}))
	if got := report["notified"].([]string); len(got) != 1 {
		t.Fatal(report)
	}
	inbox := must("inbox", "worker", map[string]any{})["messages"].([]map[string]any)
	if len(inbox) != 1 || inbox[0]["kind"] != "linear-comment" {
		t.Fatal(inbox)
	}
	answer := must("linear_comment_reserve", "worker", map[string]any{"issue_uuid": watchIssue, "operation_id": "answer", "body_sha256": bodyDigest, "parent_id": humanOne})
	if answer["in_response_to"] != humanOne {
		t.Fatal(answer)
	}
	if _, err := f.in("linear_comment_reserve", "worker", map[string]any{"issue_uuid": watchIssue, "operation_id": "again", "body_sha256": bodyDigest, "parent_id": humanOne, "in_response_to": humanOne}); !codeIs(err, "conflict", "already has an answer") {
		t.Fatal("second answer accepted:", err)
	}
	flags := must("linear_comments_posted", "coord", map[string]any{"issue_uuid": watchIssue, "comment_ids": []any{humanOne}})
	if flags["young_pending"] != true {
		t.Fatal("unsettled answer not young pending:", flags)
	}
}

// A reader may ask which comments ClawBridge posted (ownership flags, no
// bodies), so it never answers one of ours; it still writes nothing.
func TestPostgresReaderMayQueryPostedCommentsOnly(t *testing.T) {
	f := newPGIdentityFixture(t)
	reader := newID("r") + "@identity-test.example"
	f.member(reader)
	f.must(reader, "sub-reader", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "reader", Args: map[string]any{"instance": "reader-i"}})
	f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": reader, "role": "reader"}})
	as := func(op string, a map[string]any) (Result, error) {
		a["instance"] = "reader-i"
		return f.as(reader, "sub-reader", Request{Operation: op, Workspace: f.workspace, Project: "p", Session: "reader", Args: a})
	}
	out, err := as("linear_comments_posted", map[string]any{"issue_uuid": watchIssue, "comment_ids": []any{humanOne}})
	if err != nil {
		t.Fatal("reader cannot read ownership flags:", err)
	}
	if flags := out.Data.(map[string]any); flags["young_pending"] != false {
		t.Fatal(flags)
	}
	if _, err = as("linear_comment_reserve", map[string]any{"issue_uuid": watchIssue, "operation_id": "r", "body_sha256": bodyDigest}); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatal("reader reserved a comment:", err)
	}
}

// Retention drops comment-watch state past max(retention_days, 30) days and
// keeps posted reservations, which tell our comments from a human's.
func TestPostgresPurgeDropsExpiredCommentWatchState(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	var pid, wid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT p.id,p.workspace_id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1`, f.workspace).Scan(&pid, &wid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(ctx, `UPDATE workspaces SET retention_days=1 WHERE id=$1`, wid); err != nil {
		t.Fatal(err)
	}
	day := func(n int) string { return layout(time.Now().AddDate(0, 0, n)) }
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO watch_events(project_id,event_key,source,subject,reply_to,session_id,message_id,created_at) VALUES($1,$2,'comments',$3,$4,NULL,'',$5)`, []any{pid, "comment:" + humanOne, watchIssue, humanOne, day(-31)}},
		{`INSERT INTO watch_events(project_id,event_key,source,subject,reply_to,session_id,message_id,created_at) VALUES($1,$2,'comments',$3,$4,NULL,'',$5)`, []any{pid, "comment:" + humanTwo, watchIssue, humanTwo, day(-2)}},
		{`INSERT INTO watch_cursors(project_id,source,subject,cursor_time,watch_started_at,next_poll_at,active_until,updated_at) VALUES($1,'comments',$2,$3,$3,$3,$4,$3)`, []any{pid, watchIssue, day(-3), day(-2)}},
		{`INSERT INTO watch_cursors(project_id,source,subject,cursor_time,watch_started_at,next_poll_at,active_until,updated_at) VALUES($1,'comments',$2,$3,$3,$3,$4,$3)`, []any{pid, otherIssue, day(-1), day(1)}},
		{`INSERT INTO watch_cursors(project_id,source,subject,cursor_time,watch_started_at,next_poll_at,updated_at) VALUES($1,'comments',$2,$3,$3,$3,$3)`, []any{pid, "00000000-0000-4000-8000-0000000000a3", day(-90)}},
		{`INSERT INTO linear_comment_ledger(workspace_id,project_id,operation_digest,issue_uuid,body_sha256,session_id,principal,state,reserved_at,settled_at) VALUES($1,$2,$3,$4,$5,'s','p','failed',$6,$6)`, []any{wid, pid, digestOf("old-failed"), watchIssue, bodyDigest, day(-40)}},
		{`INSERT INTO linear_comment_ledger(workspace_id,project_id,operation_digest,issue_uuid,comment_id,body_sha256,session_id,principal,state,reserved_at,settled_at) VALUES($1,$2,$3,$4,$5,$6,'s','p','posted',$7,$7)`, []any{wid, pid, digestOf("old-posted"), watchIssue, ourComment, bodyDigest, day(-40)}},
	} {
		if _, err := f.s.db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	out := f.must(f.owner, "sub-owner", Request{Operation: "purge_expired", Workspace: f.workspace, Project: "p", Args: map[string]any{}}).Data.(map[string]any)
	for k, want := range map[string]int64{"deleted_watch_events": 1, "deleted_watch_cursors": 1, "deleted_failed_reservations": 1} {
		if out[k] != want {
			t.Fatalf("%s = %v, want %d: %v", k, out[k], want, out)
		}
	}
	var events, cursors, posted int
	if err := f.s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM watch_events WHERE project_id=$1),(SELECT COUNT(*) FROM watch_cursors WHERE project_id=$1),(SELECT COUNT(*) FROM linear_comment_ledger WHERE project_id=$1 AND state='posted')`, pid).Scan(&events, &cursors, &posted); err != nil || events != 1 || cursors != 2 || posted != 1 {
		t.Fatal("purge removed live state:", events, cursors, posted, err)
	}
}

// A membership revoke releases the member's assignment on the server, with
// no client of the member involved: the close marker is still written.
func TestPostgresAccessRevokedWritesTheCloseMarker(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.worker("coord")
	member := newID("m") + "@identity-test.example"
	f.member(member)
	f.must(member, "sub-member", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "member", Args: map[string]any{"instance": "member-i"}})
	in := func(op string, a map[string]any) map[string]any {
		t.Helper()
		r, err := f.in(op, "coord", a)
		if err != nil {
			t.Fatal(op, err)
		}
		return r.Data.(map[string]any)
	}
	in("coordinator_set", map[string]any{"target_session": "coord"})
	proposed, err := f.as(member, "sub-member", Request{Operation: "assignment_propose", Workspace: f.workspace, Project: "p", Session: "member", Args: map[string]any{"instance": "member-i", "issue_uuid": watchIssue, "issue_identifier": "FIX-1"}})
	if err != nil {
		t.Fatal(err)
	}
	view := proposed.Data.(map[string]any)["assignment"].(map[string]any)
	in("assignment_confirm", map[string]any{"target_session": "member", "revision": view["revision"]})
	g := in("marker_go", map[string]any{"assignment_id": view["id"], "phase": "claim", "part": "label"})
	if g["action"] != "add" {
		t.Fatal(g)
	}
	in("marker_settle", map[string]any{"assignment_id": view["id"], "phase": "claim", "part": "label", "outcome": "done"})
	f.must(f.owner, "sub-owner", Request{Operation: "project_role_revoke", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": member}})
	var reason, comment, label string
	if err = f.s.db.QueryRowContext(context.Background(), `SELECT reason,comment_state,label_state FROM assignment_markers WHERE assignment_id=$1 AND phase='close'`, view["id"]).Scan(&reason, &comment, &label); err != nil || reason != "access_revoked" || comment != "waiting_on_claim" || label != "pending" {
		t.Fatal("no close marker for a revoked member:", reason, comment, label, err)
	}
	if g = in("marker_go", map[string]any{"assignment_id": view["id"], "phase": "close", "part": "label"}); g["action"] != "remove" {
		t.Fatal("the coordinator cannot apply the revoked member's close:", g)
	}
}
