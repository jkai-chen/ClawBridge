package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/mirror"
)

func mirrorFixture(t *testing.T) (string, mirror.Snapshot) {
	t.Helper()
	now := time.Now().UTC()
	b := mirror.Binding{Server: "https://server.example.test", Audience: "https://bus.example.test", Principal: pgPrincipal("bob"), Workspace: "ws", Project: "p", Session: "bob", Instance: "known-instance"}
	data := map[string]any{"workspace": map[string]any{"id": "ws"}, "project": map[string]any{"id": "p"}, "session": map[string]any{"id": "ses-bob", "name": "bob", "instance": "known-instance"}, "principal": b.Principal, "verified": true, "identity_provenance": "google-id-token", "storage_mode": "cloud", "snapshot_at": now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		"sessions": []any{map[string]any{"id": "ses-bob", "name": "bob", "sequence": 1, "role": "worker", "status": "active"}}, "events": []any{},
		"messages":      []any{map[string]any{"id": "fresh", "sequence": 1, "body": "fresh body", "expires_at": now.Add(time.Hour).Format(time.RFC3339Nano), "delivered_at": nil, "read_at": nil}, map[string]any{"id": "expired", "sequence": 2, "body": "expired body", "expires_at": now.Add(-time.Hour).Format(time.RFC3339Nano), "delivered_at": nil, "read_at": nil}},
		"sent_messages": []any{map[string]any{"id": "expired-sent", "sequence": 3, "expires_at": now.Add(-time.Hour).Format(time.RFC3339Nano), "deliveries": []any{}}}}
	return filepath.Join(t.TempDir(), "mirror.db"), mirror.Snapshot{Binding: b, UpdatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339Nano), Data: data, Selections: []mirror.Selection{{Workspace: "team", Project: "slug"}}}
}
func TestMirrorReadOnlyBindingAndAtomicPublication(t *testing.T) {
	path, snapshot := mirrorFixture(t)
	ctx := context.Background()
	if err := mirror.Save(ctx, path, snapshot); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	request := bus.Request{Operation: "inbox", Workspace: "ws", Project: "p", Session: "bob", Args: map[string]any{"instance": "known-instance"}}
	result, err := mirror.Offline(ctx, path, snapshot.Binding, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source != "offline-mirror" {
		t.Fatal("wrong mirror source")
	}
	data := jsonData(t, result.Data)
	messages := pageItems(t, "messages", data)
	if len(messages) != 1 || object(t, messages[0])["id"] != "fresh" {
		t.Fatalf("mirror expiry wrong: %v", messages)
	}
	if object(t, messages[0])["delivered_at"] != nil || object(t, messages[0])["read_at"] != nil {
		t.Fatal("mirror stamps changed")
	}
	for _, op := range []string{"send", "reply", "mark_read", "register_session", "update_session", "retire_session", "workspace_init", "project_role_set"} {
		r := request
		r.Operation = op
		if _, err := mirror.Offline(ctx, path, snapshot.Binding, r); err == nil {
			t.Errorf("mirror accepted write %s", op)
		}
	}
	bad := snapshot
	bad.UpdatedAt = "invalid"
	if err := mirror.Save(ctx, path, bad); err == nil {
		t.Fatal("invalid snapshot replaced previous view")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("mirror reads or failed publication changed bytes")
	}
	for _, field := range []string{"server", "audience", "principal", "workspace", "project", "session", "instance"} {
		b := snapshot.Binding
		switch field {
		case "server":
			b.Server = "https://other.example.test"
		case "audience":
			b.Audience = "other"
		case "principal":
			b.Principal = pgPrincipal("alice")
		case "workspace":
			b.Workspace = "other"
		case "project":
			b.Project = "other"
		case "session":
			b.Session = "alice"
		case "instance":
			b.Instance = "other"
		}
		if _, err := mirror.Load(ctx, path, b); err == nil {
			t.Errorf("binding mismatch %s accepted", field)
		}
	}
	emptyPath := filepath.Join(t.TempDir(), "missing.db")
	if _, err := mirror.Load(ctx, emptyPath, snapshot.Binding); err == nil {
		t.Fatal("missing mirror accepted")
	}
	if _, err := os.Stat(emptyPath); !os.IsNotExist(err) {
		t.Fatal("read-only mirror load created database")
	}
}
func TestMirrorExpiredSentStatus(t *testing.T) {
	path, snapshot := mirrorFixture(t)
	ctx := context.Background()
	if err := mirror.Save(ctx, path, snapshot); err != nil {
		t.Fatal(err)
	}
	request := bus.Request{Operation: "message_status", Workspace: "ws", Project: "p", Session: "bob", Args: map[string]any{"instance": "known-instance", "message_id": "expired-sent"}}
	if _, err := mirror.Offline(ctx, path, snapshot.Binding, request); err == nil {
		t.Fatal("offline status returned a message expired after snapshot acquisition")
	}
}
