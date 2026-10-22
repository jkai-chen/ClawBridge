package linearconn

import (
	"errors"
	"os"
)

func privatePath(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("cannot inspect private Linear profile storage")
	}
	if info.Mode()&os.ModeSymlink != 0 || (dir && !info.IsDir()) || (!dir && !info.Mode().IsRegular()) {
		return errors.New("Linear profile storage must be private and not symlinked")
	}
	if err = checkPrivate(path, info); err != nil {
		return errors.New("Linear profile storage requires private ownership and permissions")
	}
	return nil
}
