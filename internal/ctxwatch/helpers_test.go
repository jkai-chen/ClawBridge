package ctxwatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// absPath is an absolute path of this platform: under the root of the
// volume the temp directory is on ("/" on unix, "C:\" on Windows). Nothing
// is created there.
func absPath(parts ...string) string {
	root := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
	return filepath.Join(append([]string{root}, parts...)...)
}

// jsonString is s as a JSON string literal, quotes included, so a Windows
// path's backslashes are escaped as they are in a real record.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// symlink makes a link. On Windows creating one needs a privilege the
// account may lack, so there a failure skips the test; elsewhere it fails.
func symlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("no symlink privilege: %v", err)
		}
		t.Fatal(err)
	}
}

// jsonEscaped is s as it appears inside a JSON string, without the quotes.
func jsonEscaped(s string) string {
	q := jsonString(s)
	return q[1 : len(q)-1]
}
