package setup

import (
	"os"
	"testing"
)

func TestPrivateConfigModeComparesContentOnWindows(t *testing.T) {
	for _, c := range []struct {
		goos string
		perm os.FileMode
		want bool
	}{
		{"windows", 0666, true}, // Windows never reports 0600
		{"windows", 0444, true},
		{"linux", 0600, true},
		{"linux", 0644, false}, // a widened config is rewritten private
		{"darwin", 0666, false},
	} {
		if got := privateConfigMode(c.goos, c.perm); got != c.want {
			t.Errorf("%s %o: %v, want %v", c.goos, c.perm, got, c.want)
		}
	}
}
