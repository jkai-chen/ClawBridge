package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The status policy and the marker's state part on Postgres: the owner
// sets the policy, a claim freezes its revision, the move is handed out
// and settled, and a release with done waits for it.
func TestPostgresStatusPolicyMovesAClaimAndADoneRelease(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.worker("coord")
	f.worker("w")
	in := func(op, session string, a map[string]any) map[string]any {
		t.Helper()
		r, err := f.in(op, session, a)
		if err != nil {
			t.Fatal(op, err)
		}
		return r.Data.(map[string]any)
	}
	in("coordinator_set", "coord", map[string]any{"target_session": "coord"})
	if _, err := f.in("status_policy_set", "w", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)}); err != nil {
		t.Fatal(err)
	}
	if got := in("status_policy_get", "w", map[string]any{}); got["revision"] != int64(1) || got["enabled"] != true {
		t.Fatal(got)
	}
	view := in("assignment_propose", "w", map[string]any{"issue_uuid": watchIssue, "issue_identifier": "FIX-1"})["assignment"].(map[string]any)
	in("assignment_confirm", "coord", map[string]any{"target_session": "w", "revision": view["revision"]})
	var id, state string
	var revision int
	if err := f.s.db.QueryRowContext(context.Background(), `SELECT m.assignment_id,m.state_state,m.state_policy_revision FROM assignment_markers m JOIN projects p ON p.id=m.project_id JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND m.phase='claim'`, f.workspace).Scan(&id, &state, &revision); err != nil || state != "pending" || revision != 1 {
		t.Fatal(id, state, revision, err)
	}
	if _, err := f.s.db.ExecContext(context.Background(), `UPDATE assignment_markers SET next_attempt_at='2000-01-01T00:00:00.000000000Z' WHERE assignment_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	g := in("marker_go", "coord", map[string]any{"assignment_id": id, "phase": "claim", "part": "state"})
	if g["action"] != "move" || g["kind"] != "claim" {
		t.Fatal(g)
	}
	in("marker_settle", "coord", map[string]any{"assignment_id": id, "phase": "claim", "part": "state", "outcome": "done", "state_target": sfProgress, "state_landed": sfProgress})
	live := in("whoami", "w", map[string]any{})["session"].(map[string]any)["assignment"].(map[string]any)
	if out := in("assignment_release", "w", map[string]any{"revision": live["revision"], "outcome": "done"}); out["outcome"] != "done" {
		t.Fatal(out)
	}
	var outcome string
	if err := f.s.db.QueryRowContext(context.Background(), `SELECT state_state,outcome FROM assignment_markers WHERE assignment_id=$1 AND phase='close'`, id).Scan(&state, &outcome); err != nil || state != "pending" || outcome != "done" {
		t.Fatal(state, outcome, err)
	}
	// A1: a done-release before the claim's move supersedes it, and the
	// close's move carries that and the claim's frozen revision.
	view = in("assignment_propose", "w", map[string]any{"issue_uuid": watchIssue, "issue_identifier": "FIX-1"})["assignment"].(map[string]any)
	in("assignment_confirm", "coord", map[string]any{"target_session": "w", "revision": view["revision"]})
	live = in("whoami", "w", map[string]any{})["session"].(map[string]any)["assignment"].(map[string]any)
	race := view["id"].(string)
	in("assignment_release", "w", map[string]any{"revision": live["revision"], "outcome": "done"})
	if _, err := f.s.db.ExecContext(context.Background(), `UPDATE assignment_markers SET next_attempt_at='2000-01-01T00:00:00.000000000Z' WHERE assignment_id=$1`, race); err != nil {
		t.Fatal(err)
	}
	if g = in("marker_go", "coord", map[string]any{"assignment_id": race, "phase": "claim", "part": "state"}); g["state"] != "superseded" {
		t.Fatal(g)
	}
	if g = in("marker_go", "coord", map[string]any{"assignment_id": race, "phase": "close", "part": "state"}); g["action"] != "move" || g["claim_state"] != "superseded" || g["claim_policy_revision"] != int64(1) || g["policy_revision"] != int64(1) {
		t.Fatal(g)
	}
}

// A skipped move on Postgres: the assignee is told once, any member reads
// the row with or without a session, a reader too, and without an active
// assignee the coordinator is told at most once in ten minutes.
func TestPostgresMarkerSkipNoticeAndStatus(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	f.worker("coord")
	in := func(op, session string, a map[string]any) map[string]any {
		t.Helper()
		r, err := f.in(op, session, a)
		if err != nil {
			t.Fatal(op, err)
		}
		return r.Data.(map[string]any)
	}
	in("coordinator_set", "coord", map[string]any{"target_session": "coord"})
	if _, err := f.in("status_policy_set", "coord", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)}); err != nil {
		t.Fatal(err)
	}
	notices := func(session string) []string {
		t.Helper()
		rows, err := f.s.db.QueryContext(ctx, `SELECT m.body FROM messages m JOIN deliveries d ON d.message_id=m.id JOIN sessions s ON s.id=d.recipient_id JOIN projects p ON p.id=s.project_id JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND s.name=$2 AND m.kind='linear-write' ORDER BY m.created_at,m.id`, f.workspace, session)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var body string
			if err = rows.Scan(&body); err != nil {
				t.Fatal(err)
			}
			out = append(out, body)
		}
		return out
	}
	skip := func(session, issue, identifier, reason string) string {
		t.Helper()
		f.worker(session)
		a := map[string]any{"issue_uuid": issue}
		if identifier != "" {
			a["issue_identifier"] = identifier
		}
		view := in("assignment_propose", session, a)["assignment"].(map[string]any)
		in("assignment_confirm", "coord", map[string]any{"target_session": session, "revision": view["revision"]})
		id := view["id"].(string)
		if _, err := f.s.db.ExecContext(ctx, `UPDATE assignment_markers SET next_attempt_at='2000-01-01T00:00:00.000000000Z' WHERE assignment_id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if g := in("marker_go", "coord", map[string]any{"assignment_id": id, "phase": "claim", "part": "state"}); g["action"] != "move" {
			t.Fatal(g)
		}
		in("marker_settle", "coord", map[string]any{"assignment_id": id, "phase": "claim", "part": "state", "outcome": "skipped", "skip_reason": reason})
		return id
	}
	id := skip("w", watchIssue, "FIX-1", "from_state")
	want := "Your claim marker on FIX-1 didn't move the issue: not moved to In Progress: someone else had moved the issue, so it was left as it is; set it yourself if right. Details: clawbridge assignment markers --issue FIX-1"
	if got := notices("w"); len(got) != 1 || got[0] != want {
		t.Fatalf("%q", got)
	}
	if _, err := f.in("marker_settle", "coord", map[string]any{"assignment_id": id, "phase": "claim", "part": "state", "outcome": "skipped", "skip_reason": "from_state"}); err == nil {
		t.Fatal("a replayed settle was accepted")
	}
	if got := notices("w"); len(got) != 1 {
		t.Fatalf("replayed: %q", got)
	}
	// The read, from a session, the owner's terminal and a reader's.
	reader := newID("r") + "@identity-test.example"
	f.role(reader, "reader")
	for _, r := range []struct {
		principal, subject, session string
		a                           map[string]any
	}{
		{f.owner, "sub-owner", "w", map[string]any{"issue_uuid": watchIssue, "instance": "w-i"}},
		{f.owner, "sub-owner", "", map[string]any{"assignment_id": id}},
		{reader, "sub-" + reader, "", map[string]any{"issue_uuid": watchIssue}},
		{reader, "sub-" + reader, "", map[string]any{"issue_identifier": "FIX-1"}},
	} {
		out, err := f.as(r.principal, r.subject, Request{Operation: "marker_status", Workspace: f.workspace, Project: "p", Session: r.session, Args: r.a})
		if err != nil {
			t.Fatal(r.principal, err)
		}
		rows := out.Data.(map[string]any)["markers"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["state_skip_reason"] != "from_state" || rows[0].(map[string]any)["summary"] != "not moved to In Progress: someone else had moved the issue, so it was left as it is; set it yourself if right" {
			t.Fatal(rows)
		}
	}
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "marker_status", Workspace: f.workspace, Project: "p", Args: map[string]any{"issue_uuid": watchIssue, "assignment_id": id}}); err == nil {
		t.Fatal("both were accepted")
	}
	// Without an active assignee, the coordinator, rate limited.
	retire := func(session string) {
		t.Helper()
		if _, err := f.s.db.ExecContext(ctx, `UPDATE sessions SET status='retired' WHERE name=$1 AND project_id=(SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$2 AND p.slug='p')`, session, f.workspace); err != nil {
			t.Fatal(err)
		}
	}
	f.worker("x")
	f.worker("y")
	for _, session := range []string{"x", "y"} {
		view := in("assignment_propose", session, map[string]any{"issue_uuid": map[string]string{"x": sfIssueNext, "y": sfIssueOther}[session]})["assignment"].(map[string]any)
		in("assignment_confirm", "coord", map[string]any{"target_session": session, "revision": view["revision"]})
		if _, err := f.s.db.ExecContext(ctx, `UPDATE assignment_markers SET next_attempt_at='2000-01-01T00:00:00.000000000Z' WHERE assignment_id=$1`, view["id"]); err != nil {
			t.Fatal(err)
		}
		retire(session)
		in("marker_go", "coord", map[string]any{"assignment_id": view["id"], "phase": "claim", "part": "state"})
		in("marker_settle", "coord", map[string]any{"assignment_id": view["id"], "phase": "claim", "part": "state", "outcome": "skipped", "skip_reason": "policy_changed"})
	}
	if got := notices("coord"); len(got) != 1 || !strings.HasPrefix(got[0], "A claim marker on "+sfIssueNext+" didn't") || !strings.Contains(got[0], "on "+sfIssueNext+" didn't move the issue: not moved to the policy's state: the status policy changed after the move was planned. Details: clawbridge assignment markers --issue "+sfIssueNext) {
		t.Fatalf("%q", got)
	}
	var audited int
	if err := f.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM history h JOIN workspaces w ON w.id=h.workspace_id WHERE w.name=$1 AND h.entity='assignment_marker' AND h.action='state_skipped'`, f.workspace).Scan(&audited); err != nil || audited != 3 {
		t.Fatal(audited, err)
	}
	// Doctor counts the three, and names the newest.
	r, err := f.as(f.owner, "sub-owner", Request{Operation: "comment_watch_status", Workspace: f.workspace, Project: "p", Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	st := r.Data.(map[string]any)
	if st["markers_state_skipped"] != 3 || st["markers_state_skipped_newest"].(map[string]any)["issue_uuid"] != sfIssueOther {
		t.Fatal(st)
	}
}

// A marker's reread acknowledges its earlier attempt's record in the
// shared ledger on Postgres, and the late outcome of the server that sent
// it still lands after: applied wins.
func TestPostgresMarkerRereadAcksTheEarlierAttempt(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	f.worker("coord")
	f.worker("w")
	in := func(op, session string, a map[string]any) map[string]any {
		t.Helper()
		r, err := f.in(op, session, a)
		if err != nil {
			t.Fatal(op, err)
		}
		return r.Data.(map[string]any)
	}
	in("coordinator_set", "coord", map[string]any{"target_session": "coord"})
	if _, err := f.in("status_policy_set", "w", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)}); err != nil {
		t.Fatal(err)
	}
	view := in("assignment_propose", "w", map[string]any{"issue_uuid": watchIssue, "issue_identifier": "FIX-1"})["assignment"].(map[string]any)
	in("assignment_confirm", "coord", map[string]any{"target_session": "w", "revision": view["revision"]})
	id := view["id"].(string)
	var pid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT project_id FROM assignment_markers WHERE assignment_id=$1`, id).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	due := func() {
		t.Helper()
		if _, err := f.s.db.ExecContext(ctx, `UPDATE assignment_markers SET next_attempt_at='2000-01-01T00:00:00.000000000Z' WHERE assignment_id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if g := in("marker_go", "coord", map[string]any{"assignment_id": id, "phase": "claim", "part": "state"}); g["action"] != "move" {
			t.Fatal(g)
		}
	}
	// The fixture's database outlives a run: each run has its own row.
	sum := sha256.Sum256([]byte(newID("run")))
	ns := hex.EncodeToString(sum[:])
	op := "marker-" + ns[:16]
	record := `{"operation_id":"` + op + `","operation":"update","project_id":"` + pid + `","version":1,"digest":"` + strings.Repeat("a", 64) + `","state":"pending","reason":"timeout","updated_at":"2026-10-29T06:00:00Z","issue_id":"` + watchIssue + `","marker_part":"state"}`
	if _, created, err := f.s.LedgerInsert(ctx, ns, op, record, pid, ""); err != nil || !created {
		t.Fatal(created, err)
	}
	due()
	in("marker_settle", "coord", map[string]any{"assignment_id": id, "phase": "claim", "part": "state", "outcome": "uncertain", "operation_id": op})
	due()
	if r := in("marker_settle", "coord", map[string]any{"assignment_id": id, "phase": "claim", "part": "state", "outcome": "done", "state_target": sfProgress, "state_landed": sfProgress}); r["operations_acknowledged"] != 1 {
		t.Fatal(r)
	}
	var by, at string
	var pending int
	if err := f.s.db.QueryRowContext(ctx, `SELECT acknowledged_by,acknowledged_at,pending FROM linear_write_ledger WHERE namespace=$1 AND operation_id=$2`, ns, op).Scan(&by, &at, &pending); err != nil || by != "reread" || at == "" || pending != 1 {
		t.Fatal(by, at, pending, err)
	}
	applied := strings.Replace(strings.Replace(record, `"state":"pending"`, `"state":"accepted"`, 1), `"reason":"timeout",`, "", 1)
	if err := f.s.LedgerReplace(ctx, ns, op, record, applied); err != nil {
		t.Fatal("the late outcome was refused:", err)
	}
	if got, _, err := f.s.LedgerGet(ctx, ns, op); err != nil || got != applied {
		t.Fatal(got, err)
	}
	var audits int
	if err := f.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM history WHERE entity='linear_write_ledger' AND entity_key=$1 AND action='acknowledged'`, op).Scan(&audits); err != nil || audits != 1 {
		t.Fatal(audits, err)
	}
}
