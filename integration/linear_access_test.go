package integration

import (
	"reflect"
	"sort"
	"testing"

	"clawbridge/internal/bus"
)

func l1CheckAccess(t *testing.T, value any, canWrite bool, role string, sessionID any) map[string]any {
	t.Helper()
	m := object(t, jsonData(t, value))
	var keys []string
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"can_write", "project_id", "project_role", "session_id", "session_name", "workspace_id"}) {
		t.Errorf("taskboard_access not exact six fields: %v", keys)
	}
	if m["can_write"] != canWrite || m["project_role"] != role || m["session_id"] != sessionID {
		t.Errorf("wrong access decision: role=%v can_write=%v session=%v", m["project_role"], m["can_write"], m["session_id"])
	}
	if m["workspace_id"] == nil || m["workspace_id"] == "" || m["project_id"] == nil || m["project_id"] == "" {
		t.Error("canonical access scope missing")
	}
	return m
}

func TestL1TaskboardAccessNoStampAndRetirement(t *testing.T) {
	for _, pg := range []bool{false, true} {
		name := "SQLite"
		if pg {
			name = "Postgres"
		}
		t.Run(name, func(t *testing.T) {
			a := newNotificationAuthority(t, pg)
			session, role, source := "worker", "owner", "local-authority"
			if pg {
				session, role, source = "alice", "member", "server"
			}
			var sid string
			if err := a.db.QueryRow(a.query("SELECT id FROM sessions WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=? AND slug='p') AND name=?"), a.workspaceID, session).Scan(&sid); err != nil {
				t.Fatal(err)
			}
			get := func(actorName string, args map[string]any) (bus.Result, error) {
				return a.execute(bus.Request{Operation: "taskboard_access", Workspace: a.workspace, Project: "p", Session: actorName, Args: args})
			}
			a.exec("UPDATE sessions SET last_seen=? WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=?)", "2001-01-01T00:00:00.000000000Z", a.workspaceID)
			before := l1BindingSnapshot(t, a)
			for _, actorName := range []string{"", session, session} {
				args := map[string]any{"ledger": 1}
				var expectedSession any
				if actorName != "" {
					args = actor(actorName, map[string]any{"ledger": 1})
					expectedSession = sid
				}
				r, err := get(actorName, args)
				if err != nil {
					t.Fatal(err)
				}
				if r.Source != source {
					t.Error("wrong access authority")
				}
				l1CheckAccess(t, r.Data, true, role, expectedSession)
			}
			if before != l1BindingSnapshot(t, a) {
				t.Fatal("access decision persisted actor/history/binding changes")
			}
			for _, request := range []struct {
				session string
				args    map[string]any
			}{
				{session, map[string]any{"instance": "wrong"}}, {session, nil}, {"", actor(session, nil)},
				{session, actor(session, map[string]any{"target_session": "lead"})}, {session, actor(session, map[string]any{"profile": "forbidden"})},
			} {
				if _, err := get(request.session, request.args); err == nil {
					t.Error("invalid access override/fence accepted")
				}
				if before != l1BindingSnapshot(t, a) {
					t.Error("denied access changed persisted state")
				}
			}
			if _, err := a.execute(bus.Request{Operation: "retire_session", Workspace: a.workspace, Project: "p", Session: session, Args: actor(session, nil)}); err != nil {
				t.Fatal(err)
			}
			before = l1BindingSnapshot(t, a)
			r, err := get(session, actor(session, map[string]any{"ledger": 1}))
			if err != nil {
				t.Fatal(err)
			}
			l1CheckAccess(t, r.Data, false, role, sid)
			if before != l1BindingSnapshot(t, a) {
				t.Fatal("retired access decision changed persisted state")
			}
		})
	}
}

func TestL1TaskboardAccessPostgresReaderAndActorlessRoles(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p")
	for _, name := range []string{"owner", "lead", "alice", "reader"} {
		canWrite := name != "reader"
		role := name
		if name == "alice" {
			role = "member"
		}
		// Reader's descriptive session role is lead, so it cannot serve as the
		// external mutation permission source.
		l1CheckAccess(t, f.call(pgPrincipal(name), "taskboard_access", "p", "", map[string]any{"ledger": 1}), canWrite, role, nil)
		active := object(t, f.call(pgPrincipal(name), "taskboard_access", "p", name, actor(name, map[string]any{"ledger": 1})))
		id, ok := active["session_id"].(string)
		if !ok || id == name || id == "" {
			t.Fatal("access did not resolve canonical actor ID")
		}
		l1CheckAccess(t, active, canWrite, role, id)
	}
	f.reject(pgOwner, "taskboard_access", "p", "alice", actor("alice", map[string]any{"ledger": 1}))
	f.reject("stranger@example.test", "taskboard_access", "p", "", map[string]any{"ledger": 1})
	f.call(pgOwner, "project_create", "", "", map[string]any{"slug": "restricted"})
	f.reject(pgPrincipal("reader"), "taskboard_access", "restricted", "", nil)
}
