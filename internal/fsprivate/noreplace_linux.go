//go:build linux

package fsprivate

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return os.ErrExist
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		// An old kernel, or a file system without the flag.
		return linkNoReplace(oldpath, newpath)
	}
	return err
}
