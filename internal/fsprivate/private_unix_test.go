//go:build !windows

package fsprivate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckPrivateRefusesWideLinkedForeignAndSymlinked(t *testing.T) {
	d := t.TempDir()
	if err := MakePrivate(d); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "f")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(p, false); err != nil {
		t.Fatal(err)
	}
	if CheckPrivate(p, true) == nil || CheckPrivate(d, false) == nil {
		t.Fatal("a file passed as a directory or the reverse")
	}
	os.Chmod(p, 0o640)
	if CheckPrivate(p, false) == nil {
		t.Fatal("a group-readable file passed")
	}
	os.Chmod(p, 0o600)
	if err := os.Link(p, filepath.Join(d, "g")); err != nil {
		t.Skip("no hard links here")
	}
	if CheckPrivate(p, false) == nil {
		t.Fatal("a hard-linked file passed")
	}
	os.Remove(filepath.Join(d, "g"))
	old := CurrentUID
	t.Cleanup(func() { CurrentUID = old })
	CurrentUID = func() int { return os.Getuid() + 1 }
	if CheckPrivate(p, false) == nil || CheckPrivate(d, true) == nil {
		t.Fatal("another user's file passed")
	}
	CurrentUID = old
	l := filepath.Join(d, "l")
	os.Symlink(p, l)
	if CheckPrivate(l, false) == nil {
		t.Fatal("a symlink passed")
	}
	if _, err := OpenRead(l); err == nil || !IsSymlinkRefusal(err) {
		t.Fatal("opened a symlink:", err)
	}
}

// The link fallback publishes without replacing, leaves one name, and
// leaves no temporary file.
func TestLinkNoReplaceFallback(t *testing.T) {
	d := t.TempDir()
	if err := MakePrivate(d); err != nil {
		t.Fatal(err)
	}
	stage := func(content string) string {
		p := filepath.Join(d, "tmp-"+content)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	target := filepath.Join(d, "k")
	if err := linkNoReplace(stage("first"), target); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(target, false); err != nil {
		t.Fatal("two names remain:", err)
	}
	second := stage("second")
	if err := linkNoReplace(second, target); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "first" {
		t.Fatal(string(b))
	}
	// A reader may hold two names for an instant, only where it says so.
	f, _ := os.Open(target)
	defer f.Close()
	os.Link(target, filepath.Join(d, "extra"))
	if CheckOpened(f, target) == nil || CheckOpenedLinks(f, target, 2) != nil {
		t.Fatal("the link allowance is wrong")
	}
}

func TestRemoveMissingIsNotExist(t *testing.T) {
	if err := Remove(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
