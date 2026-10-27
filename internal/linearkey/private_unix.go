//go:build !windows

package linearkey

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// checkPrivate refuses a key file wider than 0600, a directory wider than
// 0700, a symlink, or either owned by another user.
func checkPrivate(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !dir {
			return ErrNoKey
		}
		return errors.New("cannot read the Linear API key's file mode")
	}
	limit, kind := os.FileMode(0o600), "file"
	if dir {
		limit, kind = 0o700, "directory"
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != dir {
		return fmt.Errorf("the Linear API key %s %s is not a plain %s", kind, path, kind)
	}
	if perm := info.Mode().Perm(); perm&^limit != 0 {
		return fmt.Errorf("the Linear API key %s %s is mode %04o; it must be %04o or narrower (chmod %o %s)", kind, path, perm, limit, limit, path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("the Linear API key %s %s belongs to another user", kind, path)
	}
	return nil
}
