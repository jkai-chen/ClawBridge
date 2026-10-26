package bus

import (
	"context"
	"testing"
)

// WatchProjectID on Postgres: a bound project resolves by its names; an
// unbound one doesn't.
func TestPostgresWatchProjectID(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.worker("coord")
	var pid string
	if err := f.s.db.QueryRowContext(context.Background(), `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.WatchProjectID(context.Background(), f.workspace, "p"); err != nil || got != "" {
		t.Fatal("an unbound project resolved", got, err)
	}
	if _, err := f.in("coordinator_set", "coord", map[string]any{"target_session": "coord"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.in("taskboard_set_v2", "coord", taskboardArgsV2(0, projectOne)); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.WatchProjectID(context.Background(), f.workspace, "p"); err != nil || got != pid {
		t.Fatal(got, pid, err)
	}
}
