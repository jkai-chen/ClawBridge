package bus

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// whRandomTeam is a fresh made-up team UUID: the reviewer's Postgres is
// shared, and webhook tables are server-wide.
func whRandomTeam() string {
	h := newID("")
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}

// The webhook store on Postgres: record, dedup, hint, the hint guard and
// purge.
func TestPostgresWebhookRecordHintAndPurge(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	team := whRandomTeam()
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM webhook_deliveries WHERE team_id=$1`, `DELETE FROM webhook_counts WHERE team_id=$1`, `DELETE FROM webhook_teams WHERE team_id=$1`} {
			f.s.db.ExecContext(context.Background(), q, team)
		}
	})
	f.must(f.owner, "sub-owner", Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{team}, "linear_default_team_id": team, "linear_project_ids": []string{cpOne}}})
	var pid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.ProjectsForTeam(ctx, team); err != nil || !reflect.DeepEqual(got, []string{pid}) {
		t.Fatal(got, err)
	}
	for _, src := range [][2]string{{"intake-s1", team}, {"intake-s2", "app"}} {
		if _, err := f.s.db.ExecContext(ctx, `INSERT INTO watch_cursors(project_id,source,subject,cursor_time,watch_started_at,next_poll_at,updated_at) VALUES($1,$2,$3,$4,$4,$5,$4)`, pid, src[0], src[1], stamp(), whFar); err != nil {
			t.Fatal(err)
		}
	}
	next := func(source, subject string) string {
		t.Helper()
		var n string
		if err := f.s.db.QueryRowContext(ctx, `SELECT next_poll_at FROM watch_cursors WHERE project_id=$1 AND source=$2 AND subject=$3`, pid, source, subject).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	now := time.Now().UTC()
	id := "sha256:" + newID("")
	d := issueUpdate(id, team, now.Add(-time.Minute))
	if r, err := f.s.WebhookRecord(ctx, d); err != nil || r.Outcome != "inert" {
		t.Fatal(r, err)
	}
	if next("intake-s1", team) != whFar {
		t.Fatal("inert marked a cursor")
	}
	if r, err := f.s.WebhookRecord(ctx, d); err != nil || r.Outcome != "duplicate" {
		t.Fatal(r, err)
	}
	f.s.SetWebhooksProven(true)
	d = issueUpdate("sha256:"+newID(""), team, now.Add(-time.Minute))
	if r, err := f.s.WebhookRecord(ctx, d); err != nil || r.Outcome != "applied" || !reflect.DeepEqual(r.Projects, []string{pid}) {
		t.Fatal(r, err)
	}
	if next("intake-s1", team) != layout(d.ReceivedAt) || next("intake-s2", "app") != layout(d.ReceivedAt) {
		t.Fatal("not marked")
	}
	// A read that started before the hint stays due.
	if err := f.s.IntakeCursorSet(ctx, pid, "intake-s1", team, layout(now.Add(-2*time.Minute)), false); err != nil {
		t.Fatal(err)
	}
	if n := next("intake-s1", team); at(n).After(time.Now().UTC()) {
		t.Fatal("the hint was buried:", n)
	}
	// Purge keeps the fresh rows; an old one goes.
	old := issueUpdate("sha256:"+newID(""), team, now.Add(-8*24*time.Hour))
	if _, err := f.s.WebhookRecord(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PurgeWebhooks(ctx, now); err != nil {
		t.Fatal(err)
	}
	var kept, gone int
	if err := f.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhook_deliveries WHERE team_id=$1`, team).Scan(&kept); err != nil || kept != 2 {
		t.Fatal("kept:", kept, err)
	}
	if err := f.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhook_deliveries WHERE delivery_id=$1`, old.DeliveryID).Scan(&gone); err != nil || gone != 0 {
		t.Fatal("old row kept:", gone, err)
	}
	// The status read works on Postgres.
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "webhook_status", Workspace: f.workspace, Project: "p"}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresWebhookRecordIsAtomic(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	team := whRandomTeam()
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM webhook_deliveries WHERE team_id=$1`, `DELETE FROM webhook_counts WHERE team_id=$1`, `DELETE FROM webhook_teams WHERE team_id=$1`} {
			f.s.db.ExecContext(context.Background(), q, team)
		}
	})
	f.must(f.owner, "sub-owner", Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{team}, "linear_default_team_id": team, "linear_project_ids": []string{cpOne}}})
	var pid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(ctx, `INSERT INTO watch_cursors(project_id,source,subject,cursor_time,watch_started_at,next_poll_at,updated_at) VALUES($1,'intake-s1',$2,$3,$3,$4,$3)`, pid, team, stamp(), whFar); err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		var n int
		if err := f.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE team_id=$1`, team).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	next := func() string {
		var n string
		if err := f.s.db.QueryRowContext(ctx, `SELECT next_poll_at FROM watch_cursors WHERE project_id=$1 AND source='intake-s1' AND subject=$2`, pid, team).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	webhookRecordIsAtomic(t, f.s, team, count, next)
}
