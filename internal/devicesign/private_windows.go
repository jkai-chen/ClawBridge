//go:build windows

package devicesign

import (
	"errors"
	"fmt"
	"os"
)

// checkPrivate on Windows checks only that the path is a plain file or
// directory: POSIX modes don't apply, and the profile directory's ACL is
// what protects it.
func checkPrivate(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !dir {
			return ErrNoKey
		}
		return fmt.Errorf("cannot read the device key's mode: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != dir {
		return fmt.Errorf("the device key path %s is not a plain file or directory", path)
	}
	return nil
}
