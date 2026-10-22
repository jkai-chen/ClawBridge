package bus

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// History is append-only and retired sessions are never deleted, so a long-lived
// project must still produce a snapshot: the newest events and the most recent
// retired sessions, every active session, and flags saying what was left out.
func TestPostgresSnapshotBoundsHistoryAndRetiredSessions(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner := "owner@snapshot-test.example"
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("unit_")
	call := func(op, session string, a map[string]any) map[string]any {
		t.Helper()
		if session != "" {
			a["instance"] = session + "-inst"
		}
		r, e := s.ExecuteAs(ctx, owner, Request{Operation: op, Workspace: w, Project: "p", Session: session, Args: a})
		if e != nil {
			t.Fatalf("%s: %v", op, e)
		}
		return r.Data.(map[string]any)
	}
	call("workspace_init", "", map[string]any{"mode": "server"})
	pid := call("project_create", "", map[string]any{"slug": "p"})["id"].(string)
	// An active session registered long before the retired ones.
	early := call("register_session", "early", map[string]any{})["id"].(string)
	me := call("register_session", "me", map[string]any{})["id"].(string)
	snap := call("snapshot", "me", map[string]any{})
	if snap["events_truncated"] != false || snap["sessions_truncated"] != false {
		t.Fatal("a small project's snapshot was marked truncated", snap["events_truncated"], snap["sessions_truncated"])
	}
	var wid string
	if e := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM projects WHERE id=$1`, pid).Scan(&wid); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, e := s.db.ExecContext(ctx, `INSERT INTO sessions(id,project_id,name,instance,owner_principal,role,agent_kind,model,host,account,transport,notes,status,last_seen,created_at,updated_at) SELECT 'ses_retired_'||$1||'_'||g,$1,'retired-'||g,'retired-inst-'||g,$2,'worker','','','','','','','retired',$3,$3,$3 FROM generate_series(1,1100) g`, pid, owner, now); e != nil {
		t.Fatal(e)
	}
	late := call("register_session", "late", map[string]any{})["id"].(string)
	// More history than the old 10,000-event cap, which failed every sync.
	if _, e := s.db.ExecContext(ctx, `INSERT INTO history(id,workspace_id,project_id,entity,entity_key,action,details,changed_at,changed_by_principal) SELECT 'hist_bulk_'||$2||'_'||g,$1,$2,'bulk','k'||g,'noted','{}',$3,$4 FROM generate_series(1,10050) g`, wid, pid, now, owner); e != nil {
		t.Fatal(e)
	}
	snap = call("snapshot", "me", map[string]any{})
	events := snap["events"].([]map[string]any)
	var newest int64
	if e := s.db.QueryRowContext(ctx, `SELECT MAX(sequence) FROM history WHERE workspace_id=$1 AND project_id=$2`, wid, pid).Scan(&newest); e != nil {
		t.Fatal(e)
	}
	if len(events) != snapshotEvents || snap["events_truncated"] != true {
		t.Fatal("history was not bounded to the newest window", len(events), snap["events_truncated"])
	}
	if events[len(events)-1]["sequence"] != newest || snap["events_oldest_sequence"] != events[0]["sequence"] {
		t.Fatal("the history window is not the newest events in order", events[0]["sequence"], events[len(events)-1]["sequence"], newest)
	}
	for i := 1; i < len(events); i++ {
		if events[i]["sequence"].(int64) <= events[i-1]["sequence"].(int64) {
			t.Fatal("events are not in ascending sequence order")
		}
	}
	active, retired := map[string]bool{}, 0
	for _, row := range snap["sessions"].([]map[string]any) {
		if row["status"] == "active" {
			active[row["id"].(string)] = true
			continue
		}
		retired++
		var n int
		if name := row["name"].(string); strings.HasPrefix(name, "retired-") {
			for _, c := range strings.TrimPrefix(name, "retired-") {
				n = n*10 + int(c-'0')
			}
		}
		if n <= 100 {
			t.Fatal("an older retired session displaced a recent one:", row["name"])
		}
	}
	if !active[early] || !active[me] || !active[late] || len(active) != 3 {
		t.Fatal("an active session was left out of the snapshot", active)
	}
	if retired != snapshotRetiredSessions || snap["sessions_truncated"] != true {
		t.Fatal("retired sessions were not bounded to the recent window", retired, snap["sessions_truncated"])
	}
}
