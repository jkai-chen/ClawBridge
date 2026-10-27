package bus

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func errCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return ""
}

func (h *harness) threads(a map[string]any) map[string]any {
	h.t.Helper()
	return h.call("project_threads", "", a).(map[string]any)
}

func (h *harness) thread(id string, a map[string]any) map[string]any {
	h.t.Helper()
	if a == nil {
		a = map[string]any{}
	}
	a["thread_id"] = id
	return h.call("project_thread", "", a).(map[string]any)
}

func (h *harness) threadErr(op string, a map[string]any) error {
	_, err := h.s.Execute(context.Background(), Request{Operation: op, Workspace: "w", Project: "p", Args: a})
	return err
}

// stamps is every delivery stamp and last_seen, to show a read moves none.
func (h *harness) stamps() string {
	h.t.Helper()
	rows, err := h.s.db.Query(`SELECT COALESCE(delivered_at,'-')||COALESCE(read_at,'-') FROM deliveries UNION ALL SELECT last_seen FROM sessions ORDER BY 1`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

func names(rows []map[string]any, key string) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r[key].(string))
	}
	return out
}

// The owner lists the project's threads, newest activity first, with names
// and counts only, then reads one oldest first with its recipients'
// stamps. Neither marks anything.
func TestProjectThreadsListAndRead(t *testing.T) {
	h := newHarness(t)
	q := h.send("q")
	h.call("reply", "b", map[string]any{"message_id": q["id"], "body": "an answer", "operation_id": "ans"})
	group := h.call("send", "c", map[string]any{"to": "@role:worker", "body": "to everyone", "operation_id": "g"}).(map[string]any)
	h.call("inbox", "b", nil)
	h.call("mark_read", "b", map[string]any{"ids": []any{q["id"]}})
	before := h.stamps()

	list := h.threads(nil)
	rows := list["threads"].([]map[string]any)
	if !reflect.DeepEqual(names(rows, "thread_id"), []string{group["id"].(string), q["id"].(string)}) || list["next"] != nil {
		t.Fatal(list)
	}
	if !reflect.DeepEqual(rows[1]["participants"], []string{"a", "b"}) || rows[1]["messages"] != int64(2) || rows[1]["kind"] != "message" {
		t.Fatal(rows[1])
	}
	if !reflect.DeepEqual(rows[0]["participants"], []string{"a", "b", "c"}) || rows[0]["messages"] != int64(1) {
		t.Fatal(rows[0])
	}
	var created string
	h.s.db.QueryRow(`SELECT created_at FROM projects WHERE slug='p'`).Scan(&created)
	if list["readable_from"] != created {
		t.Fatal("readable_from", list["readable_from"], created)
	}
	for _, r := range rows {
		for k := range r {
			if k == "body" || k == "excerpt" || k == "first" {
				t.Fatal("the list shows text:", r)
			}
		}
	}

	one := h.thread(q["id"].(string), nil)
	msgs := one["messages"].([]map[string]any)
	if len(msgs) != 2 || msgs[0]["body"] != "hello" || msgs[1]["body"] != "an answer" || msgs[0]["sender"] != "a" || one["next"] != nil || one["readable_from"] != created {
		t.Fatal(one)
	}
	rec := msgs[0]["recipients"].([]map[string]any)
	if len(rec) != 1 || rec[0]["name"] != "b" || rec[0]["delivered_at"] == nil || rec[0]["read_at"] == nil {
		t.Fatal(rec)
	}
	if rec = msgs[1]["recipients"].([]map[string]any); len(rec) != 1 || rec[0]["name"] != "a" || rec[0]["delivered_at"] != nil {
		t.Fatal(rec)
	}
	for _, k := range []string{"instance", "transport", "created_by_principal", "operation_id", "payload_hash"} {
		if strings.Contains(asJSON(one), `"`+k+`"`) || strings.Contains(asJSON(list), `"`+k+`"`) {
			t.Fatal("output carries", k)
		}
	}
	if h.stamps() != before {
		t.Fatal("a thread read moved a stamp")
	}
}

// A session is refused, even the owner's own; so is an instance alone.
func TestProjectThreadsRefuseSessions(t *testing.T) {
	h := newHarness(t)
	q := h.send("q")
	for _, op := range []string{"project_threads", "project_thread"} {
		a := map[string]any{"thread_id": q["id"]}
		if op == "project_threads" {
			a = map[string]any{}
		}
		_, err := h.s.Execute(context.Background(), h.request(op, "a", a))
		if errCode(err) != "forbidden" || !strings.Contains(err.Error(), "a session can't read") {
			t.Fatal(op, err)
		}
		// A session without its instance, and an instance alone.
		_, err = h.s.Execute(context.Background(), Request{Operation: op, Workspace: "w", Project: "p", Session: "a", Args: map[string]any{"thread_id": q["id"]}})
		if op == "project_threads" {
			_, err = h.s.Execute(context.Background(), Request{Operation: op, Workspace: "w", Project: "p", Session: "a", Args: map[string]any{}})
		}
		if errCode(err) != "forbidden" {
			t.Fatal(op, "session alone:", err)
		}
		a["instance"] = "a-instance"
		if err = h.threadErr(op, a); errCode(err) != "forbidden" {
			t.Fatal(op, "instance alone:", err)
		}
	}
}

// Every thread out of reach gets the one not_found, and the list leaves
// it out: another project's, expired, from before the cutoff (even with
// replies after it), a reply's ID, or none at all.
func TestProjectThreadOutOfReachIsOneAnswer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	other := func(op string, session string, a map[string]any) map[string]any {
		if session != "" {
			a["instance"] = session + "-instance"
		}
		r, err := h.s.Execute(ctx, Request{Operation: op, Workspace: "w", Project: "listed", Session: session, Args: a})
		if err != nil {
			t.Fatal(op, err)
		}
		return r.Data.(map[string]any)
	}
	other("register_session", "x", map[string]any{"role": "worker"})
	other("register_session", "y", map[string]any{"role": "worker"})
	foreign := other("send", "x", map[string]any{"to": "y", "body": "elsewhere", "operation_id": "f"})
	expired := h.send("expired")
	early := h.send("early")
	late := h.call("reply", "b", map[string]any{"message_id": early["id"], "body": "after the cutoff", "operation_id": "late"}).(map[string]any)
	kept := h.send("kept")
	if _, err := h.s.db.Exec(`UPDATE messages SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=?`, expired["id"]); err != nil {
		t.Fatal(err)
	}
	// The cutoff falls between early and its reply.
	if _, err := h.s.db.Exec(`UPDATE projects SET threads_from=(SELECT created_at FROM messages WHERE id=?) WHERE slug='p'`, late["id"]); err != nil {
		t.Fatal(err)
	}
	if got := names(h.threads(nil)["threads"].([]map[string]any), "thread_id"); !reflect.DeepEqual(got, []string{kept["id"].(string)}) {
		t.Fatal(got)
	}
	var want string
	for i, id := range []any{foreign["id"], expired["id"], early["id"], late["id"], "msg_" + strings.Repeat("0", 32)} {
		err := h.threadErr("project_thread", map[string]any{"thread_id": id})
		if errCode(err) != "not_found" || (i > 0 && err.Error() != want) {
			t.Fatal(id, err)
		}
		want = err.Error()
	}
	h.thread(kept["id"].(string), nil)
}

// The cutoff is inclusive, to the nanosecond: a message stamped at it is
// readable, one a nanosecond before is not.
func TestProjectThreadsCutoffBoundary(t *testing.T) {
	h := newHarness(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Add(600)
	on, before := h.send("on"), h.send("before")
	for id, stamp := range map[any]string{on["id"]: layout(at), before["id"]: layout(at.Add(-time.Nanosecond))} {
		if _, err := h.s.db.Exec(`UPDATE messages SET created_at=? WHERE id=?`, stamp, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.s.db.Exec(`UPDATE projects SET threads_from=? WHERE slug='p'`, layout(at)); err != nil {
		t.Fatal(err)
	}
	list := h.threads(nil)
	if got := names(list["threads"].([]map[string]any), "thread_id"); !reflect.DeepEqual(got, []string{on["id"].(string)}) || list["readable_from"] != layout(at) {
		t.Fatal(got, list["readable_from"])
	}
	if errCode(h.threadErr("project_thread", map[string]any{"thread_id": before["id"]})) != "not_found" {
		t.Fatal("read a message from before the cutoff")
	}
}

// The migration stamps every project that exists with its own time; a
// project created after it reads from its created_at.
func TestThreadsSchemaStampsExistingProjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t, s}
	h.call("workspace_init", "", map[string]any{"mode": "local"})
	h.call("project_create", "", map[string]any{"slug": "p"})
	if _, err = s.db.Exec(`DROP INDEX history_entity; ALTER TABLE projects DROP COLUMN threads_from; PRAGMA user_version=33;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	at := time.Date(2031, 5, 6, 7, 8, 9, 10, time.UTC)
	hostClock = func() time.Time { return at }
	t.Cleanup(func() { hostClock = time.Now })
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	var from string
	s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if err = s.db.QueryRow(`SELECT threads_from FROM projects WHERE slug='p'`).Scan(&from); err != nil || from != layout(at) || version != 34 {
		t.Fatal(from, version, err)
	}
	h = &harness{t, s}
	h.call("project_create", "", map[string]any{"slug": "later"})
	var later any
	if s.db.QueryRow(`SELECT threads_from FROM projects WHERE slug='later'`).Scan(&later); later != nil {
		t.Fatal("a new project was stamped:", later)
	}
}

// Pages are stable while messages arrive: the cursor is the last row's
// (last_at, thread_id), and the scope is in every query whatever it says.
func TestProjectThreadsCursor(t *testing.T) {
	h := newHarness(t)
	var ids []string
	for _, op := range []string{"t1", "t2", "t3"} {
		ids = append(ids, h.send(op)["id"].(string))
	}
	first := h.threads(map[string]any{"limit": 2})
	if got := names(first["threads"].([]map[string]any), "thread_id"); !reflect.DeepEqual(got, []string{ids[2], ids[1]}) || first["next"] == nil {
		t.Fatal(first)
	}
	// New activity on the first page's threads and a new thread don't
	// move the next page.
	h.call("reply", "b", map[string]any{"message_id": ids[2], "body": "more", "operation_id": "more"})
	h.send("t4")
	second := h.threads(map[string]any{"limit": 2, "before": first["next"]})
	if got := names(second["threads"].([]map[string]any), "thread_id"); !reflect.DeepEqual(got, []string{ids[0]}) || second["next"] != nil {
		t.Fatal(second)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []any{
		"!!", enc(`[]`), enc(`{"last_at":"2030-01-01T00:00:00Z"}`),
		enc(`{"last_at":"2030-01-01T00:00:00Z","thread_id":"` + ids[0] + `","x":1}`),
		enc(`{"last_at":"yesterday","thread_id":"` + ids[0] + `"}`),
		enc(`{"last_at":"2030-01-01T00:00:00Z","thread_id":"x' OR 1=1"}`),
		enc(`{"last_at":1,"thread_id":"` + ids[0] + `"}`),
		base64.URLEncoding.EncodeToString([]byte(`{"last_at":"2030-01-01T00:00:00Z","thread_id":"` + ids[0] + `"}`)), 7, 7.5, map[string]any{}, []any{}, nil, true,
	} {
		if err := h.threadErr("project_threads", map[string]any{"before": bad}); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	for _, bad := range []map[string]any{{"limit": 0}, {"limit": 101}} {
		if err := h.threadErr("project_threads", bad); errCode(err) != "invalid" {
			t.Fatal(bad, err)
		}
	}
	for _, bad := range []map[string]any{{"limit": 201}, {"after": -1}, {"thread_id": "q"}} {
		if _, ok := bad["thread_id"]; !ok {
			bad["thread_id"] = ids[0]
		}
		if err := h.threadErr("project_thread", bad); err == nil {
			t.Fatal(bad)
		}
	}
	// A well-formed cursor naming another project's thread finds nothing
	// outside this project.
	page := h.threads(map[string]any{"before": encodeThreadsCursor(threadsCursor{LastAt: "2999-01-01T00:00:00.000000000Z", ThreadID: "msg_" + strings.Repeat("f", 32)})})
	if len(page["threads"].([]map[string]any)) != 4 {
		t.Fatal(page)
	}
	// project_thread pages by sequence.
	h.call("reply", "a", map[string]any{"message_id": ids[2], "body": "again", "operation_id": "again"})
	p1 := h.thread(ids[2], map[string]any{"limit": 2})
	p2 := h.thread(ids[2], map[string]any{"limit": 2, "after": p1["next"]})
	if len(p1["messages"].([]map[string]any)) != 2 || p1["next"] == nil || len(p2["messages"].([]map[string]any)) != 1 || p2["next"] != nil {
		t.Fatal(p1, p2)
	}
}

// One audit row per call, with counts and the thread ID only; over 30 a
// minute is refused and writes none.
func TestProjectThreadsAuditAndRateLimit(t *testing.T) {
	h := newHarness(t)
	q := h.send("q")
	audits := func() []map[string]any {
		r, err := h.s.Execute(context.Background(), h.request("history", "", map[string]any{"entity": "threads"}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Data.(map[string]any)["events"].([]map[string]any)
	}
	h.threads(map[string]any{"limit": 7})
	h.thread(q["id"].(string), nil)
	rows := audits()
	if len(rows) != 2 || rows[0]["action"] != "project_threads" || rows[1]["action"] != "project_thread" || rows[1]["entity_key"] != q["id"] {
		t.Fatal(rows)
	}
	if asJSON(rows[0]["details"]) != `{"count":1,"limit":7}` || asJSON(rows[1]["details"]) != `{"count":1,"limit":100,"thread_id":"`+q["id"].(string)+`"}` {
		t.Fatal(rows[0]["details"], rows[1]["details"])
	}
	for i := 2; i < threadsPerMinute; i++ {
		h.threads(nil)
	}
	err := h.threadErr("project_threads", map[string]any{})
	if errCode(err) != "rate_limited" || len(audits()) != threadsPerMinute {
		t.Fatal(err, len(audits()))
	}
	if errCode(h.threadErr("project_thread", map[string]any{"thread_id": q["id"]})) != "rate_limited" {
		t.Fatal("project_thread passed the limit")
	}
	// A minute on, the window has moved.
	hostClock = func() time.Time { return time.Now().Add(61 * time.Second) }
	t.Cleanup(func() { hostClock = time.Now })
	h.threads(nil)
}

// A long participant list is cut at 20, with the rest counted.
func TestProjectThreadsParticipantsCap(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 20; i++ {
		h.call("register_session", "w"+string(rune('a'+i)), map[string]any{"instance": "w" + string(rune('a'+i)) + "-instance", "role": "crowd"})
	}
	h.call("send", "a", map[string]any{"to": "@role:crowd", "body": "all", "operation_id": "all"})
	row := h.threads(nil)["threads"].([]map[string]any)[0]
	// The sender and 20 recipients: one over.
	if len(row["participants"].([]string)) != 20 || row["participants_more"] != 1 {
		t.Fatal(row)
	}
}
