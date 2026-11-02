package linearconn

import (
	"errors"
	"fmt"
	"os"

	"clawbridge/internal/fsprivate"
)

func makePrivate(path string) error { return fsprivate.MakePrivate(path) }
func checkPrivate(path string, info os.FileInfo) error {
	return fsprivate.CheckPrivate(path, info.IsDir())
}
func syncDirectory(path string) error { return fsprivate.SyncDir(path) }

// checkFile refuses a connection file or directory that is a symlink, or
// whose ACL is not this user's alone: POSIX modes don't apply here.
func checkFile(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !dir {
			return errNoFile
		}
		return fmt.Errorf("cannot read the Linear connection's file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != dir {
		return fmt.Errorf("the Linear connection path %s is not a plain file or directory", path)
	}
	if err = checkPrivate(path, info); err != nil {
		return fmt.Errorf("the Linear connection path %s must be private to this user: %w", path, err)
	}
	return nil
}

// openNoFollow is unused on Windows: checkFile refuses a symlink, and the
// opened file is compared with the checked one.
const openNoFollow = 0
