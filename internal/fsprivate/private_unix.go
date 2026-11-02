//go:build !windows

package fsprivate

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// CurrentUID is this user's ID; tests change it.
var CurrentUID = os.Getuid

// MakePrivate narrows path to 0700 for a directory, 0600 for a file.
func MakePrivate(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.Chmod(path, 0700)
	}
	return os.Chmod(path, 0600)
}

// CheckPrivate refuses path unless it is a plain file (dir false) or
// directory (dir true), not a symlink, owned by this user, no wider than
// 0600 or 0700, and for a file with exactly one name. It fails closed and
// names the path, never the content.
func CheckPrivate(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkInfo(path, dir, info, 1)
}

// CheckOpened is CheckPrivate for a file already open, so the file read is
// the file checked, whatever replaced the name in between.
func CheckOpened(f *os.File, path string) error { return CheckOpenedLinks(f, path, 1) }

// CheckOpenedLinks is CheckOpened allowing the file up to maxLinks names.
// Only the creation path of a key file passes more than 1, for the instant
// the link fallback of CreateExclusive holds two names.
func CheckOpenedLinks(f *os.File, path string, maxLinks int) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return checkInfo(path, false, info, maxLinks)
}

func checkInfo(path string, dir bool, info os.FileInfo, maxLinks int) error {
	limit, kind := os.FileMode(0o600), "file"
	if dir {
		limit, kind = 0o700, "directory"
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != dir || (!dir && !info.Mode().IsRegular()) {
		return fmt.Errorf("%s is not a plain %s", path, kind)
	}
	if perm := info.Mode().Perm(); perm&^limit != 0 {
		return fmt.Errorf("%s is mode %04o; it must be %04o or narrower", path, perm, limit)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot read the owner of " + path)
	}
	if int(st.Uid) != CurrentUID() {
		return fmt.Errorf("%s belongs to another user", path)
	}
	if !dir && uint64(st.Nlink) > uint64(maxLinks) {
		return fmt.Errorf("%s is hard-linked; it must have one name", path)
	}
	return nil
}

// SyncDir flushes a directory's entries to disk.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// OpenRead opens path for reading and refuses a symlink there (O_NOFOLLOW).
func OpenRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// OpenDir opens a directory for listing and refuses a symlink there.
func OpenDir(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// IsSymlinkRefusal reports whether err is the open of a symlink refused.
func IsSymlinkRefusal(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}

func renameBlocked(error) bool { return false }
