//go:build windows

package sessions

import (
	"errors"
	"os"
	"syscall"
)

const noFollow = 0

// On Windows privacy comes from the profile's ACL; fsprivate adds the
// explicit ACL checks when it replaces this layer.
func tightenDir(string) error { return nil }

func syncDir(string) {}

// Windows refuses to delete or rename a file while any handle to it was
// opened without FILE_SHARE_DELETE, and Go's own opens leave that flag out.
// The lock file is read by one goroutine while another releases it, so every
// open here shares read, write and delete.
const (
	errSharingViolation = syscall.Errno(32)
	errLockViolation    = syscall.Errno(33)
)

const shareAll = syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE

func openShared(path string, access, create uint32) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, access, shareAll, nil, create, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, syscall.ERROR_FILE_EXISTS) || errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
			err = os.ErrExist
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// openRead opens path for reading, sharing delete.
func openRead(path string) (*os.File, error) {
	return openShared(path, syscall.GENERIC_READ, syscall.OPEN_EXISTING)
}

// openExclusive creates path for writing, failing with os.ErrExist when it is
// there.
func openExclusive(path string) (*os.File, error) {
	return openShared(path, syscall.GENERIC_WRITE, syscall.CREATE_NEW)
}

// transient reports whether err is a sharing or access error that clears when
// the other handle closes, or while a deleted file's name is still pending.
func transient(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) ||
		errors.Is(err, errSharingViolation) ||
		errors.Is(err, errLockViolation)
}
