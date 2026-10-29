package ctxwatch

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The queue record is data the hooks act on: only a private regular file
// naming its own session and exactly one route is read.
func TestQueueRecordIsReadOnlyWhenTrusted(t *testing.T) {
	dir := t.TempDir()
	r := QueueRecord{Session: "agent", Workspace: "w", Project: "p", Instance: "i", Server: "https://x.example.test", MCPPID: 1, AgentPID: 2, UpdatedAt: Now()}
	if err := SaveQueueRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadQueueRecord(dir, "agent")
	if err != nil || !ok || got.Instance != "i" || got.Version != 1 {
		t.Fatal(got, ok, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "agent.queue.json")); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	if _, ok, err = LoadQueueRecord(dir, "nobody"); ok || err != nil {
		t.Fatal(ok, err)
	}
	for name, bad := range map[string]QueueRecord{
		"two routes":  {Session: "agent", Instance: "i", Server: "https://x.example.test", DB: "/db"},
		"no route":    {Session: "agent", Instance: "i"},
		"no instance": {Session: "agent", Server: "https://x.example.test"},
	} {
		if err := SaveQueueRecord(dir, bad); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := LoadQueueRecord(dir, "agent"); ok || err == nil {
			t.Fatal(name)
		}
	}
	// Another session's record under this name is not this session's.
	other := r
	other.Session = "other"
	if err := SaveQueueRecord(dir, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "other.queue.json"), filepath.Join(dir, "agent.queue.json")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadQueueRecord(dir, "agent"); ok || err == nil {
		t.Fatal("a record of another session was read")
	}
	// A link is never followed.
	if err := SaveQueueRecord(dir, other); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "agent.queue.json"))
	symlink(t, filepath.Join(dir, "other.queue.json"), filepath.Join(dir, "agent.queue.json"))
	if _, ok, err := LoadQueueRecord(dir, "agent"); ok || err == nil {
		t.Fatal("a linked record was read")
	}
	if p, ok := ParentPID(os.Getpid()); !ok || p != wantParent() {
		t.Fatal(p, ok)
	}
	if _, ok := ParentPID(0); ok {
		t.Fatal("a parent for no process")
	}
}

// A record removed between the stat and the open is missing, not
// unreadable: the MCP server exited while a hook read it. A file swapped in
// there is checked as the one opened.
func TestQueueRecordRacesTheStat(t *testing.T) {
	dir := t.TempDir()
	r := QueueRecord{Session: "agent", Workspace: "w", Project: "p", Instance: "i", Server: "https://x.example.test", MCPPID: 1, AgentPID: 2, UpdatedAt: Now()}
	path := filepath.Join(dir, "agent.queue.json")
	between := func(f func()) {
		t.Cleanup(func() { afterStat = func() {} })
		afterStat = func() { afterStat = func() {}; f() }
	}
	if err := SaveQueueRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	between(func() { RemoveQueueRecord(dir, "agent", 1) })
	if _, ok, err := LoadQueueRecord(dir, "agent"); ok || err != nil {
		t.Fatal(ok, err)
	}
	if runtime.GOOS == "windows" {
		// Windows has no mode bits: a record is private by the profile's ACL,
		// so no swap for a looser mode exists to catch.
		return
	}
	if err := SaveQueueRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	between(func() {
		// The same record, but readable by others.
		other := t.TempDir()
		if err := SaveQueueRecord(other, r); err != nil {
			t.Fatal(err)
		}
		swap := filepath.Join(other, "agent.queue.json")
		if err := os.Chmod(swap, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(swap, path); err != nil {
			t.Fatal(err)
		}
	})
	if _, ok, err := LoadQueueRecord(dir, "agent"); ok || err == nil {
		t.Fatal("a file readable by others, swapped in after the stat, was read", ok, err)
	}
}
