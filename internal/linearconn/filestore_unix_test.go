//go:build !windows

package linearconn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinearFileStoreRefusesWideModesAndOtherOwners(t *testing.T) {
	rt, _ := oauthProvider(t)
	m := testManager(t, rt)
	p, e := connectWith(m, StoreFile)
	if e != nil {
		t.Fatal(e)
	}
	path := connectionFile(m, p.ID)
	for _, c := range []struct {
		path string
		mode os.FileMode
	}{{path, 0o644}, {path, 0o640}, {path, 0o604}, {filepath.Dir(path), 0o755}, {filepath.Dir(path), 0o710}} {
		if err := os.Chmod(c.path, c.mode); err != nil {
			t.Fatal(err)
		}
		_, err := m.Status(context.Background(), p.ID)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("mode %04o", c.mode)) || !strings.Contains(err.Error(), "chmod") {
			t.Fatalf("%s %o accepted: %v", c.path, c.mode, err)
		}
		noSecret(t, err.Error())
		_ = os.Chmod(path, 0o600)
		_ = os.Chmod(filepath.Dir(path), 0o700)
	}
	if _, e = m.Status(context.Background(), p.ID); e != nil {
		t.Fatal(e)
	}
	old := currentUID
	t.Cleanup(func() { currentUID = old })
	currentUID = func() int { return os.Getuid() + 1 }
	_, e = m.Status(context.Background(), p.ID)
	if e == nil || !strings.Contains(e.Error(), "belongs to another user") {
		t.Fatal(e)
	}
	// Nothing is written over another user's file either.
	_, e = m.Connect(context.Background(), ConnectOptions{Name: "work", ProfileID: p.ID, Access: Write, KeyStore: StoreFile, RedirectURL: "http://127.0.0.1:44999/callback", FetchCode: fetchCode})
	if e == nil {
		t.Fatal("reconnect wrote over another user's file")
	}
	currentUID = old
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, e = m.Connect(context.Background(), ConnectOptions{Name: "work", ProfileID: p.ID, Access: Write, KeyStore: StoreFile, RedirectURL: "http://127.0.0.1:44999/callback", FetchCode: fetchCode})
	if e == nil || !strings.Contains(e.Error(), "mode 0644") {
		t.Fatal("a wide file was written over", e)
	}
}

// A symlink swapped in for the checked file is not followed.
func TestLinearReadPrivateRefusesASymlink(t *testing.T) {
	d := t.TempDir()
	target, link := filepath.Join(d, "target"), filepath.Join(d, "link")
	if err := os.WriteFile(target, []byte("token-one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if b, err := readPrivate(link); err == nil {
		t.Fatalf("followed a symlink: %q", b)
	}
	if b, err := readPrivate(target); err != nil || string(b) != "token-one" {
		t.Fatal(b, err)
	}
}

// A failed write names what failed, never the content.
func TestLinearWritePrivateNamesWhatFailed(t *testing.T) {
	err := writePrivate(filepath.Join(t.TempDir(), "absent"), "x.store", "connection store record", []byte("token-one"))
	if err == nil || !strings.Contains(err.Error(), "connection store record") {
		t.Fatal(err)
	}
	noSecret(t, err.Error())
}

func TestLinearFileStoreRefusesAHardLinkedFile(t *testing.T) {
	rt, _ := oauthProvider(t)
	m := testManager(t, rt)
	p, e := connectWith(m, StoreFile)
	if e != nil {
		t.Fatal(e)
	}
	path := connectionFile(m, p.ID)
	if e = os.Link(path, filepath.Join(t.TempDir(), "copy")); e != nil {
		t.Skip("no hard links here:", e)
	}
	_, e = m.Status(context.Background(), p.ID)
	if e == nil || !strings.Contains(e.Error(), "hard-linked") {
		t.Fatal(e)
	}
	noSecret(t, e.Error())
}
