package bus

import (
	"context"
	"strings"
	"testing"
)

// On a hosted server an owner retiring another session gets the same
// question, and outcome done finishes that session's assignment.
func TestPostgresRetiringAnotherSessionAsksAndFinishes(t *testing.T) {
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
	view := in("assignment_propose", "w", map[string]any{"issue_uuid": watchIssue, "issue_identifier": "FIX-1"})["assignment"].(map[string]any)
	in("assignment_confirm", "coord", map[string]any{"target_session": "w", "revision": view["revision"]})
	if _, err := f.in("retire_session", "coord", map[string]any{"target_session": "w"}); err == nil || !strings.Contains(err.Error(), "session w holds the confirmed assignment for FIX-1") {
		t.Fatal("retiring another session's work went unasked:", err)
	}
	got := in("retire_session", "coord", map[string]any{"target_session": "w", "outcome": "done"})
	if got["status"] != "retired" || got["assignment"].(map[string]any)["outcome"] != "done" {
		t.Fatal(got)
	}
	var state, outcome string
	if err := f.s.db.QueryRowContext(context.Background(), `SELECT state_state,outcome FROM assignment_markers WHERE assignment_id=$1 AND phase='close'`, view["id"]).Scan(&state, &outcome); err != nil || state != "pending" || outcome != "done" {
		t.Fatal(state, outcome, err)
	}
}
