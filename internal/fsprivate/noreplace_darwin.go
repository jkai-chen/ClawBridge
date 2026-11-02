//go:build darwin

package fsprivate

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplace(oldpath, newpath string) error {
	err := unix.RenamexNp(oldpath, newpath, unix.RENAME_EXCL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return os.ErrExist
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EINVAL):
		// A file system without exclusive rename.
		return linkNoReplace(oldpath, newpath)
	}
	return err
}
