package linearboard

import (
	"errors"
	"os"
)

func privatePath(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("cannot inspect private operation ledger")
	}
	if info.Mode()&os.ModeSymlink != 0 || (dir && !info.IsDir()) || (!dir && !info.Mode().IsRegular()) {
		return errors.New("operation ledger must be private and not symlinked")
	}
	if err = checkPrivate(path, info); err != nil {
		return errors.New("operation ledger requires private ownership and permissions")
	}
	return nil
}
