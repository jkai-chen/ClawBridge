//go:build windows

package sessions

import "os"

// Windows reports no Unix modes; the tests treat the layer as private.
func runtimeMode(fi os.FileInfo) os.FileMode {
	if fi.IsDir() {
		return 0o700
	}
	return 0o600
}
