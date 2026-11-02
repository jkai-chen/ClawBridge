//go:build windows

package sessions

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A goroutine reading the lock file must not stop another from releasing it:
// the read shares delete, so the remove goes through, and a taker can then
// create the lock again.
func TestReleaseWhileLockFileIsOpen(t *testing.T) {
	dir := testRecordsDir(t)
	path := filepath.Join(dir, ".lock")
	err := WithProjectLock(dir, func() error {
		f, err := openRead(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return releaseLock(path, readToken(t, path))
	})
	if err != nil {
		t.Fatalf("release with the file open for reading: %v", err)
	}
	// The name may linger until the reader closes; a new lock still comes.
	if err := WithProjectLock(dir, func() error { return nil }); err != nil {
		t.Fatalf("take the lock again: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

// A reader that holds the file for a short while delays the release but does
// not fail it.
func TestReleaseRetriesPastAPlainOpen(t *testing.T) {
	dir := testRecordsDir(t)
	path := filepath.Join(dir, ".lock")
	if err := fsys.MkdirPrivate(dir); err != nil {
		t.Fatal(err)
	}
	if err := fsys.CreateExclusive(path, []byte(`{"pid":1,"token":"t"}`)); err != nil {
		t.Fatal(err)
	}
	// os.Open leaves out FILE_SHARE_DELETE, so the remove must wait.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.Close()
	}()
	if err := releaseLock(path, "t"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("lock left behind: %v", err)
	}
}

func readToken(t *testing.T, path string) string {
	t.Helper()
	h, _, ok, err := readHolder(path)
	if err != nil || !ok {
		t.Fatalf("read holder: %v %v", ok, err)
	}
	return h.Token
}
