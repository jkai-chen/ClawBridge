package bus

import (
	"context"
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
}
