//go:build windows

package webui

import (
	"errors"
	"os"
	"syscall"
)

// errSharingViolation is ERROR_SHARING_VIOLATION.
var errSharingViolation = syscall.Errno(32)

var errLocked = errors.New("another clawbridge ui holds the lock")

// lockFile opens path with no sharing, so a second process cannot open it
// until this one closes the handle or ends.
func lockFile(path string) (func(), error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { syscall.CloseHandle(h) }, nil
}

// checkPrivateDir only checks that dir is a directory: Windows keeps the
// per-user profile private by ACL, and has no mode bits to check.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New(dir + " is not a directory")
	}
	return nil
}
