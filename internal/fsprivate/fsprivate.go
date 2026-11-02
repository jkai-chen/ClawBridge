// Package fsprivate holds the file-system primitives for storage only its
// owner may read: a private mode or ACL, an atomic replace that syncs, a
// refusal of WSL's Windows drives, and opens that never follow a link. The
// Linear connection store and the sealed key store share them.
package fsprivate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OnWindowsDrive reports whether path, through any symlink in it, is under
// drives, where WSL mounts Windows' drives and other mounts that can't hold
// private files. It is never true when onWindows says the program itself
// runs on Windows.
func OnWindowsDrive(path, drives string, onWindows bool) bool {
	if onWindows {
		return false
	}
	path = filepath.Clean(path)
	// The nearest existing ancestor is resolved: the store's own
	// directories may not be made yet.
	resolved, rest := path, ""
	for p := path; ; {
		if r, e := filepath.EvalSymlinks(p); e == nil {
			resolved = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		rest, p = filepath.Join(filepath.Base(p), rest), parent
	}
	return strings.HasPrefix(resolved+string(filepath.Separator), drives)
}

// The steps of an atomic write, for the caller to word a failure by.
const (
	StageCreate  = "create"  // the temporary file could not be made
	StageProtect = "protect" // it could not be made private
	StageVerify  = "verify"  // it was not private once made
	StageWrite   = "write"   // writing, syncing or closing it failed
	StageRename  = "rename"  // moving it into place failed
	StageSync    = "sync"    // the directory could not be synced
)

// WriteError is a failed atomic write. It names a step, never the data.
type WriteError struct {
	Stage string
	Err   error
}

func (e *WriteError) Error() string { return "atomic write failed at " + e.Stage + " step" }
func (e *WriteError) Unwrap() error { return e.Err }

// WriteAtomic replaces dir/name with data through a private temporary file
// named from prefix: it is written, synced and renamed into place, and the
// directory is synced. A reader sees the old file or the new one, never a
// part. A failure leaves the old file and no temporary file.
func WriteAtomic(dir, prefix, name string, data []byte) error {
	f, err := os.CreateTemp(dir, prefix+"*")
	if err != nil {
		return &WriteError{StageCreate, err}
	}
	defer os.Remove(f.Name())
	if err = MakePrivate(f.Name()); err != nil {
		f.Close()
		return &WriteError{StageProtect, err}
	}
	if err = CheckPrivate(f.Name(), false); err != nil {
		f.Close()
		return &WriteError{StageVerify, err}
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return &WriteError{StageWrite, err}
	}
	if err = Rename(f.Name(), filepath.Join(dir, name)); err != nil {
		return &WriteError{StageRename, err}
	}
	if err = SyncDir(dir); err != nil {
		return &WriteError{StageSync, err}
	}
	return nil
}

// CreateExclusive makes path with data, owner-only, only if nothing is
// there. The data is staged in a private temporary file beside path,
// synced, and published by a rename that never replaces (renameat2 with
// RENAME_NOREPLACE on Linux, renamex_np with RENAME_EXCL on macOS,
// MoveFileEx without MOVEFILE_REPLACE_EXISTING on Windows; elsewhere, or
// where the file system lacks it, a hard link and an unlink). The name
// appears whole or not at all, so a reader never sees a part and a crash
// leaves no short file. If something is there already, the caller lost a
// race: it gets an error for which errors.Is(err, os.ErrExist) holds, and
// nothing changes.
func CreateExclusive(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return &WriteError{StageCreate, err}
	}
	defer os.Remove(f.Name())
	if err = MakePrivate(f.Name()); err != nil {
		f.Close()
		return &WriteError{StageProtect, err}
	}
	if err = CheckPrivate(f.Name(), false); err != nil {
		f.Close()
		return &WriteError{StageVerify, err}
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return &WriteError{StageWrite, err}
	}
	if err = renameNoReplace(f.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return &WriteError{StageRename, err}
	}
	if err = SyncDir(dir); err != nil {
		return &WriteError{StageSync, err}
	}
	return nil
}

// Remove deletes path, retrying briefly while another process has it open
// (Windows). A path that is not there is os.ErrNotExist.
func Remove(path string) error {
	return renameRetry(func() error { return os.Remove(path) }, renameBlocked, time.Sleep)
}

// Retry schedule for a rename that another process blocks (Windows):
// about ten tries over one second.
const (
	renameTries = 10
	renameDelay = 100 * time.Millisecond
)

// renameRetry calls rename until it succeeds, fails with an error retryable
// does not accept, or the tries run out, pausing with sleep between tries.
func renameRetry(rename func() error, retryable func(error) bool, sleep func(time.Duration)) error {
	var err error
	for i := 0; i < renameTries; i++ {
		if err = rename(); err == nil || !retryable(err) {
			return err
		}
		if i < renameTries-1 {
			sleep(renameDelay)
		}
	}
	return err
}

// Rename replaces newpath with oldpath. On Windows it retries briefly while
// a reader holds newpath open; elsewhere it is os.Rename.
func Rename(oldpath, newpath string) error {
	return renameRetry(func() error { return os.Rename(oldpath, newpath) }, renameBlocked, time.Sleep)
}
