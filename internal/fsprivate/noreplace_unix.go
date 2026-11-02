//go:build !windows

package fsprivate

import (
	"errors"
	"os"
)

// linkNoReplace publishes oldpath at newpath without replacing: link fails
// if newpath exists. Once the temporary name is unlinked the file must have
// one name again, which is checked, so a hard link planted meanwhile is not
// taken for the file's own second name.
func linkNoReplace(oldpath, newpath string) error {
	if err := os.Link(oldpath, newpath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return err
	}
	if err := os.Remove(oldpath); err != nil {
		return err
	}
	return CheckPrivate(newpath, false)
}
