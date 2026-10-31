package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FR24 §5: the binary an update replaces is kept in one slot, so
// `self-update --rollback` can put it back. The slot is a directory holding
// clawbridge-vX.Y.Z[.exe] and previous.json, which names that file with its
// version and SHA-256. previous.json is the commit point: a reader trusts
// only the file it names, and only when the file's SHA-256 matches.

// PreviousDirName is the slot's directory in the client's state home.
const PreviousDirName = "previous"

const (
	previousManifest = "previous.json"
	maxPreviousJSON  = 4 << 10
)

// ErrNoPrevious is a slot with no previous.json: nothing is kept.
var ErrNoPrevious = errors.New("no previous binary is kept")

// Previous is previous.json: the kept binary's version, its SHA-256 and its
// file name in the slot.
type Previous struct {
	Version string    `json:"version"`
	SHA256  string    `json:"sha256"`
	File    string    `json:"file"`
	SavedAt time.Time `json:"saved_at"`
}

// slotName is the kept binary's file name for version.
func slotName(version string) string {
	name := "clawbridge-" + version
	if hostOS == "windows" {
		name += ".exe"
	}
	return name
}

// ReadPrevious reads dir's previous.json and checks its fields; it doesn't
// hash the kept file. A missing previous.json is ErrNoPrevious.
func ReadPrevious(dir string) (Previous, error) {
	f, err := os.Open(filepath.Join(dir, previousManifest))
	if errors.Is(err, fs.ErrNotExist) {
		return Previous{}, ErrNoPrevious
	}
	if err != nil {
		return Previous{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPreviousJSON+1))
	if err != nil {
		return Previous{}, err
	}
	var p Previous
	if len(b) > maxPreviousJSON || json.Unmarshal(b, &p) != nil ||
		!validVersion(p.Version) || p.File != slotName(p.Version) || !manifestHash.MatchString(p.SHA256) {
		return Previous{}, fmt.Errorf("%s is malformed", filepath.Join(dir, previousManifest))
	}
	return p, nil
}

// CheckPrevious is ReadPrevious, then the kept file's SHA-256 against it.
func CheckPrevious(dir string) (Previous, error) {
	p, err := ReadPrevious(dir)
	if err != nil {
		return Previous{}, err
	}
	sum, err := hashFile(filepath.Join(dir, p.File))
	if err != nil {
		return Previous{}, fmt.Errorf("the kept %s can't be read: %w", p.Version, err)
	}
	if sum != p.SHA256 {
		return Previous{}, fmt.Errorf("the kept %s doesn't match the sha256 in %s", p.Version, previousManifest)
	}
	return p, nil
}

// pendingSlot is a copy of the replaced binary under a temporary name in
// the slot, waiting for the swap to succeed.
type pendingSlot struct {
	dir, temp, version, sum string
}

// prepareSlot copies src into dir under a temporary name: a hard link when
// link is set and dir is on src's volume, else a byte copy with src's mode.
// version is what src runs as; a development build is not kept.
func prepareSlot(dir, src, version string, link bool) (*pendingSlot, error) {
	if !validVersion(version) {
		return nil, fmt.Errorf("the replaced binary is a development build (%s)", version)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	temp := filepath.Join(dir, ".clawbridge-slot-"+randomHex())
	if !link || linkFile(src, temp) != nil {
		if err := copyFile(src, temp); err != nil {
			_ = os.Remove(temp)
			return nil, err
		}
	}
	sum, err := hashFile(temp)
	if err != nil {
		_ = os.Remove(temp)
		return nil, err
	}
	return &pendingSlot{dir: dir, temp: temp, version: version, sum: sum}, nil
}

// discard removes the temporary copy: the swap failed, the slot stays.
func (s *pendingSlot) discard() {
	if s != nil {
		_ = os.Remove(s.temp)
	}
}

// commit names the copy clawbridge-vX[.exe], writes previous.json for it,
// then removes every other kept binary, so one is kept.
func (s *pendingSlot) commit() error {
	name := slotName(s.version)
	if err := os.Rename(s.temp, filepath.Join(s.dir, name)); err != nil {
		_ = os.Remove(s.temp)
		return err
	}
	b, err := json.MarshalIndent(Previous{Version: s.version, SHA256: s.sum, File: name, SavedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(s.dir, previousManifest), append(b, '\n')); err != nil {
		return err
	}
	removeKept(s.dir, name)
	return nil
}

// keepSlot finishes a slot after a successful swap: it commits the copy,
// or on any failure (to prepare or to commit) empties the slot, so it
// never offers a rollback to anything but the binary just replaced. It
// returns the version kept, or a warning; never an error, since the update
// stands. A "" dir keeps nothing.
func keepSlot(dir string, s *pendingSlot, prepareErr error) (kept, warning string) {
	if dir == "" {
		return "", ""
	}
	err := prepareErr
	if err == nil {
		err = s.commit()
	}
	if err != nil {
		clearSlot(dir)
		return "", fmt.Sprintf("rollback isn't available: the replaced binary could not be kept in %s (%v)", dir, err)
	}
	return s.version, ""
}

// clearSlot empties the slot, best effort: previous.json first, so a
// half-cleared slot reads as empty.
func clearSlot(dir string) {
	_ = os.Remove(filepath.Join(dir, previousManifest))
	removeKept(dir, "")
}

// removeKept removes the kept binaries in dir other than keep.
func removeKept(dir, keep string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "clawbridge-*"))
	for _, path := range matches {
		if filepath.Base(path) == keep {
			continue
		}
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			_ = os.Remove(path)
		}
	}
}

// copyFile copies src to a new file dst with src's permission bits.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// hashFile is path's hex SHA-256.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeFileAtomic writes b to path through a temporary file in its
// directory (CreateTemp's mode, 0600), so a reader sees the old file or the
// new one.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+strings.TrimSuffix(filepath.Base(path), ".json")+"-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(temp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}
