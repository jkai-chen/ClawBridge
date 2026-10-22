package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func coord(intent string) map[string]any {
	return map[string]any{"issue_id": "11111111-2222-3333-4444-555555555555", "assignment_revision": "rev-7", "intent": intent}
}
func TestCoordinationMetadataAndCorrelation(t *testing.T) {
	h := newHarness(t)
	c := coord("request")
	req := h.call("send", "a", map[string]any{"to": "b", "body": "question", "operation_id": "request", "coordination": c}).(map[string]any)
	if !reflect.DeepEqual(req["coordination"], c) {
		t.Fatal(req)
	}
	c = coord("accept")
	c["request_id"] = req["id"]
	args := map[string]any{"message_id": req["id"], "body": "yes", "operation_id": "ack", "coordination": c}
	ack := h.call("reply", "b", args).(map[string]any)
	if ack["reply_to"] != req["id"] || !reflect.DeepEqual(ack["coordination"], c) {
		t.Fatal(ack)
	}
	// Same semantic object and retry ID retains the original acknowledgement.
	retry := h.call("reply", "b", args).(map[string]any)
	if retry["id"] != ack["id"] {
		t.Fatal("retry changed message")
	}
	c["assignment_revision"] = "rev-8"
	args["operation_id"] = "mismatch"
	h.fail("reply", "b", args)
	c["assignment_revision"] = "rev-7"
	args["operation_id"] = "invisible"
	h.fail("reply", "c", args)
	inbox := h.call("inbox", "b", nil).(map[string]any)["messages"].([]map[string]any)
	if len(inbox) != 1 || inbox[0]["coordination"] == nil {
		t.Fatal(inbox)
	}
}
func TestCoordinationRejectsMalformedPresentMetadata(t *testing.T) {
	for _, field := range []string{"issue_id", "assignment_revision", "intent", "request_id", "source_commit"} {
		for _, bad := range []any{nil, "", 42, false, map[string]any{}} {
			c := coord("request")
			c[field] = bad
			if _, err := ParseCoordination(c); err == nil {
				t.Fatalf("accepted %s=%v", field, bad)
			}
		}
	}
	for _, bad := range []any{nil, map[string]any{}, map[string]any{"extra": "field"}, []any{}, "text"} {
		if _, err := ParseCoordination(bad); err == nil {
			t.Fatal(bad)
		}
	}
	c := coord("request")
	c["assignment_revision"] = "bad\nrevision"
	if _, err := ParseCoordination(c); err == nil {
		t.Fatal("control revision")
	}
	if _, err := ParseCoordination(coord("accept")); err == nil {
		t.Fatal("uncorrelated ack")
	}
}
func TestCoordinationVersionOneMigrationPreservesLegacyRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	// Fixture is a genuine v1 schema, populated through the old digest shape.
	if _, err = db.Exec(`INSERT INTO workspaces VALUES('w','w','local',90,'owner','time'); INSERT INTO projects VALUES('p','w','p','p','','time');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 3 {
		t.Fatal(version, err)
	}
	// Local identity is assigned on open; keep fixture ownership aligned.
	if _, err = s.db.Exec(`UPDATE workspaces SET owner_principal=?`, s.principal); err != nil {
		t.Fatal(err)
	}
	h := &harness{t, s}
	h.call("register_session", "a", map[string]any{"instance": "a-instance"})
	h.call("register_session", "b", map[string]any{"instance": "b-instance"})
	sent := h.send("legacy")
	var digest string
	if err = s.db.QueryRow(`SELECT payload_hash FROM messages WHERE id=?`, sent["id"]).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	want := hashPayload(map[string]any{"operation": "send", "to": "b", "body": "hello", "kind": "message", "thread_id": "", "message_id": ""})
	if digest != want {
		t.Fatal("legacy digest changed")
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h.s = s
	retry := h.send("legacy")
	if retry["id"] != sent["id"] {
		t.Fatal("legacy retry changed")
	}
	if _, ok := retry["coordination"]; ok {
		t.Fatal("legacy field not omitted")
	}
}
func TestCoordinationOwnPolicyAndNoStampStatus(t *testing.T) {
	h := newHarness(t)
	h.send("hello")
	h.call("set_notification_policy", "b", map[string]any{"policy": "hold"})
	before, err := queryForTest(h.s)
	if err != nil {
		t.Fatal(err)
	}
	status := h.call("notification_status", "b", nil).(map[string]any)
	if len(status) != 7 || status["policy"] != "hold" {
		t.Fatal(status)
	}
	plain := h.call("inbox_status", "b", nil).(map[string]any)
	if len(plain) != 6 {
		t.Fatal(plain)
	}
	after, err := queryForTest(h.s)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("metadata wrote authority", err)
	}
	h.fail("set_notification_policy", "b", map[string]any{"policy": "accept", "target_session": "a"})
	h.call("register_session", "b", map[string]any{"instance": "b-instance", "resume": true})
	var policy string
	if err = h.s.db.QueryRow(`SELECT notification_policy FROM sessions WHERE name='b'`).Scan(&policy); err != nil || policy != "hold" {
		t.Fatal(policy, err)
	}
}
func queryForTest(s *Store) ([]byte, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := map[string]any{}
	for _, table := range []string{"sessions", "deliveries", "history"} {
		rows, e := query(context.Background(), &transaction{Tx: tx}, "SELECT * FROM "+table)
		if e != nil {
			return nil, e
		}
		out[table] = rows
	}
	return json.Marshal(out)
}
