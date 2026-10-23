package bus

import (
	"context"
	"strings"
	"testing"
)

// systemSession is the system session of session c's project, created by
// the watcher's first call there.
func (h *harness) systemSession(t *testing.T) map[string]any {
	t.Helper()
	c := h.sessionID("c")
	var pid string
	if err := h.s.db.QueryRow(`SELECT project_id FROM sessions WHERE id=?`, c).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.ExecuteSystem(context.Background(), pid, Request{Operation: "taskboard_get_v2"}); err != nil {
		t.Fatal(err)
	}
	tx, err := h.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := query(context.Background(), &transaction{Tx: tx}, `SELECT * FROM sessions WHERE role='system' AND project_id=(SELECT project_id FROM sessions WHERE id=?)`, c)
	if err != nil || len(rows) != 1 {
		t.Fatal("system sessions:", rows, err)
	}
	return rows[0]
}

// The system session exists per project and no request reaches it.
func TestSystemSessionIsUnreachableFromRequests(t *testing.T) {
	h := newHarness(t)
	sys := h.systemSession(t)
	if sys["name"] != SystemSessionName || sys["owner_principal"] != "" || sys["status"] != "active" {
		t.Fatal(sys)
	}
	for _, name := range []string{"clawbridge", "ClawBridge"} {
		if e := h.errorOf("register_session", name, map[string]any{"instance": "x"}); !strings.Contains(e, "reserved") {
			t.Fatal("registered as the system session:", name, e)
		}
	}
	// Acting as it, even with its real instance, fails.
	for _, as := range []string{str(sys, "id"), SystemSessionName} {
		r := h.request("whoami", as, map[string]any{"instance": sys["instance"]})
		if out, e := h.s.Execute(context.Background(), r); e == nil && out.Data.(map[string]any)["session"] != nil {
			t.Fatal("a request acted as the system session:", as, out)
		}
	}
	for name, a := range map[string]map[string]any{
		"send by name":        {"to": "clawbridge", "body": "hi", "operation_id": "s1"},
		"send by id":          {"to": str(sys, "id"), "body": "hi", "operation_id": "s2"},
		"coordinator by name": {"target_session": "clawbridge"},
	} {
		op := "send"
		if strings.HasPrefix(name, "coordinator") {
			op = "coordinator_set"
		}
		if e := h.errorOf(op, "c", a); e == "" {
			t.Fatal(name, "reached the system session")
		}
	}
	// @all never includes it, and list_sessions hides it.
	h.call("send", "a", map[string]any{"to": "@all", "body": "hello", "operation_id": "all"})
	if e := h.errorOf("send", "a", map[string]any{"to": "@role:system", "body": "hello", "operation_id": "role"}); !strings.Contains(e, "no active recipients") {
		t.Fatal("@role:system found a recipient:", e)
	}
	var delivered int
	if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE recipient_id=?`, str(sys, "id")).Scan(&delivered); e != nil || delivered != 0 {
		t.Fatal("@all or @role reached the system session:", delivered, e)
	}
	list := h.call("list_sessions", "a", nil).(map[string]any)
	for _, s := range list["sessions"].([]map[string]any) {
		if s["role"] == "system" {
			t.Fatal("list_sessions shows the system session")
		}
	}
}

func anyStrings(v any) []string {
	out := []string{}
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		for _, x := range list {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// A person's session named clawbridge before the name was reserved keeps
// working under a new name once the watcher first acts for its project.
func TestSystemSessionRenamesAPersonsClawbridgeOnFirstUse(t *testing.T) {
	h := newHarness(t)
	a := h.sessionID("a")
	var pid string
	if e := h.s.db.QueryRow(`SELECT project_id FROM sessions WHERE id=?`, a).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE role='system'`).Scan(&n); e != nil || n != 0 {
		t.Fatal("a system session exists before the watcher acted:", n, e)
	}
	if _, e := h.s.db.Exec(`UPDATE sessions SET name='clawbridge' WHERE id=?`, a); e != nil {
		t.Fatal(e)
	}
	if _, err := h.s.ExecuteSystem(context.Background(), pid, Request{Operation: "taskboard_get_v2"}); err != nil {
		t.Fatal(err)
	}
	var name string
	if e := h.s.db.QueryRow(`SELECT name FROM sessions WHERE id=?`, a).Scan(&name); e != nil || name != "clawbridge-user-1" {
		t.Fatal(name, e)
	}
	if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM sessions s WHERE role='system' AND name='clawbridge' AND project_id=(SELECT project_id FROM sessions WHERE id=?)`, a).Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}

func TestExecuteSystemRunsOnlyWatcherOperations(t *testing.T) {
	h := newHarness(t)
	var pid string
	if e := h.s.db.QueryRow(`SELECT project_id FROM sessions LIMIT 1`).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if _, e := h.s.ExecuteSystem(ctx, pid, Request{Operation: "send", Args: map[string]any{"to": "a", "body": "x", "operation_id": "o"}}); e == nil {
		t.Fatal("the system session sent a message")
	}
	if _, e := h.s.ExecuteSystem(ctx, pid, Request{Operation: "comment_watch_lease", Args: map[string]any{"action": "acquire", "bogus": 1}}); e == nil {
		t.Fatal("unvalidated arguments accepted")
	}
	r, e := h.s.ExecuteSystem(ctx, pid, Request{Operation: "comment_watch_lease", Args: map[string]any{"action": "acquire"}})
	if e != nil || r.Source != "server" || r.Data.(map[string]any)["lease_token"] == nil {
		t.Fatal(r, e)
	}
	access, e := h.s.ExecuteSystem(ctx, pid, Request{Operation: "taskboard_access", Args: map[string]any{"ledger": 1}})
	if e != nil {
		t.Fatal(e)
	}
	got, err := ParseTaskboardAccess(access.Data)
	if err != nil || got.SessionName != SystemSessionName || !got.CanWrite {
		t.Fatal(got, err)
	}
}

// Ticks rotate over bound projects, the longest since a tick first, and
// skip projects with no binding (IN-M1).
func TestWatchProjectsRotateByLastTick(t *testing.T) {
	h := newHarness(t)
	projects := []string{}
	rows, err := h.s.db.Query(`SELECT id FROM projects ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		projects = append(projects, id)
	}
	rows.Close()
	if len(projects) < 2 {
		t.Fatal(projects)
	}
	ctx := context.Background()
	if got, _ := h.s.WatchProjects(ctx, 10); len(got) != 0 {
		t.Fatal("unbound projects are watched:", got)
	}
	for _, p := range projects {
		if _, e := h.s.db.Exec(`INSERT INTO taskboard_bindings(project_id,revision,mapping,updated_at) VALUES(?,1,'{}','2026-01-01T00:00:00Z')`, p); e != nil {
			t.Fatal(e)
		}
	}
	if err = h.s.WatchTicked(ctx, projects[0], true, ""); err != nil {
		t.Fatal(err)
	}
	got, err := h.s.WatchProjects(ctx, 1)
	if err != nil || len(got) != 1 || got[0] != projects[1] {
		t.Fatal("the never-ticked project did not come first:", got, err)
	}
	if err = h.s.WatchTicked(ctx, projects[1], false, "deadline"); err != nil {
		t.Fatal(err)
	}
	if got, _ = h.s.WatchProjects(ctx, 1); got[0] != projects[0] {
		t.Fatal("the rotation did not move on:", got)
	}
}
