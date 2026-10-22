//go:build !windows

package linearconn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinearExistingUnsafeDirectoryAndFileAreNotRepaired(t *testing.T) {
	d := filepath.Join(t.TempDir(), "linear")
	if err := os.Mkdir(d, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0755); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Directory: d})
	if _, err := m.privateDir(); err == nil {
		t.Fatal("unsafe existing directory accepted")
	}
	if _, err := m.load(); err == nil {
		t.Fatal("unsafe existing directory read")
	}
	st, err := os.Stat(d)
	if err != nil || st.Mode().Perm() != 0755 {
		t.Fatal("existing directory repaired", err)
	}
	if err = os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "profiles.json")
	if err = os.WriteFile(p, []byte(`{"version":1,"profiles":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = m.load(); err == nil {
		t.Fatal("unsafe file read")
	}
	if err = m.save(index{Version: 1}); err == nil {
		t.Fatal("unsafe file overwritten")
	}
	st, err = os.Stat(p)
	if err != nil || st.Mode().Perm() != 0644 {
		t.Fatal("existing file repaired", err)
	}
	if err = os.Chmod(p, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(p, filepath.Join(d, "hard-link")); err != nil {
		t.Fatal(err)
	}
	if _, err = m.load(); err == nil {
		t.Fatal("hard linked profile accepted")
	}
}
