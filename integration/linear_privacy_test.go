package integration

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"clawbridge/internal/linearconn"
)

func TestL1ProfileStorageReloadAndOwnedLockLifecycle(t *testing.T) {
	f := newL1OAuth(t, linearconn.Read, "")
	p, err := f.manager.Connect(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(f.dir, "profiles.json")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	values := f.store.snapshot()
	// Reopening is an actual personal-manager boundary using explicit memory
	// credentials; no native secure-store implementation is ever selected.
	m := linearconn.New(linearconn.Config{Directory: f.dir, Store: f.store})
	profiles, err := m.Profiles()
	if err != nil || len(profiles) != 1 || profiles[0].ID != p.ID {
		t.Fatalf("fresh manager cannot reload profile: %v", err)
	}
	if _, err := m.Status(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(f.dir, ".credentials.lock")
	if err := os.WriteFile(lock, []byte("another-operation-owned-lock"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := m.Logout(ctx, p.ID); err == nil {
		t.Fatal("logout ignored retained lock")
	}
	if time.Since(start) > time.Second {
		t.Fatal("locked profile operation ignored its shorter deadline")
	}
	if b, err := os.ReadFile(lock); err != nil || string(b) != "another-operation-owned-lock" {
		t.Fatal("failed operation stole/deleted another lock")
	}
	if !reflect.DeepEqual(values, f.store.snapshot()) {
		t.Fatal("retained-lock failure changed credential state")
	}
	if b, err := os.ReadFile(index); err != nil || string(b) != string(before) {
		t.Fatal("retained-lock failure changed profile metadata")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := m.Logout(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.store.snapshot()) != 0 {
		t.Fatal("selected logout did not remove memory credential")
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "profiles.json" {
			t.Fatal("profile operation retained staging/lock file")
		}
	}
}

func TestL1ProfileStorageRejectsLeafSymlinksBeforeCredentialRead(t *testing.T) {
	for _, kind := range []string{"directory", "index"} {
		t.Run(kind, func(t *testing.T) {
			f := newL1OAuth(t, linearconn.Read, "")
			p, err := f.manager.Connect(context.Background(), f.options())
			if err != nil {
				t.Fatal(err)
			}
			values := f.store.snapshot()
			gets := f.store.gets
			path := f.dir
			if kind == "index" {
				path = filepath.Join(path, "profiles.json")
			}
			moved := path + "-original"
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, path); err != nil {
				t.Skipf("fixture symlink privilege unavailable: %v", err)
			}
			if _, err := f.manager.Profiles(); err == nil {
				t.Fatal("symlink profile metadata accepted")
			}
			if _, err := f.manager.Status(context.Background(), p.ID); err == nil {
				t.Fatal("symlink profile route reached credentials")
			}
			if f.store.gets != gets || !reflect.DeepEqual(values, f.store.snapshot()) {
				t.Fatal("rejected unsafe profile path touched credential store")
			}
		})
	}
}
