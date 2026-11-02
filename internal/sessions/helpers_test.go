package sessions

import (
	"fmt"
	"sync"
	"testing"
)

// fakeProcs overrides the process table for chosen PIDs; any other PID (the
// test process itself, for the lock) is looked up for real.
type fakeProcs struct {
	mu      sync.Mutex
	procs   map[int]int64 // pid -> start
	unknown map[int]bool
	parents map[int]int
}

func newFakeProcs(t *testing.T) *fakeProcs {
	t.Helper()
	f := &fakeProcs{procs: map[int]int64{}, unknown: map[int]bool{}, parents: map[int]int{}}
	oldLookup, oldParent := lookupProc, parentPID
	lookupProc = func(pid int) (int64, procState) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.unknown[pid] {
			return 0, procUnknown
		}
		if s, ok := f.procs[pid]; ok {
			return s, procRunning
		}
		if pid >= fakeBase {
			return 0, procGone
		}
		return oldLookup(pid)
	}
	parentPID = func(pid int) (int, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if p, ok := f.parents[pid]; ok {
			return p, true
		}
		return oldParent(pid)
	}
	t.Cleanup(func() { lookupProc, parentPID = oldLookup, oldParent })
	return f
}

// fakeBase keeps fake PIDs apart from real ones.
const fakeBase = 4_000_000

func (f *fakeProcs) add(pid int, start int64) ProcID {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[pid] = start
	return ProcID{PID: pid, Start: start}
}

func (f *fakeProcs) kill(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.procs, pid)
}

func (f *fakeProcs) restart(pid int, start int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[pid] = start
}

// testConfig points the per-user config dir at a temp dir.
func testConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := userConfigDir
	userConfigDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { userConfigDir = old })
	return dir
}

// testRecordsDir is a records dir shaped like the real one.
func testRecordsDir(t *testing.T) string {
	t.Helper()
	dir, err := RecordsDir(t.TempDir(), "ws", "proj")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func convID(n int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", n) }

func formatInt(n int) string { return fmt.Sprintf("%d", n) }
