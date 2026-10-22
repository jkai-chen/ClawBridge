//go:build !windows

package linearboard

import (
	"errors"
	"os"
	"path/filepath"
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
	if !ok || s.Uid != uint32(os.Getuid()) || (!info.IsDir() && (s.Nlink != 1 || info.Mode().Perm()&0077 != 0)) {
		return errors.New("operation ledger must be private, owned by this user and not hard-linked")
	}
	if info.IsDir() {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		path = resolved
		// An owned directory inside an owned 0700 ancestor is also private;
		// Go test temp subdirectories and real app state commonly use this layout.
		for {
			st, err := os.Stat(path)
			if err != nil {
				return err
			}
			owner, ok := st.Sys().(*syscall.Stat_t)
			if ok && owner.Uid == uint32(os.Getuid()) && st.Mode().Perm()&0077 == 0 {
				return nil
			}
			parent := filepath.Dir(path)
			if parent == path {
				return errors.New("operation ledger directory requires a private ancestor")
			}
			path = parent
		}
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
