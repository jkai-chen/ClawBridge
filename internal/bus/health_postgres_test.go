package bus

import (
	"context"
	"testing"
)

// Project health on Postgres: only the owner sets it, any member reads it,
// and two runs with the same health tell the coordinator.
func TestPostgresProjectHealthRunsAndNotifies(t *testing.T) {
	f := newPGIdentityFixture(t)
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
	in("taskboard_set_v2", "coord", taskboardArgsV2(0, projectOne))
	member := newID("member") + "@identity-test.example"
	f.member(member)
	if _, err := f.as(member, "sub-member", Request{Operation: "health_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"linear_project": projectOne, "expected_revision": 0, "health_policy": map[string]any{"enabled": true}}}); err == nil {
		t.Fatal("a member set project health")
	}
	in("health_set", "coord", map[string]any{"linear_project": projectOne, "expected_revision": 0, "health_policy": map[string]any{"enabled": true}})
	if got, err := f.as(member, "sub-member", Request{Operation: "health_get", Workspace: f.workspace, Project: "p", Args: map[string]any{}}); err != nil || len(got.Data.(map[string]any)["projects"].([]any)) != 1 {
		t.Fatal("a member cannot read project health:", got, err)
	}
	ctx := context.Background()
	var pid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	sys := func(op string, a map[string]any) map[string]any {
		t.Helper()
		r, err := f.s.ExecuteSystem(ctx, pid, Request{Operation: op, Args: a})
		if err != nil {
			t.Fatal(op, err)
		}
		return r.Data.(map[string]any)
	}
	l := sys("comment_watch_lease", map[string]any{"action": "acquire"})
	fence := func(a map[string]any) map[string]any {
		a["lease_token"], a["generation"] = l["lease_token"], l["generation"]
		return a
	}
	for run := 0; run < 2; run++ {
		if _, err := f.s.db.ExecContext(ctx, `UPDATE project_health SET next_health_at='2000-01-01T00:00:00.000000000Z' WHERE project_id=$1`, pid); err != nil {
			t.Fatal(err)
		}
		if runs := sys("health_due", fence(map[string]any{"limit": 2}))["runs"].([]any); len(runs) != 1 {
			t.Fatal(runs)
		}
		sys("health_report", fence(map[string]any{"linear_project": projectOne, "calls": 3, "facts": blockedFacts(2)}))
	}
	var n int
	if err := f.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE project_id=$1 AND kind='project-health'`, pid).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
