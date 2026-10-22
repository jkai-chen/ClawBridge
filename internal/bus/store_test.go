package bus

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type harness struct {
	t *testing.T
	s *Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "core.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	h := &harness{t, s}
	h.call("workspace_init", "", map[string]any{"mode": "local"})
	h.call("project_create", "", map[string]any{"slug": "listed"})
	projects := h.call("list_projects", "", nil).(map[string]any)
	if len(projects["projects"].([]map[string]any)) != 1 {
		t.Fatal("local project listing omitted project")
	}
	h.call("project_create", "", map[string]any{"slug": "p"})
	for _, name := range []string{"a", "b", "c"} {
		h.call("register_session", name, map[string]any{"instance": name + "-instance", "role": "worker"})
	}
	return h
}
func (h *harness) request(op, session string, a map[string]any) Request {
	if a == nil {
		a = map[string]any{}
	}
	if session != "" && op != "register_session" {
		if _, ok := a["instance"]; !ok {
			a["instance"] = session + "-instance"
		}
	}
	return Request{Operation: op, Workspace: "w", Project: "p", Session: session, Args: a}
}
func (h *harness) call(op, session string, a map[string]any) any {
	h.t.Helper()
	r, e := h.s.Execute(context.Background(), h.request(op, session, a))
	if e != nil {
		h.t.Fatalf("%s: %v", op, e)
	}
	return r.Data
}
func (h *harness) fail(op, session string, a map[string]any) {
	h.t.Helper()
	if _, e := h.s.Execute(context.Background(), h.request(op, session, a)); e == nil {
		h.t.Fatalf("%s unexpectedly succeeded", op)
	}
}
func (h *harness) send(opid string) map[string]any {
	return h.call("send", "a", map[string]any{"to": "b", "body": "hello", "operation_id": opid}).(map[string]any)
}
func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if e := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func TestDeliveryFailureRollsBackMessageAndAudit(t *testing.T) {
	h := newHarness(t)
	before := count(t, h.s.db, "history")
	_, e := h.s.db.Exec(`CREATE TRIGGER fail_delivery BEFORE INSERT ON deliveries WHEN (SELECT name FROM sessions WHERE id=NEW.recipient_id)='c' BEGIN SELECT RAISE(ABORT,'injected delivery failure'); END`)
	if e != nil {
		t.Fatal(e)
	}
	h.fail("send", "a", map[string]any{"to": "@role:worker", "body": "atomic expansion", "operation_id": "retry"})
	for _, table := range []string{"messages", "deliveries"} {
		if n := count(t, h.s.db, table); n != 0 {
			t.Fatalf("partial %s: %d", table, n)
		}
	}
	if n := count(t, h.s.db, "history"); n != before {
		t.Fatal("failed delivery appended history")
	}
	if _, e = h.s.db.Exec("DROP TRIGGER fail_delivery"); e != nil {
		t.Fatal(e)
	}
	sent := h.call("send", "a", map[string]any{"to": "@role:worker", "body": "atomic expansion", "operation_id": "retry"}).(map[string]any)
	if len(sent["deliveries"].([]map[string]any)) != 2 {
		t.Fatal("safe retry did not atomically expand recipients")
	}
}
func TestMixedMarkReadRollsBackEarlierDelivery(t *testing.T) {
	h := newHarness(t)
	id := h.send("mixed")["id"].(string)
	before := count(t, h.s.db, "history")
	h.fail("mark_read", "b", map[string]any{"ids": []string{id, "missing-message"}})
	var delivered, read sql.NullString
	if e := h.s.db.QueryRow(`SELECT delivered_at,read_at FROM deliveries WHERE message_id=?`, id).Scan(&delivered, &read); e != nil {
		t.Fatal(e)
	}
	if delivered.Valid || read.Valid {
		t.Fatal("partial delivery stamps escaped rollback")
	}
	if count(t, h.s.db, "history") != before {
		t.Fatal("failed mark_read appended history")
	}
}
func TestResumeFencesOldInstanceAndKeepsDeliveryIdentity(t *testing.T) {
	h := newHarness(t)
	sent := h.send("before-resume")
	old := h.call("whoami", "b", nil).(map[string]any)["session"].(map[string]any)
	next := h.call("register_session", "b", map[string]any{"instance": "b-instance", "resume": true}).(map[string]any)
	if next["id"] != old["id"] || next["instance"] == old["instance"] {
		t.Fatal("resume changed ID or did not rotate instance")
	}
	for _, op := range []string{"inbox", "whoami", "list_sessions", "history", "update_session", "retire_session"} {
		h.fail(op, "b", nil)
	}
	data := h.call("inbox", "b", map[string]any{"instance": next["instance"]}).(map[string]any)
	messages := data["messages"].([]map[string]any)
	if len(messages) != 1 || messages[0]["id"] != sent["id"] {
		t.Fatal("resume lost existing deliveries")
	}
	again := h.call("register_session", "b", map[string]any{"instance": next["instance"]}).(map[string]any)
	if again["id"] != next["id"] || again["instance"] != next["instance"] {
		t.Fatal("normal register rotated identity")
	}
	h.fail("register_session", "missing", map[string]any{"resume": true, "instance": "none"})
}
func TestPresenceHeartbeatAndFailureRollback(t *testing.T) {
	h := newHarness(t)
	stale := time.Now().UTC().Add(-time.Hour).Format(timestampLayout)
	if _, e := h.s.db.Exec(`UPDATE sessions SET last_seen=? WHERE name='b'`, stale); e != nil {
		t.Fatal(e)
	}
	page := h.call("list_sessions", "", nil).(map[string]any)
	for _, s := range page["sessions"].([]map[string]any) {
		if s["name"] == "b" && s["presence"] != "stale" {
			t.Fatal("crashed session not shown stale")
		}
	}
	h.fail("send", "b", map[string]any{"to": "missing", "body": "failed heartbeat", "operation_id": "bad"})
	var last string
	if e := h.s.db.QueryRow(`SELECT last_seen FROM sessions WHERE name='b'`).Scan(&last); e != nil {
		t.Fatal(e)
	}
	if last != stale {
		t.Fatal("failed operation changed last_seen")
	}
	actor := h.call("whoami", "b", nil).(map[string]any)["session"].(map[string]any)
	if actor["presence"] != "active" || actor["stale"] != false || actor["last_seen"] == stale {
		t.Fatal("successful actor use failed to refresh presence")
	}
}
func TestUnsupportedSchemaAndUnrelatedDatabaseUntouched(t *testing.T) {
	for _, kind := range []string{"newer", "unrelated"} {
		t.Run(kind, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "db")
			db, e := sql.Open("sqlite", p)
			if e != nil {
				t.Fatal(e)
			}
			sqlText := "PRAGMA user_version=99"
			if kind == "unrelated" {
				sqlText = "CREATE TABLE business_data(id INTEGER)"
			}
			if _, e = db.Exec(sqlText); e != nil {
				t.Fatal(e)
			}
			db.Close()
			if s, e := Open(p); e == nil {
				s.Close()
				t.Fatal("unsupported database accepted")
			}
			db, e = sql.Open("sqlite", p)
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			var n int
			if e = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='workspaces'`).Scan(&n); e != nil {
				t.Fatal(e)
			}
			if n != 0 {
				t.Fatal("unsupported database modified")
			}
		})
	}
}
func TestNormalizeRemote(t *testing.T) {
	for _, raw := range []string{"git@GitHub.com:Acme/Repo.git", "ssh://git@github.com/Acme/Repo.git", "https://github.com/Acme/Repo.git/", "github.com/Acme/Repo"} {
		got, e := NormalizeRemote(raw)
		if e != nil || got != "github.com/Acme/Repo" {
			t.Errorf("%q = %q, %v", raw, got, e)
		}
	}
	for _, raw := range []string{"https://github.com/Acme/Repo.git?token=hidden", "missing-path", "https:///missing-host"} {
		if _, e := NormalizeRemote(raw); e == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	got, e := NormalizeRemote("ssh://git@host.example:2222/Case/Repo.git")
	if e != nil || got != "host.example:2222/Case/Repo" {
		t.Fatal(got, e)
	}
}
func TestBoundedInputAndNoImplicitAuthority(t *testing.T) {
	h := newHarness(t)
	h.fail("send", "a", map[string]any{"to": "b", "body": strings.Repeat("x", 65537), "operation_id": "oversize"})
	h.fail("inbox", "b", map[string]any{"limit": 1001})
	h.fail("inbox", "b", map[string]any{"after": -1})
	h.fail("register_session", "unknown", map[string]any{"transport": map[string]any{"bad": func() {}}})
	p := filepath.Join(t.TempDir(), "empty.db")
	s, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Execute(context.Background(), Request{Operation: "project_create", Workspace: "missing", Args: map[string]any{"slug": "p"}}); e == nil {
		t.Fatal("opening database implicitly initialized writable workspace")
	}
}
func TestSessionNameCannotImpersonateSessionID(t *testing.T) {
	h := newHarness(t)
	victim := h.call("whoami", "b", nil).(map[string]any)["session"].(map[string]any)["id"].(string)
	for _, name := range []string{victim, strings.ToUpper(victim[:4]) + victim[4:], "ses_custom"} {
		h.fail("register_session", name, map[string]any{"instance": "impostor-instance"})
	}
	// A pre-existing row (allowed before this fix) carrying the victim's ID as its name.
	if _, e := h.s.db.Exec(`UPDATE sessions SET name=? WHERE name='c'`, victim); e != nil {
		t.Fatal(e)
	}
	impostor := func() int {
		var n int
		if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM deliveries d JOIN sessions s ON s.id=d.recipient_id WHERE s.name=?`, victim).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	sent := h.call("send", "a", map[string]any{"to": victim, "body": "to the real b", "operation_id": "before-retire"}).(map[string]any)
	if d := sent["deliveries"].([]map[string]any); len(d) != 1 || d[0]["recipient_id"] != victim || impostor() != 0 {
		t.Fatal("ID address was ambiguous or reached the impostor", d)
	}
	question := h.call("send", "b", map[string]any{"to": "a", "body": "question", "operation_id": "question"}).(map[string]any)
	h.call("retire_session", "b", nil)
	h.fail("send", "a", map[string]any{"to": victim, "body": "after retire", "operation_id": "after-retire"})
	h.fail("reply", "a", map[string]any{"message_id": question["id"], "body": "answer", "operation_id": "reply-retired"})
	h.fail("retire_session", "a", map[string]any{"successor": victim})
	if impostor() != 0 {
		t.Fatal("retired session ID address was forwarded to an impostor name")
	}
}
func TestProjectRoleRevokeRequiresHostedAuthority(t *testing.T) {
	h := newHarness(t)
	if _, e := h.s.Execute(context.Background(), h.request("project_role_revoke", "", map[string]any{"principal": "bob@example.com"})); e == nil || !strings.Contains(e.Error(), "hosted authority") {
		t.Fatal("local workspace accepted role revocation", e)
	}
}
