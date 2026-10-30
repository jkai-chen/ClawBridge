//go:build !windows

package channel

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// An administrator's umask (sudo may keep 077) doesn't hide the drop-in or
// its folders from the machine's other Claude Code users.
func TestWriteAllowIgnoresTheUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	root := filepath.Join(t.TempDir(), "ClaudeCode")
	w, err := WriteAllow(root, false)
	if err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]os.FileMode{w.Path: 0o644, root: 0o755, filepath.Dir(w.Path): 0o755} {
		if info, _ := os.Stat(p); info.Mode().Perm() != mode {
			t.Fatalf("%s: %v", p, info.Mode())
		}
	}
}
