//go:build !windows

package linearconn

import (
	"errors"
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
