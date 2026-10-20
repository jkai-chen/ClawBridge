package integration

import (
	"context"
	"encoding/json"

	"clawbridge/internal/bus"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func array(t *testing.T, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("expected array, got %T: %v", v, v)
	}
	return a
}
func deliveries(t *testing.T, v any) []any { t.Helper(); return array(t, object(t, v)["deliveries"]) }
func onlyDelivery(t *testing.T, v any) map[string]any {
	t.Helper()
	d := deliveries(t, v)
	if len(d) != 1 {
		t.Fatalf("expected one delivery: %v", v)
	}
	return object(t, d[0])
}
func assertStamp(t *testing.T, v any, start, end time.Time) {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("expected timestamp string: %v", v)
	}
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	if at.Before(start) || at.After(end) {
		t.Fatalf("authority time %s not in [%s,%s]", at, start, end)
	}
}

func TestDeliveryStateThreadsAndAtomicMarkRead(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p", "other")
	start := time.Now().UTC()
	sent := f.send("w", "p", "lead", "worker", "root", "root")
	end := time.Now().UTC()
	id := stringField(t, sent, "id")
	root := stringField(t, sent, "thread_id")
	if id != root || object(t, sent)["reply_to"] != nil {
		t.Fatalf("root ancestry invalid: %v", sent)
	}
	assertStamp(t, object(t, sent)["created_at"], start, end)
	status := f.call("message_status", "w", "p", "lead", actor("lead", map[string]any{"message_id": id}))
	d := onlyDelivery(t, status)
	if d["delivered_at"] != nil || d["read_at"] != nil {
		t.Fatalf("send falsely marked delivery/read: %v", d)
	}
	inbox := pageItems(t, "messages", f.call("inbox", "w", "p", "worker", actor("worker", nil)))
	if len(inbox) != 1 || stringField(t, inbox[0], "id") != id {
		t.Fatalf("unexpected inbox %v", inbox)
	}
	status = f.call("message_status", "w", "p", "lead", actor("lead", map[string]any{"message_id": id}))
	d = onlyDelivery(t, status)
	delivered := d["delivered_at"]
	if delivered == nil || d["read_at"] != nil {
		t.Fatalf("fetch must deliver without reading: %v", d)
	}
	f.call("inbox", "w", "p", "worker", actor("worker", nil))
	foreign := f.send("w", "other", "lead", "worker", "foreign", "foreign")
	f.reject("mark_read", "w", "p", "worker", actor("worker", map[string]any{"ids": []string{id, stringField(t, foreign, "id")}}))
	afterFailed := onlyDelivery(t, f.call("message_status", "w", "p", "lead", actor("lead", map[string]any{"message_id": id})))
	if afterFailed["read_at"] != nil {
		t.Fatal("mixed-scope mark_read partially committed")
	}
	f.call("mark_read", "w", "p", "worker", actor("worker", map[string]any{"ids": []string{id, id}}))
	read := onlyDelivery(t, f.call("message_status", "w", "p", "lead", actor("lead", map[string]any{"message_id": id})))
	if read["read_at"] == nil || read["delivered_at"] != delivered {
		t.Fatalf("mark_read lost delivery time: %v", read)
	}
	f.call("mark_read", "w", "p", "worker", actor("worker", map[string]any{"ids": []string{id}}))
	repeat := onlyDelivery(t, f.call("message_status", "w", "p", "lead", actor("lead", map[string]any{"message_id": id})))
	if !reflect.DeepEqual(read, repeat) {
		t.Fatalf("repeated mark_read changed stamps: %v vs %v", read, repeat)
	}
	if got := pageItems(t, "messages", f.call("inbox", "w", "p", "worker", actor("worker", nil))); len(got) != 0 {
		t.Fatalf("read message remains unread: %v", got)
	}
	firstReply := f.call("reply", "w", "p", "worker", actor("worker", map[string]any{"message_id": id, "body": "reply one", "operation_id": "reply-one"}))
	firstID := stringField(t, firstReply, "id")
	if stringField(t, firstReply, "thread_id") != root || stringField(t, firstReply, "reply_to") != id {
		t.Fatalf("first reply ancestry %v", firstReply)
	}
	secondReply := f.call("reply", "w", "p", "lead", actor("lead", map[string]any{"message_id": firstID, "body": "reply two", "operation_id": "reply-two"}))
	if stringField(t, secondReply, "thread_id") != root || stringField(t, secondReply, "reply_to") != firstID {
		t.Fatalf("nested reply must preserve root and immediate parent: %v", secondReply)
	}
	f.reject("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": "non-root thread", "thread_id": firstID, "operation_id": "bad-root"}))
	f.reject("send", "w", "p", "outsider", actor("outsider", map[string]any{"to": "worker", "body": "not a participant", "thread_id": root, "operation_id": "not-participant"}))
}

func TestBroadcastRoleSnapshotAndReplay(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p", "other")
	role := f.send("w", "p", "worker", "@role:lead", "role mail", "role-mail")
	if len(deliveries(t, role)) != 1 {
		t.Fatalf("role expansion incorrect: %v", role)
	}
	all := f.send("w", "p", "lead", "@all", "broadcast", "broadcast")
	if len(deliveries(t, all)) != 2 {
		t.Fatalf("broadcast should snapshot other active sessions: %v", all)
	}
	f.call("register_session", "w", "p", "late", map[string]any{"instance": "late-instance", "role": "lead"})
	late := pageItems(t, "messages", f.call("inbox", "w", "p", "late", actor("late", nil)))
	if len(late) != 0 {
		t.Fatalf("late registration inherited deliveries: %v", late)
	}
	// Membership changes must not turn a retry into a new fanout.
	repeated := f.send("w", "p", "lead", "@all", "broadcast", "broadcast")
	if stringField(t, all, "id") != stringField(t, repeated, "id") || len(deliveries(t, repeated)) != 2 {
		t.Fatalf("retry re-expanded recipients: %v", repeated)
	}
	for _, project := range []string{"p", "other"} {
		sessions := pageItems(t, "sessions", f.call("list_sessions", "w", project, "", nil))
		expected := 3
		if project == "p" {
			expected = 4
		}
		if len(sessions) != expected {
			t.Fatalf("%s sessions count %d want %d", project, len(sessions), expected)
		}
	}
}

func TestLocalModeProvenanceAndBounds(t *testing.T) {
	f := newFixture(t)
	f.reject("workspace_init", "cloud", "", "", map[string]any{"mode": "cloud"})
	f.reject("workspace_init", "implicit", "", "", nil)
	f.reject("project_create", "cloud", "", "", map[string]any{"slug": "p"})
	f.setup("w", "p")
	who := object(t, f.call("whoami", "w", "p", "lead", actor("lead", nil)))
	if who["verified"] != false || who["identity_provenance"] != "local-os-user-unverified" || who["storage_mode"] != "local" {
		t.Fatalf("misleading local identity: %v", who)
	}
	for _, n := range []int{0, -1, 1001} {
		f.reject("inbox", "w", "p", "worker", actor("worker", map[string]any{"limit": n}))
		f.reject("history", "w", "p", "", map[string]any{"limit": n})
	}
	for i, body := range []string{"", "  ", "bad\x00text", string([]byte{0xff}), fmt.Sprintf("%065537d", 1)} {
		f.reject("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": body, "operation_id": fmt.Sprintf("invalid-body-%d", i)}))
	}
	raw, _ := json.Marshal(who)
	if len(raw) == 0 {
		t.Fatal("whoami cannot encode")
	}
}

func pageItems(t *testing.T, key string, v any) []any { t.Helper(); return array(t, object(t, v)[key]) }

func TestStableCursorPagination(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	expected := []string{}
	for i := 0; i < 7; i++ {
		sent := f.send("w", "p", "lead", "worker", fmt.Sprint("page-", i), fmt.Sprint("page-", i))
		expected = append(expected, stringField(t, sent, "id"))
	}
	cursor := float64(0)
	got := []string{}
	for pageNo := 0; pageNo < 10; pageNo++ {
		page := object(t, f.call("inbox", "w", "p", "worker", actor("worker", map[string]any{"after": cursor, "limit": 2})))
		messages := pageItems(t, "messages", page)
		if len(messages) > 2 {
			t.Fatalf("limit ignored: %d", len(messages))
		}
		for _, message := range messages {
			seq := object(t, message)["sequence"].(float64)
			if seq <= cursor {
				t.Fatalf("nonincreasing cursor %v <= %v", seq, cursor)
			}
			got = append(got, stringField(t, message, "id"))
			cursor = seq
		}
		if page["next_cursor"] != cursor {
			t.Fatalf("wrong next_cursor: %v want %v", page["next_cursor"], cursor)
		}
		if pageNo == 0 {
			// Read-filter changes and concurrent insertions cannot reorder earlier rows.
			f.call("mark_read", "w", "p", "worker", actor("worker", map[string]any{"ids": []string{got[0]}}))
			extra := f.send("w", "p", "lead", "worker", "insert while paging", "page-extra")
			expected = append(expected, stringField(t, extra, "id"))
			// Only the fetched page should have been delivered.
			pending := onlyDelivery(t, f.call("message_status", "w", "p", "lead", actor("lead", map[string]any{"message_id": expected[2]})))
			if pending["delivered_at"] != nil {
				t.Fatal("fetch delivered an unreturned page")
			}
		}
		if page["has_more"] == false {
			break
		}
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("pagination skipped/duplicated/reordered messages: %v want %v", got, expected)
	}
	empty := object(t, f.call("inbox", "w", "p", "worker", actor("worker", map[string]any{"after": cursor, "limit": 2})))
	if len(pageItems(t, "messages", empty)) != 0 || empty["next_cursor"] != cursor || empty["has_more"] != false {
		t.Fatalf("empty polling cursor changed: %v", empty)
	}
	for _, kind := range []struct{ op, key string }{{"history", "events"}, {"list_sessions", "sessions"}} {
		all := pageItems(t, kind.key, f.call(kind.op, "w", "p", "", map[string]any{"limit": 1000}))
		cursor = 0
		ids := []string{}
		for pageNo := 0; pageNo < 100; pageNo++ {
			page := object(t, f.call(kind.op, "w", "p", "", map[string]any{"limit": 2, "after": cursor}))
			for _, item := range pageItems(t, kind.key, page) {
				ids = append(ids, stringField(t, item, "id"))
			}
			next := page["next_cursor"].(float64)
			if page["has_more"] == true && next <= cursor {
				t.Fatalf("%s cursor did not advance", kind.op)
			}
			cursor = next
			if page["has_more"] == false {
				break
			}
		}
		expectedIDs := []string{}
		for _, item := range all {
			expectedIDs = append(expectedIDs, stringField(t, item, "id"))
		}
		if !reflect.DeepEqual(ids, expectedIDs) {
			t.Fatalf("%s pagination mismatch %v want %v", kind.op, ids, expectedIDs)
		}
	}
}

func TestSessionResumeFencesPriorIncarnation(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	before := object(t, f.call("whoami", "w", "p", "lead", actor("lead", nil)))["session"]
	original := f.send("w", "p", "lead", "worker", "retry across resume", "resume-send")
	resumed := f.call("register_session", "w", "p", "lead", map[string]any{"instance": "lead-instance", "resume": true})
	next := stringField(t, resumed, "instance")
	if next == "lead-instance" || stringField(t, resumed, "id") != stringField(t, before, "id") {
		t.Fatalf("resume must rotate instance while preserving session: %v", resumed)
	}
	f.reject("inbox", "w", "p", "lead", actor("lead", nil))
	f.reject("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": "stale sender", "operation_id": "stale-sender"}))
	f.reject("register_session", "w", "p", "lead", map[string]any{"instance": "lead-instance", "resume": true})
	f.reject("register_session", "w", "p", "unknown", map[string]any{"instance": "missing", "resume": true})
	f.call("inbox", "w", "p", "lead", map[string]any{"instance": next})
	replay := f.call("send", "w", "p", "lead", map[string]any{"instance": next, "to": "worker", "body": "retry across resume", "operation_id": "resume-send"})
	if stringField(t, replay, "id") != stringField(t, original, "id") {
		t.Fatal("resume retry allocated a new message")
	}
	f.reject("send", "w", "p", "lead", map[string]any{"instance": next, "to": "worker", "body": "changed across resume", "operation_id": "resume-send"})
	reopened, err := bus.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result, err := reopened.Execute(context.Background(), bus.Request{Operation: "register_session", Workspace: "w", Project: "p", Session: "lead", Args: map[string]any{"instance": next}})
	if err != nil {
		t.Fatalf("explicit known incarnation failed after reopening Store: %v", err)
	}
	raw, _ := json.Marshal(result.Data)
	var registered map[string]any
	if err = json.Unmarshal(raw, &registered); err != nil {
		t.Fatal(err)
	}
	if stringField(t, registered, "id") != stringField(t, resumed, "id") {
		t.Fatal("restart changed immutable session ID")
	}
	f.call("retire_session", "w", "p", "lead", map[string]any{"instance": next, "successor": "worker"})
	f.reject("register_session", "w", "p", "lead", map[string]any{"instance": next, "resume": true})
	events := pageItems(t, "events", f.call("history", "w", "p", "", map[string]any{"entity": "session", "key": stringField(t, resumed, "id")}))
	resumeEvents := 0
	for _, event := range events {
		if object(t, event)["action"] == "resumed" {
			resumeEvents++
		}
	}
	if resumeEvents != 1 {
		t.Fatalf("resume should append one audit event, got %d", resumeEvents)
	}
}
