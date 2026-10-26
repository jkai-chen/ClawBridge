package bus

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

// A hosted heartbeat writes its lease after the read commits, on the beat's
// light path: an allow-list narrowed, the session retired or moved, or a
// membership revoked while the read was in flight skips it. It never moves
// last_seen.
func TestPostgresHeartbeatRechecksAccessBeforeItLeases(t *testing.T) {
	if os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN") == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	t.Cleanup(func() { beforeBeat.Store(nil) })
	for name, change := range map[string]string{
		"none":               "",
		"domain narrowed":    `UPDATE workspaces SET allowed_domains='["other.example"]' WHERE name=$1`,
		"session retired":    `UPDATE sessions SET status='retired' WHERE project_id=(SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1) AND name='hb'`,
		"session moved":      `UPDATE sessions SET instance='another-instance' WHERE project_id=(SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1) AND name='hb'`,
		"membership revoked": `DELETE FROM members WHERE workspace_id=(SELECT id FROM workspaces WHERE name=$1) AND principal<>(SELECT owner_principal FROM workspaces WHERE name=$1)`,
	} {
		t.Run(name, func(t *testing.T) {
			g := newPGIdentityFixture(t)
			member := newID("m") + "@corp.example"
			g.member(member)
			instance := g.session(member, "corp.example", "hb")
			read := func() (string, sql.NullString) {
				var seen string
				var until sql.NullString
				if err := g.s.db.QueryRowContext(ctx, `SELECT last_seen,alive_until FROM sessions WHERE owner_principal=$1`, member).Scan(&seen, &until); err != nil {
					t.Fatal(err)
				}
				return seen, until
			}
			seen, _ := read()
			hook := func() {
				if change != "" {
					if _, err := g.s.db.ExecContext(ctx, change, g.workspace); err != nil {
						t.Error(err)
					}
				}
			}
			beforeBeat.Store(&hook)
			out, err := g.asHD(member, "corp.example", Request{Operation: "heartbeat", Workspace: g.workspace, Project: "p", Session: "hb", Args: map[string]any{"instance": instance, "state": "working"}})
			beforeBeat.Store(nil)
			if err != nil {
				t.Fatal("the read itself was admitted and must succeed", err)
			}
			after, until := read()
			if until.Valid != (change == "") || out.Data.(map[string]any)["renewed"] != (change == "") {
				t.Fatalf("lease written=%v (renewed %v) after %s", until.Valid, out.Data, name)
			}
			if after != seen {
				t.Fatal("a heartbeat moved last_seen")
			}
		})
	}
}

// A hosted heartbeat stays out of the lock queue: with the workspace lock
// held by a writer it returns at once, unrenewed; once free, it renews, and
// not again within 20 s.
func TestPostgresHeartbeatSkipsABusyWorkspace(t *testing.T) {
	if os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN") == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	g := newPGIdentityFixture(t)
	member := newID("m") + "@corp.example"
	g.member(member)
	instance := g.session(member, "corp.example", "hb")
	var wid string
	if err := g.s.db.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE name=$1`, g.workspace).Scan(&wid); err != nil {
		t.Fatal(err)
	}
	heartbeat := func() any {
		t.Helper()
		out, err := g.asHD(member, "corp.example", Request{Operation: "heartbeat", Workspace: g.workspace, Project: "p", Session: "hb", Args: map[string]any{"instance": instance}})
		if err != nil {
			t.Fatal(err)
		}
		return out.Data.(map[string]any)["renewed"]
	}
	writer, err := g.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "workspace:"+wid); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	renewed := heartbeat()
	if took := time.Since(start); renewed != false || took > 2*time.Second {
		t.Fatalf("with the lock held: renewed %v after %s", renewed, took)
	}
	if err = writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	if renewed := heartbeat(); renewed != true {
		t.Fatal("not renewed with the lock free")
	}
	if renewed := heartbeat(); renewed != false {
		t.Fatal("renewed twice within 20 s")
	}
}
