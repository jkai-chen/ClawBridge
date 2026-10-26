//go:build !windows

package ctxwatch

import (
	"errors"
	"os"
	"syscall"
)

func ownedByUser(info os.FileInfo) error {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != uint32(os.Getuid()) {
		return errors.New("is not owned by this user")
	}
	return nil
}

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// privateToUser: owned by this user and readable by nobody else.
func privateToUser(info os.FileInfo) error {
	if err := ownedByUser(info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("is readable by other users; it must be 0600")
	}
	return nil
}
