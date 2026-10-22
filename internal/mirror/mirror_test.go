package mirror

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

func testBinding(session, instance string) Binding {
	return Binding{Server: "https://server.example.test", Audience: "https://bus.example.test", Principal: "bob@example.test", Workspace: "ws", Project: "p", Session: session, Instance: instance}
}

func testSnapshot(b Binding, sessions []any, extra map[string]any) Snapshot {
	data := map[string]any{"workspace": map[string]any{"id": "ws"}, "project": map[string]any{"id": "p"}, "session": map[string]any{"id": "ses-" + b.Session, "name": b.Session}, "sessions": sessions, "events": []any{map[string]any{"sequence": 7, "entity": "message", "entity_key": "m1"}}, "messages": []any{}, "sent_messages": []any{}}
	for k, v := range extra {
		data[k] = v
	}
	return Snapshot{Binding: b, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Data: data}
}

func TestOfflineReadsSayWhenTheSnapshotWindowIsPartial(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mirror.db")
	b := testBinding("bob", "bob-inst")
	sessions := []any{map[string]any{"id": "ses-bob", "name": "bob", "sequence": 1, "status": "active"}}
	if e := Save(ctx, path, testSnapshot(b, sessions, map[string]any{"events_truncated": true, "sessions_truncated": false})); e != nil {
		t.Fatal(e)
	}
	for op, want := range map[string]bool{"history": true, "list_sessions": false} {
		r, e := Offline(ctx, path, b, bus.Request{Operation: op, Workspace: "ws", Project: "p", Session: "bob", Args: map[string]any{"instance": "bob-inst"}})
		if e != nil {
			t.Fatal(op, e)
		}
		if got := r.Data.(map[string]any)["truncated"]; got != want {
			t.Fatalf("%s truncated = %v, want %v", op, got, want)
		}
	}
	// A snapshot from an older server carries no flag; it reads as complete.
	if e := Save(ctx, path, testSnapshot(b, sessions, nil)); e != nil {
		t.Fatal(e)
	}
	r, e := Offline(ctx, path, b, bus.Request{Operation: "history", Workspace: "ws", Project: "p", Session: "bob", Args: map[string]any{"instance": "bob-inst"}})
	if e != nil || r.Data.(map[string]any)["truncated"] != false {
		t.Fatal("a snapshot without the flag must read as untruncated", e)
	}
}
