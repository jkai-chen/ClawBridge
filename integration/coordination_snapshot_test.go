package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/mirror"
)

func TestCoordinationPostgresSnapshotMirrorAndPurge(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p", "other")
	c := coordinationObject("opaque-snapshot-9", "ready")
	args := actor("alice", map[string]any{"to": "bob", "body": "participant-only context", "operation_id": "snapshot-context", "coordination": c})
	sent := f.call(pgPrincipal("alice"), "send", "p", "alice", args)
	id := stringField(t, sent, "id")
	private := coordinationObject("different-private-context", "blocked")
	f.call(pgPrincipal("bob"), "send", "p", "bob", actor("bob", map[string]any{"to": "reader", "body": "unrelated private body", "operation_id": "private-context", "coordination": private}))
	for _, name := range []string{"alice", "bob"} {
		t.Run(name, func(t *testing.T) {
			data := object(t, f.call(pgPrincipal(name), "snapshot", "p", name, actor(name, nil)))
			key := "sent_messages"
			if name == "bob" {
				key = "messages"
			}
			items := data[key].([]any)
			found := false
			for _, item := range items {
				m := object(t, item)
				if m["id"] == id {
					found = true
					if !reflect.DeepEqual(m["coordination"], c) {
						t.Fatal("snapshot lost canonical participant context")
					}
				}
			}
			if !found {
				t.Fatal("own message missing from authorized snapshot")
			}
			if name == "alice" {
				raw, _ := json.Marshal(data)
				if strings.Contains(string(raw), "different-private-context") || strings.Contains(string(raw), "unrelated private body") {
					t.Fatal("snapshot leaks unrelated participant context")
				}
			}
			binding := mirror.Binding{Server: "https://synthetic-review.example.test", Audience: "synthetic-audience", Principal: pgPrincipal(name), Workspace: stringField(t, data["workspace"], "id"), Project: stringField(t, data["project"], "id"), Session: stringField(t, data["session"], "id"), Instance: name + "-instance"}
			path := filepath.Join(t.TempDir(), "mirror.db")
			if err := mirror.Save(context.Background(), path, mirror.Snapshot{Binding: binding, UpdatedAt: stringField(t, data, "snapshot_at"), Data: data}); err != nil {
				t.Fatal(err)
			}
			op := "message_status"
			query := map[string]any{"instance": binding.Instance, "message_id": id}
			if name == "bob" {
				op = "inbox"
				delete(query, "message_id")
			}
			result, err := mirror.Offline(context.Background(), path, binding, bus.Request{Operation: op, Workspace: binding.Workspace, Project: binding.Project, Session: binding.Session, Args: query})
			if err != nil {
				t.Fatal(err)
			}
			if result.Source != "offline-mirror" {
				t.Fatal("mirror misstates authority")
			}
			m := object(t, jsonData(t, result.Data))
			if name == "bob" {
				messages := pageItems(t, "messages", m)
				if len(messages) != 1 {
					t.Fatal("mirror recipient messages not scoped")
				}
				m = object(t, messages[0])
			}
			if !reflect.DeepEqual(m["coordination"], c) {
				t.Fatal("mirror lost sender/recipient metadata")
			}
			for _, op := range []string{"inbox_status", "notification_status"} {
				if mirror.Readable(op) {
					t.Fatal("mirror advertises wake status")
				}
			}
		})
	}
	// Broad project audit is not a participant-context channel.
	events := f.call(pgPrincipal("reader"), "history", "p", "reader", actor("reader", nil))
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "opaque-snapshot-9") || strings.Contains(string(raw), coordinationIssue) || strings.Contains(string(raw), "different-private-context") {
		t.Fatal("audit exposes participant coordination")
	}
	db, err := sql.Open("pgx", os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var digest string
	if err = db.QueryRow("SELECT payload_hash FROM messages WHERE id=$1", id).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE messages SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	f.call(pgOwner, "purge_expired", "p", "owner", actor("owner", nil))
	var body, actualHash string
	var coordination sql.NullString
	if err = db.QueryRow("SELECT body,coordination,payload_hash FROM messages WHERE id=$1", id).Scan(&body, &coordination, &actualHash); err != nil {
		t.Fatal(err)
	}
	if body != "" || coordination.Valid || actualHash != digest {
		t.Fatal("purge failed to clear context or preserve retry tombstone")
	}
	retry, retryErr := f.store.ExecuteAs(context.Background(), pgPrincipal("alice"), f.request("send", "p", "alice", args))
	if retryErr == nil {
		tombstone := object(t, jsonData(t, retry.Data))
		if tombstone["id"] != id || tombstone["body"] != "" || tombstone["coordination"] != nil {
			t.Fatal("expired retry disclosed purged context or created fresh message")
		}
	}
	var count int
	db.QueryRow("SELECT COUNT(*) FROM messages WHERE id=$1", id).Scan(&count)
	if count != 1 {
		t.Fatal("expired retry removed/duplicated tombstone")
	}
	ack := coordinationObject("opaque-snapshot-9", "accept")
	ack["request_id"] = id
	f.reject(pgPrincipal("bob"), "reply", "p", "bob", actor("bob", map[string]any{"message_id": id, "body": "expired acknowledgement", "operation_id": "expired-ack", "coordination": ack}))
}
