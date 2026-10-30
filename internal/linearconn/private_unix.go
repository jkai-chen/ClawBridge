//go:build !windows

package linearconn

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func makePrivate(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.Chmod(path, 0700)
	}
	return os.Chmod(path, 0600)
}
func checkPrivate(path string, info os.FileInfo) error {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 || (!info.IsDir() && s.Nlink != 1) {
		return errors.New("Linear profile storage must be private, owned by this user and not hard-linked")
	}
	return nil
}
func syncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// currentUID is this user's ID; tests change it.
var currentUID = os.Getuid

// checkFile refuses a connection file wider than 0600, a directory wider
// than 0700, a symlink, or either owned by another user, as the device
// key's file is refused.
func checkFile(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !dir {
			return errNoFile
		}
		return fmt.Errorf("cannot read the Linear connection's file mode: %w", err)
	}
	limit, kind := os.FileMode(0o600), "file"
	if dir {
		limit, kind = 0o700, "directory"
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != dir {
		return fmt.Errorf("the Linear connection %s %s is not a plain %s", kind, path, kind)
	}
	if perm := info.Mode().Perm(); perm&^limit != 0 {
		return fmt.Errorf("the Linear connection %s %s is mode %04o; it must be %04o or narrower (chmod %o %s)", kind, path, perm, limit, limit, path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != currentUID() {
		return fmt.Errorf("the Linear connection %s %s belongs to another user", kind, path)
	} else if ok && !dir && st.Nlink != 1 {
		return fmt.Errorf("the Linear connection file %s is hard-linked; it must have one name", path)
	}
	return nil
}

// openNoFollow refuses to open a symlink.
const openNoFollow = syscall.O_NOFOLLOW
