package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// Only disposable fixture databases are inspected directly. This observer never
// calls whoami/history/message_status, which could themselves refresh presence.
type notificationAuthority struct {
	t           *testing.T
	db          *sql.DB
	pg          bool
	workspace   string
	workspaceID string
	execute     func(bus.Request) (bus.Result, error)
	send        func(string, string, string, string) any
}

func newNotificationAuthority(t *testing.T, pg bool) *notificationAuthority {
	t.Helper()
	a := &notificationAuthority{t: t, pg: pg}
	var driver, dsn string
	if pg {
		f := newPGFixture(t)
		f.setup("p", "other")
		a.workspace = f.w
		a.execute = func(r bus.Request) (bus.Result, error) {
			return f.store.ExecuteAs(context.Background(), pgPrincipal("alice"), r)
		}
		a.send = func(p, from, to, id string) any {
			return f.send(p, from, to, "private notification body must stay in inbox", id)
		}
		driver, dsn = "pgx", os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	} else {
		f := newFixture(t)
		f.setup("w", "p", "other")
		f.setup("other-workspace", "p")
		a.workspace = "w"
		a.execute = func(r bus.Request) (bus.Result, error) { return f.store.Execute(context.Background(), r) }
		a.send = func(p, from, to, id string) any {
			return f.send("w", p, from, to, "private notification body must stay in inbox", id)
		}
		driver, dsn = "sqlite", f.path
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a.db = db
	if err := db.QueryRow(a.query("SELECT id FROM workspaces WHERE name=?"), a.workspace).Scan(&a.workspaceID); err != nil {
		t.Fatal(err)
	}
	return a
}
func (a *notificationAuthority) query(s string) string {
	if !a.pg {
		return s
	}
	n := 0
	for strings.Contains(s, "?") {
		n++
		s = strings.Replace(s, "?", fmt.Sprintf("$%d", n), 1)
	}
	return s
}
func (a *notificationAuthority) exec(s string, args ...any) {
	a.t.Helper()
	if _, err := a.db.Exec(a.query(s), args...); err != nil {
		a.t.Fatal(err)
	}
}
func (a *notificationAuthority) poll(session string, args map[string]any) (bus.Result, error) {
	return a.execute(bus.Request{Operation: "inbox_status", Workspace: a.workspace, Project: "p", Session: session, Args: args})
}
func (a *notificationAuthority) snapshot() string {
	a.t.Helper()
	tables := map[string]string{
		"workspaces": "id=?", "projects": "workspace_id=?", "sessions": "project_id IN (SELECT id FROM projects WHERE workspace_id=?)",
		"messages": "project_id IN (SELECT id FROM projects WHERE workspace_id=?)", "deliveries": "project_id IN (SELECT id FROM projects WHERE workspace_id=?)", "history": "workspace_id=?",
	}
	if a.pg {
		tables["members"] = "workspace_id=?"
		tables["project_roles"] = "project_id IN (SELECT id FROM projects WHERE workspace_id=?)"
	}
	all := map[string][]string{}
	for table, where := range tables {
		rows, err := a.db.Query(a.query("SELECT * FROM "+table+" WHERE "+where), a.workspaceID)
		if err != nil {
			a.t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			a.t.Fatal(err)
		}
		records := []string{}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				a.t.Fatal(err)
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					vals[i] = string(b)
				}
			}
			encoded, err := json.Marshal(vals)
			if err != nil {
				a.t.Fatal(err)
			}
			records = append(records, string(encoded))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			a.t.Fatal(err)
		}
		if table == "sessions" && len(records) == 0 {
			a.t.Fatal("persisted-state observer captured no fixture sessions")
		}
		sort.Strings(records)
		all[table] = records
	}
	encoded, err := json.Marshal(all)
	if err != nil {
		a.t.Fatal(err)
	}
	return string(encoded)
}
func notificationStatus(t *testing.T, r bus.Result, err error, pg bool, unread, latest, fresh float64) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	source := "local-authority"
	if pg {
		source = "server"
	}
	if r.Source != source {
		t.Fatalf("source=%q", r.Source)
	}
	data := object(t, jsonData(t, r.Data))
	keys := []string{}
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"latest_sequence", "new_count", "project_id", "session_id", "unread_count", "workspace_id"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("status keys=%v", keys)
	}
	if data["unread_count"] != unread || data["latest_sequence"] != latest || data["new_count"] != fresh {
		t.Fatalf("unexpected counts: %v; want %.0f/%.0f/%.0f", data, unread, latest, fresh)
	}
	for _, k := range []string{"workspace_id", "project_id", "session_id"} {
		stringField(t, data, k)
	}
	return data
}

func TestNotificationInboxStatusNoPersistedWrites(t *testing.T) {
	for _, pg := range []bool{false, true} {
		name := "SQLite"
		if pg {
			name = "Postgres"
		}
		t.Run(name, func(t *testing.T) {
			a := newNotificationAuthority(t, pg)
			recipient, from, outsider := "worker", "lead", "outsider"
			if pg {
				recipient, from, outsider = "alice", "bob", "owner"
			}
			instance := recipient + "-instance"
			// Force distinguishable old presence rather than relying on clock sleeps.
			a.exec("UPDATE sessions SET last_seen=? WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=?)", "2001-01-01T00:00:00.000000000Z", a.workspaceID)
			before := a.snapshot()
			for i := 0; i < 3; i++ {
				r, e := a.poll(recipient, map[string]any{"instance": instance})
				notificationStatus(t, r, e, pg, 0, 0, 0)
			}
			if before != a.snapshot() {
				t.Fatal("empty status mutated persisted authority state")
			}
			first := a.send("p", from, recipient, "notification-first")
			second := a.send("p", from, recipient, "notification-second")
			a.send("p", from, outsider, "notification-other-recipient")
			a.send("other", from, recipient, "notification-other-project")
			if !pg { // Same immutable selectors in another workspace cannot affect counts.
				// Existing fixture setup already supplies the colliding scope and actor.
				r, e := a.execute(bus.Request{Operation: "inbox_status", Workspace: "other-workspace", Project: "p", Session: recipient, Args: map[string]any{"instance": instance}})
				notificationStatus(t, r, e, false, 0, 0, 0)
			}
			seq1 := object(t, first)["sequence"].(float64)
			seq2 := object(t, second)["sequence"].(float64)
			a.exec("UPDATE sessions SET last_seen=? WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=?)", "2001-01-01T00:00:00.000000000Z", a.workspaceID)
			before = a.snapshot()
			for i := 0; i < 3; i++ {
				for _, tc := range []struct {
					after any
					fresh float64
				}{{0, 2}, {seq1, 1}, {seq2, 0}, {9007199254740991, 0}} {
					r, e := a.poll(recipient, map[string]any{"instance": instance, "after": tc.after})
					notificationStatus(t, r, e, pg, 2, seq2, tc.fresh)
				}
			}
			for _, args := range []map[string]any{
				{}, {"instance": "wrong"}, {"instance": instance, "after": -1}, {"instance": instance, "after": 1.5}, {"instance": instance, "after": 9007199254740992.0},
				{"instance": instance, "target_session": outsider}, {"instance": instance, "principal": "stranger@example.test"}, {"instance": instance, "limit": 1},
			} {
				if _, e := a.poll(recipient, args); e == nil {
					t.Fatalf("invalid status args accepted: %v", args)
				}
			}
			if _, e := a.poll("", map[string]any{"instance": instance}); e == nil {
				t.Fatal("actorless status accepted")
			}
			if before != a.snapshot() {
				t.Fatal("status/denial mutated authority: presence, delivery, read, history or other persisted rows changed")
			}
			// Simulate expiry/read in fixture data, with no production time override.
			a.exec("UPDATE messages SET expires_at=? WHERE id=?", time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000000000Z07:00"), stringField(t, first, "id"))
			before = a.snapshot()
			r, e := a.poll(recipient, map[string]any{"instance": instance})
			notificationStatus(t, r, e, pg, 1, seq2, 1)
			if before != a.snapshot() {
				t.Fatal("expiry status performed cleanup or other persisted write")
			}
			a.exec("UPDATE deliveries SET read_at=? WHERE message_id=?", "2002-01-01T00:00:00.000000000Z", stringField(t, second, "id"))
			before = a.snapshot()
			r, e = a.poll(recipient, map[string]any{"instance": instance})
			notificationStatus(t, r, e, pg, 0, 0, 0)
			if before != a.snapshot() {
				t.Fatal("read-filter status mutated persisted data")
			}
			role := "@role:worker"
			if pg {
				role = "@role:lead"
			}
			expanded := a.send("p", from, role, "notification-role-expanded")
			before = a.snapshot()
			r, e = a.poll(recipient, map[string]any{"instance": instance})
			data := notificationStatus(t, r, e, pg, 1, object(t, expanded)["sequence"].(float64), 1)
			var pid, sid string
			if err := a.db.QueryRow(a.query("SELECT p.id,s.id FROM projects p JOIN sessions s ON s.project_id=p.id WHERE p.workspace_id=? AND p.slug='p' AND s.name=?"), a.workspaceID, recipient).Scan(&pid, &sid); err != nil {
				t.Fatal(err)
			}
			if data["workspace_id"] != a.workspaceID || data["project_id"] != pid || data["session_id"] != sid {
				t.Fatal("status scope differs from authorized immutable recipient")
			}
			if before != a.snapshot() {
				t.Fatal("role-expanded status mutated persisted data")
			}
		})
	}
}

func TestNotificationInboxStatusActorFencing(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	f.call("register_session", "w", "p", "worker", map[string]any{"instance": "worker-instance", "resume": true})
	f.reject("inbox_status", "w", "p", "worker", actor("worker", nil))
	f.call("retire_session", "w", "p", "lead", actor("lead", map[string]any{"successor": "outsider"}))
	f.reject("inbox_status", "w", "p", "lead", actor("lead", nil))
	f.call("retire_session", "w", "p", "outsider", actor("outsider", nil))
	f.reject("inbox_status", "w", "p", "outsider", actor("outsider", nil))
}

func TestNotificationPostgresPrincipalAndScopeFencing(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p", "other")
	for _, name := range []string{"alice", "reader"} {
		sent := f.send("p", "bob", name, "private recipient mail", "notification-"+name)
		r, e := f.store.ExecuteAs(context.Background(), pgPrincipal(name), f.request("inbox_status", "p", name, actor(name, nil)))
		notificationStatus(t, r, e, true, 1, object(t, sent)["sequence"].(float64), 1)
		r, e = f.store.ExecuteAs(context.Background(), pgPrincipal(name), f.request("inbox_status", "other", name, actor(name, nil)))
		notificationStatus(t, r, e, true, 0, 0, 0)
	}
	for _, name := range []string{"bob", "lead", "owner", "stranger"} {
		f.reject(pgPrincipal(name), "inbox_status", "p", "alice", actor("alice", nil))
	}
	f.call(pgOwner, "project_create", "", "", map[string]any{"slug": "restricted"})
	f.reject(pgPrincipal("alice"), "inbox_status", "restricted", "alice", actor("alice", nil))
	db, err := sql.Open("pgx", os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Revoke membership directly in this disposable fixture to exercise a watcher
	// that was authorized at startup but must revalidate each subsequent poll.
	if _, err = db.Exec("DELETE FROM project_roles WHERE principal=$1 AND project_id IN (SELECT id FROM projects WHERE workspace_id IN (SELECT id FROM workspaces WHERE name=$2) AND slug='p')", pgPrincipal("alice"), f.w); err != nil {
		t.Fatal(err)
	}
	f.reject(pgPrincipal("alice"), "inbox_status", "p", "alice", actor("alice", nil))
	f.role("p", "alice", "member")
	resumed := f.call(pgPrincipal("alice"), "register_session", "p", "alice", map[string]any{"instance": "alice-instance", "resume": true})
	f.reject(pgPrincipal("alice"), "inbox_status", "p", "alice", actor("alice", nil))
	next := stringField(t, resumed, "instance")
	f.call(pgPrincipal("alice"), "inbox_status", "p", "alice", map[string]any{"instance": next})
	f.call(pgPrincipal("alice"), "retire_session", "p", "alice", map[string]any{"instance": next, "successor": "bob"})
	f.reject(pgPrincipal("alice"), "inbox_status", "p", "alice", map[string]any{"instance": next})
}
