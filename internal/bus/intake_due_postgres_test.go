package bus

import (
	"context"
	"testing"
)

// IntakeDueAt on Postgres answers as on SQLite.
func TestPostgresIntakeDueAt(t *testing.T) {
	f := newPGIdentityFixture(t)
	var pid string
	if err := f.s.db.QueryRowContext(context.Background(), `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	checkIntakeDueAt(t, f.s, pid)
}
