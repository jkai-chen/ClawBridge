//go:build !windows

package ctxwatch

import (
	"errors"
	"fmt"
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
func privateToUser(_ string, info os.FileInfo) error {
	if err := ownedByUser(info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("is readable by other users; it must be 0600")
	}
	return nil
}

// openedPrivate checks the opened file by its own stat, info.
func openedPrivate(_ *os.File, path string, info os.FileInfo) error {
	return privateToUser(path, info)
}

// On Unix the temp file's Chmod(0600) makes a record private; the context
// directory is created 0700 and an existing one is left as it is.
func makePrivate(string) error { return nil }

// MakeContextDir creates the context directory dir, 0700.
func MakeContextDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the context directory %s could not be created", dir)
	}
	return nil
}

// Writers on Unix keep their rules: a binding must already be private to be
// replaced; the other records are replaced by the rename whatever they were.
func bindingReplaceable(path string, info os.FileInfo) error { return privateToUser(path, info) }

func recordReplaceable(string, os.FileInfo) error { return nil }

// ProcessAlive says whether a process with pid exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
