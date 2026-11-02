//go:build !windows

package sealed

import (
	"os"
	"path/filepath"
	"testing"

	"clawbridge/internal/fsprivate"
)

func TestWideOwnerLinkedAndSymlinkedStorageIsRefused(t *testing.T) {
	setup := func(t *testing.T) (*Store, string) {
		s, state := open(t)
		seedAll(t, s)
		return s, state
	}
	t.Run("wide record", func(t *testing.T) {
		s, state := setup(t)
		os.Chmod(recFile(state, KindLinear, linA), 0o644)
		_, err := s.Unseal(KindLinear, linA)
		wantCode(t, err, CodeNotPrivate)
		if rep := s.Check(); rep.OK() {
			t.Fatal("Check passed a wide record")
		}
	})
	t.Run("wide data key", func(t *testing.T) {
		_, state := setup(t)
		os.Chmod(filepath.Join(state, "keys", "data.key"), 0o640)
		s2 := &Store{dir: filepath.Join(state, "keys"), keys: map[string][]byte{}}
		_, err := s2.Unseal(KindLinear, linA)
		wantCode(t, err, CodeNotPrivate)
		wantCode(t, s2.Seal(KindLinear, linB, blob), CodeNotPrivate)
		rep := s2.Check()
		if rep.DataKey.Private || rep.DataKey.Err != CodeNotPrivate {
			t.Fatalf("%+v", rep.DataKey)
		}
	})
	t.Run("wide keys directory", func(t *testing.T) {
		_, state := setup(t)
		os.Chmod(filepath.Join(state, "keys"), 0o755)
		_, err := Open(state)
		wantCode(t, err, CodeNotPrivate)
	})
	t.Run("wide kind directory", func(t *testing.T) {
		s, state := setup(t)
		os.Chmod(filepath.Join(state, "keys", KindLinear), 0o750)
		_, err := s.Unseal(KindLinear, linA)
		wantCode(t, err, CodeNotPrivate)
		_, err = s.List(KindLinear)
		wantCode(t, err, CodeNotPrivate)
	})
	t.Run("another owner", func(t *testing.T) {
		s, _ := setup(t)
		old := fsprivate.CurrentUID
		t.Cleanup(func() { fsprivate.CurrentUID = old })
		fsprivate.CurrentUID = func() int { return os.Getuid() + 1 }
		_, err := s.Unseal(KindLinear, linA)
		wantCode(t, err, CodeNotPrivate)
	})
	t.Run("hard link", func(t *testing.T) {
		s, state := setup(t)
		if os.Link(recFile(state, KindLinear, linA), filepath.Join(t.TempDir(), "copy")) != nil {
			t.Skip("no hard links here")
		}
		_, err := s.Unseal(KindLinear, linA)
		wantCode(t, err, CodeNotPrivate)
	})
	t.Run("symlinked keys directory", func(t *testing.T) {
		_, state := setup(t)
		other := filepath.Join(t.TempDir(), "elsewhere")
		os.Rename(filepath.Join(state, "keys"), other)
		os.Symlink(other, filepath.Join(state, "keys"))
		_, err := Open(state)
		wantCode(t, err, CodeNotPrivate)
	})
	t.Run("symlinked kind directory", func(t *testing.T) {
		s, state := setup(t)
		other := filepath.Join(t.TempDir(), "elsewhere")
		os.Rename(filepath.Join(state, "keys", KindLinear), other)
		os.Symlink(other, filepath.Join(state, "keys", KindLinear))
		_, err := s.Unseal(KindLinear, linA)
		wantCode(t, err, CodeNotPrivate)
		_, err = s.List(KindLinear)
		wantCode(t, err, CodeNotPrivate)
		wantCode(t, s.Seal(KindLinear, linB, blob), CodeNotPrivate)
	})
	t.Run("symlinked record", func(t *testing.T) {
		s, state := setup(t)
		p := recFile(state, KindLinear, linA)
		real := recFile(state, KindLinear, linB)
		os.Rename(p, real)
		os.Symlink(real, p)
		_, err := s.Unseal(KindLinear, linA)
		wantCode(t, err, CodeNotPrivate)
	})
	t.Run("symlinked data key", func(t *testing.T) {
		_, state := setup(t)
		dk := filepath.Join(state, "keys", "data.key")
		real := filepath.Join(state, "keys", "moved")
		os.Rename(dk, real)
		os.Symlink(real, dk)
		s2 := &Store{dir: filepath.Join(state, "keys"), keys: map[string][]byte{}}
		_, err := s2.Unseal(KindLinear, linA)
		wantCode(t, err, CodeNotPrivate)
	})
}
