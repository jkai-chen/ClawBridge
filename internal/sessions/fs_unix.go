//go:build !windows

package sessions

import (
	"os"
	"syscall"
)

// noFollow makes an open fail on a symlink.
const noFollow = syscall.O_NOFOLLOW

// tightenDir makes an existing directory 0700.
func tightenDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// syncDir flushes a rename into the directory; best effort.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// openRead opens path for reading without following a symlink.
func openRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|noFollow, 0)
}

// openExclusive creates path 0600 for writing, failing with os.ErrExist when
// it is there.
func openExclusive(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600)
}

// transient reports whether err may clear by itself on a retry. Unix has no
// such errors: a file in use can still be removed or renamed.
func transient(error) bool { return false }
