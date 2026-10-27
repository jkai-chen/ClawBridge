//go:build !windows

package ctxwatch

import (
	"errors"
	"os"
	"syscall"
)

// TryLock takes an exclusive lock on path without waiting, creating the
// file 0600 if needed. It reports false when another process holds it. The
// lock lasts until the file is closed or the process exits.
func TryLock(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}
