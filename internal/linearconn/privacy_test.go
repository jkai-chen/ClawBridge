package linearconn

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLinearProfileArtifactsUsePlatformPrivacy(t *testing.T) {
	m := New(Config{Directory: filepath.Join(t.TempDir(), "linear")})
	d, err := m.privateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err = privatePath(d, true); err != nil {
		t.Fatal(err)
	}
	unlock, err := m.diskLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(d, ".credentials.lock")
	if err = privatePath(lock, false); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if _, err = os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatal("owned lock retained", err)
	}
	if err = m.save(index{Version: 1, Profiles: []Profile{}}); err != nil {
		t.Fatal(err)
	}
	if err = privatePath(filepath.Join(d, "profiles.json"), false); err != nil {
		t.Fatal(err)
	}
	if _, err = m.load(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(d)
	if err != nil || len(entries) != 1 {
		t.Fatal("unexpected retained staging files", entries, err)
	}
}

func TestLinearProfileStorageRejectsFinalDirectoryAndFileSymlinks(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := makePrivate(target); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linear-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlink creation unavailable on this platform")
	}
	m := New(Config{Directory: link})
	if _, err := m.privateDir(); err == nil {
		t.Fatal("directory symlink accepted")
	}
	if _, err := m.load(); err == nil {
		t.Fatal("directory symlink read")
	}
	m = New(Config{Directory: target})
	outside := filepath.Join(t.TempDir(), "outside.json")
	const original = `{"version":1,"profiles":[]}`
	if err := os.WriteFile(outside, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := makePrivate(outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "profiles.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.load(); err == nil {
		t.Fatal("profile symlink read")
	}
	if err := m.save(index{Version: 1}); err == nil {
		t.Fatal("profile symlink overwritten")
	}
	b, err := os.ReadFile(outside)
	if err != nil || string(b) != original {
		t.Fatal("symlink target changed", err)
	}
}
