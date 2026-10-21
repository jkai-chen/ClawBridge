//go:build !windows

package notify

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNotificationPrivateAncestorUsesActualDirectory(t *testing.T) {
	private := t.TempDir()
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal(err)
	}
	// An ancestor symlink must not confer its lexical parent's privacy on a
	// separately reachable public directory.
	public, err := os.MkdirTemp("", "clawbridge-public-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(public)
	if err := os.Chmod(public, 0755); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(public, "leaf")
	if err := os.Mkdir(leaf, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(private, "link")
	if err := os.Symlink(public, link); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(link, "leaf")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.Stat(leaf)
	if err != nil {
		t.Fatal(err)
	}
	want := checkPrivate(leaf, actual)
	got := checkPrivate(path, info)
	if (got == nil) != (want == nil) {
		t.Fatal("lexical private ancestor changed target privacy", got, want)
	}
}
