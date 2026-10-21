package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestInboxStatusDoesNotWriteAndRequiresCurrentActiveRecipient(t *testing.T) {
	h := newHarness(t)
	first := h.send("status-one")
	second := h.send("status-two")
	h.call("send", "a", map[string]any{"to": "c", "body": "not b's message", "operation_id": "other"})
	for _, table := range []string{"sessions", "deliveries"} {
		if _, err := h.s.db.Exec("CREATE TRIGGER status_no_" + table + " BEFORE UPDATE ON " + table + " BEGIN SELECT RAISE(ABORT,'status must not mutate'); END"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.s.db.Exec("CREATE TRIGGER status_no_history BEFORE INSERT ON history BEGIN SELECT RAISE(ABORT,'status must not audit'); END"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r, err := h.s.Execute(context.Background(), h.request("inbox_status", "b", map[string]any{"after": first["sequence"]}))
		if err != nil {
			t.Fatal(err)
		}
		data := r.Data.(map[string]any)
		if data["unread_count"] != int64(2) || data["new_count"] != int64(1) || data["latest_sequence"] != second["sequence"] {
			t.Fatal(data)
		}
		encoded, _ := json.Marshal(data)
		if len(data) != 6 || strings.Contains(string(encoded), "hello") || strings.Contains(string(encoded), "instance") {
			t.Fatal("status leaked extra fields", string(encoded))
		}
	}
	for _, a := range []map[string]any{{"instance": "wrong"}, {"instance": ""}, {"after": -1}, {"after": 1.5}, {"recipient": "c"}, {"unread_only": true}} {
		h.fail("inbox_status", "b", a)
	}
	h.fail("inbox_status", "", nil)
	if _, err := h.s.db.Exec("UPDATE messages SET expires_at='2000-01-01T00:00:00Z' WHERE id=?", first["id"]); err != nil {
		t.Fatal(err)
	}
	d := h.call("inbox_status", "b", nil).(map[string]any)
	if d["unread_count"] != int64(1) {
		t.Fatal(d)
	}
	if _, err := h.s.db.Exec("DROP TRIGGER status_no_sessions"); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"retired", "moved"} {
		if _, err := h.s.db.Exec("UPDATE sessions SET status=? WHERE name='b'", status); err != nil {
			t.Fatal(err)
		}
		h.fail("inbox_status", "b", nil)
	}
}
