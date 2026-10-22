//go:build !windows

package integration

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"clawbridge/internal/linearconn"
)

func TestL1ProfileUnixPrivateModesAndHardLinkRefusal(t *testing.T) {
	for _, kind := range []string{"directory-mode", "index-mode", "index-hardlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newL1OAuth(t, linearconn.Read, "")
			p, err := f.manager.Connect(context.Background(), f.options())
			if err != nil {
				t.Fatal(err)
			}
			index := filepath.Join(f.dir, "profiles.json")
			for _, path := range []string{f.dir, index} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm()&0077 != 0 {
					t.Fatal("new profile metadata was not made private")
				}
			}
			values := f.store.snapshot()
			gets := f.store.gets
			switch kind {
			case "directory-mode":
				if err := os.Chmod(f.dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "index-mode":
				if err := os.Chmod(index, 0644); err != nil {
					t.Fatal(err)
				}
			case "index-hardlink":
				if err := os.Link(index, filepath.Join(t.TempDir(), "second-index-link")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.manager.Profiles(); err == nil {
				t.Fatal("unsafe Unix profile metadata accepted")
			}
			if _, err := f.manager.Status(context.Background(), p.ID); err == nil {
				t.Fatal("unsafe Unix profile route reached credentials")
			}
			if f.store.gets != gets || !reflect.DeepEqual(values, f.store.snapshot()) {
				t.Fatal("unsafe profile metadata touched credentials")
			}
		})
	}
}
