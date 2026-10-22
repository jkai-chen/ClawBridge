package mirror

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
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

func session(name, status string, sequence int) map[string]any {
	return map[string]any{"id": "ses-" + name, "name": name, "sequence": sequence, "status": status}
}

func storedViews(t *testing.T, path string) []string {
	t.Helper()
	all, e := snapshots(context.Background(), path)
	if e != nil {
		t.Fatal(e)
	}
	var out []string
	for _, s := range all {
		b := s.Binding
		out = append(out, fmt.Sprintf("%s/%s/%s/%s/%s", b.Principal, b.Project, b.Server, b.Session, b.Instance))
	}
	sort.Strings(out)
	return out
}

func TestSaveDeletesViewsNoLiveBindingCanLoad(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mirror.db")
	save := func(b Binding, sessions ...any) {
		t.Helper()
		if e := Save(ctx, path, testSnapshot(b, sessions, nil)); e != nil {
			t.Fatal(e)
		}
	}
	bob, alice, carol := session("bob", "active", 2), session("alice", "active", 3), session("carol", "active", 1)
	// Views that must survive whatever bob's project snapshot says.
	otherProject := testBinding("dave", "dave-inst")
	otherProject.Project = "q"
	otherPrincipal := testBinding("erin", "erin-inst")
	otherPrincipal.Principal = "erin@example.test"
	otherServer := testBinding("frank", "frank-inst")
	otherServer.Server = "https://other.example.test"
	for _, b := range []Binding{otherProject, otherPrincipal, otherServer} {
		save(b, session(b.Session, "active", 1))
	}
	save(testBinding("alice", "alice-inst"), bob, alice, carol)
	save(testBinding("carol", "carol-inst"), bob, alice, carol)
	save(testBinding("bob", "bob-old"), bob, alice, carol)
	// bob's instance rotated and alice retired. carol is active but registered
	// long before 1,000 more recent retired sessions; she must stay.
	sessions := []any{carol, bob, session("alice", "retired", 3)}
	for i := 0; i < 1000; i++ {
		sessions = append(sessions, session(fmt.Sprint("gone-", i), "retired", 10+i))
	}
	save(testBinding("bob", "bob-new"), sessions...)
	want := []string{
		"bob@example.test/p/https://server.example.test/bob/bob-new",
		"bob@example.test/p/https://server.example.test/carol/carol-inst",
		"bob@example.test/q/https://server.example.test/dave/dave-inst",
		"bob@example.test/p/https://other.example.test/frank/frank-inst",
		"erin@example.test/p/https://server.example.test/erin/erin-inst",
	}
	sort.Strings(want)
	if got := storedViews(t, path); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("stored views after sync:\n got %v\nwant %v", got, want)
	}
	if _, e := Load(ctx, path, testBinding("carol", "carol-inst")); e != nil {
		t.Fatal("an active session's view was deleted:", e)
	}
}

func TestSaveDeletesNothingWithoutACompleteSessionList(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mirror.db")
	bob, alice := session("bob", "active", 1), session("alice", "active", 2)
	for _, b := range []Binding{testBinding("alice", "alice-inst"), testBinding("bob", "bob-old")} {
		if e := Save(ctx, path, testSnapshot(b, []any{bob, alice}, nil)); e != nil {
			t.Fatal(e)
		}
	}
	before := storedViews(t, path)
	// No sessions list, or one that does not show the saving session as
	// active, cannot be trusted to list every active session.
	noList := testSnapshot(testBinding("bob", "bob-new"), nil, nil)
	delete(noList.Data, "sessions")
	notActive := testSnapshot(testBinding("bob", "bob-newer"), []any{session("bob", "retired", 1)}, nil)
	for _, s := range []Snapshot{noList, notActive} {
		if e := Save(ctx, path, s); e != nil {
			t.Fatal(e)
		}
	}
	got := storedViews(t, path)
	for _, view := range before {
		found := false
		for _, g := range got {
			found = found || g == view
		}
		if !found {
			t.Fatalf("a view was deleted on an untrusted session list: %s (now %v)", view, got)
		}
	}
	if len(got) != len(before)+2 {
		t.Fatal("the new views were not saved", got)
	}
}
