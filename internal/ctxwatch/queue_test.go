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
	if runtime.GOOS == "windows" {
		t.Skip("the hooks are not installed on Windows")
	}
	dir := t.TempDir()
	r := QueueRecord{Session: "agent", Workspace: "w", Project: "p", Instance: "i", Server: "https://x.example.test", MCPPID: 1, AgentPID: 2, UpdatedAt: Now()}
	if err := SaveQueueRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadQueueRecord(dir, "agent")
	if err != nil || !ok || got.Instance != "i" || got.Version != 1 {
		t.Fatal(got, ok, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "agent.queue.json")); info.Mode().Perm() != 0o600 {
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
	if err := os.Symlink(filepath.Join(dir, "other.queue.json"), filepath.Join(dir, "agent.queue.json")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadQueueRecord(dir, "agent"); ok || err == nil {
		t.Fatal("a linked record was read")
	}
	if p, ok := ParentPID(os.Getpid()); !ok || p != os.Getppid() {
		t.Fatal(p, ok)
	}
	if _, ok := ParentPID(0); ok {
		t.Fatal("a parent for no process")
	}
}
