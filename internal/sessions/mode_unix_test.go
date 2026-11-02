//go:build !windows

package sessions

import "os"

func runtimeMode(fi os.FileInfo) os.FileMode { return fi.Mode().Perm() }
