//go:build !windows

package webui

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

var errLocked = errors.New("another clawbridge ui holds the lock")

// lockFile takes an exclusive lock on path that the OS drops when the
// process ends. The file is never removed, so two processes cannot lock
// different files of one name.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// checkPrivateDir makes dir a 0700 directory of this user, or fails.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
		if fi, err = os.Lstat(dir); err != nil {
			return err
		}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is not private (mode %o)", dir, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by this user", dir)
	}
	return nil
}
