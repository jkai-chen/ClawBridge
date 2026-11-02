// Package sealed keeps ClawBridge's client secrets (a device's signing seed,
// a Linear connection and a Linear API key) as encrypted files under
// <state>/keys, replacing the OS key store.
//
// A 32-byte data key in <state>/keys/data.key encrypts each secret with
// XChaCha20-Poly1305 into its own record, <state>/keys/<kind>/<id>.sealed.
// A record carries the ID of the key that sealed it, and its additional
// data binds the kind, the ID and that key ID, so a record renamed, copied
// under another ID or resealed by hand does not open. In mode "file", the
// only mode this package opens, the data key sits beside the records and
// full-disk encryption is what protects both; a "passphrase" header is
// recognised and reported as ErrPassphraseMode.
//
// Every write is atomic (a private temporary file, synced, renamed, with
// the directory synced). Every open refuses a symlink. Directories and
// files must be private to the user, and a state directory under WSL's
// /mnt is refused. A Store caches data keys by key ID and re-reads the key
// files once when it meets a record sealed under a key it does not hold,
// so a long-running process survives Rotate.
//
// Known limits. There is no lock between processes: two Rotate calls at
// once make the second fail cleanly (the first holds data.key.next) rather
// than queue. The privacy checks and the opens are not one atomic step, so
// a program running as the same user can swap a directory for a symlink
// between a check and a later create; it could read the files anyway, and
// a program running as another user is stopped by the 0700 parent.
package sealed

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"

	"clawbridge/internal/fsprivate"
)

// windowsDrives is where WSL mounts Windows' drives, which can't hold a
// private file; tests move it.
var windowsDrives = "/mnt/"

// goos is the OS the drive check is for; tests change it.
var goos = runtime.GOOS

const (
	dataKeyName = "data.key"
	nextKeyName = "data.key.next"
	suffix      = ".sealed"
)

// Store is the sealed files under one state directory. It is safe for
// concurrent use; other processes may use the same directory.
type Store struct {
	dir string // <state>/keys

	mu   sync.Mutex
	keys map[string][]byte // data keys by kid; mode file only

	// tolerateLinks lets data.key have two names for the moment the link
	// fallback of a no-replace rename holds them; set only while a process
	// that lost the creation race reads the winner's key.
	tolerateLinks bool

	// unlocked is set on a store made by OpenUnlocked: its data key came
	// from the caller, not from data.key, and it never makes a data key.
	unlocked bool

	// afterReseal is a test hook: Rotate calls it after each record is
	// resealed, and an error stops the rotation there as a crash would.
	afterReseal func(done int) error
}

// Open prepares <stateDir>/keys, making it 0700 (or owner-only on Windows)
// when missing, and returns the store. It makes data.key only when there is
// no sealed record, and finishes a rotation a crash interrupted. A missing
// data.key with records present is not made good: Open succeeds so that
// Check can report it, and every Seal and Unseal then fails with
// ErrMissingDataKey.
func Open(stateDir string) (*Store, error) {
	s, err := openDir(stateDir)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.recoverRotation(); err != nil {
		return nil, err
	}
	if _, _, err = s.writerKey(); err != nil && !errors.Is(err, ErrMissingDataKey) && !errors.Is(err, ErrPassphraseMode) {
		return nil, err
	}
	return s, nil
}

// openDir prepares <stateDir>/keys and returns a store over it, touching no
// key file.
func openDir(stateDir string) (*Store, error) {
	if stateDir == "" {
		return nil, fail(CodeInvalid, "", "no state directory")
	}
	state, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fail(CodeInvalid, stateDir, "cannot resolve the state directory "+stateDir)
	}
	if fsprivate.OnWindowsDrive(state, windowsDrives, goos == "windows") {
		return nil, fail(CodeWindowsDrive, state, "ClawBridge's state directory "+state+" is under "+windowsDrives+" (WSL's Windows drives and other mounts there can't hold private files); keep ClawBridge's state in the Linux home (~/.clawbridge, or set CLAWBRIDGE_HOME there)")
	}
	s := &Store{dir: filepath.Join(state, "keys"), keys: map[string][]byte{}}
	if err = s.makeDir(s.dir); err != nil {
		return nil, err
	}
	return s, nil
}

// makeDir makes dir 0700 and private if it is missing, then checks it.
func (s *Store) makeDir(dir string) error {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return ioFail(dir, "cannot make the directory", err)
		}
		if err = os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return ioFail(dir, "cannot make the directory", err)
		}
		if err = fsprivate.MakePrivate(dir); err != nil {
			return fail(CodeNotPrivate, dir, "cannot make "+dir+" private to this user")
		}
	}
	return s.checkDir(dir)
}

// checkDir refuses dir unless it is a private plain directory.
func (s *Store) checkDir(dir string) error {
	if err := fsprivate.CheckPrivate(dir, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ioFail(dir, "the directory is missing", err)
		}
		return notPrivate(dir, err)
	}
	return nil
}

func notPrivate(path string, cause error) error {
	e := fail(CodeNotPrivate, path, "the sealed key storage at "+path+" must be a plain file or directory, private to this user, not a symlink and not hard-linked")
	e.(*Error).err = cause
	return e
}

// kindDir is the directory of a kind's records, made when create is set.
// A missing directory is os.ErrNotExist.
func (s *Store) kindDir(kind string, create bool) (string, error) {
	if err := s.checkDir(s.dir); err != nil {
		return "", err
	}
	d := filepath.Join(s.dir, kind)
	if _, err := os.Lstat(d); errors.Is(err, os.ErrNotExist) {
		if !create {
			return d, os.ErrNotExist
		}
		if err = s.makeDir(d); err != nil {
			return d, err
		}
	}
	return d, s.checkDir(d)
}

// readFile reads a private regular file of at most max bytes without
// following a symlink. A missing file is os.ErrNotExist; a file that is not
// private, or is a symlink, is ErrNotPrivate; a larger one is corrupt.
func readFile(path string, max int64) ([]byte, error) { return readFileLinks(path, max, 1) }

// readFileLinks is readFile allowing the file up to links names.
func readFileLinks(path string, max int64, links int) ([]byte, error) {
	f, err := fsprivate.OpenRead(path)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil, err
		case fsprivate.IsSymlinkRefusal(err):
			return nil, notPrivate(path, err)
		}
		return nil, ioFail(path, "cannot open the file", err)
	}
	defer f.Close()
	if err = fsprivate.CheckOpenedLinks(f, path, links); err != nil {
		return nil, notPrivate(path, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, ioFail(path, "cannot read the file", err)
	}
	if int64(len(raw)) > max {
		return nil, fail(CodeRecordCorrupt, path, "the file "+path+" is too large")
	}
	return raw, nil
}

// readKeyFile loads a key file and caches its key.
func (s *Store) readKeyFile(path string) (kid string, key []byte, err error) {
	// A file's inherited ACEs come from keys/, so keys/ is checked on every
	// read, not only when the store is opened.
	if err = s.checkDir(s.dir); err != nil {
		return "", nil, err
	}
	links := 1
	if s.tolerateLinks {
		links = 2
	}
	raw, err := readFileLinks(path, maxKeyFileSize, links)
	if err != nil {
		return "", nil, err
	}
	kid, key, err = parseDataKey(raw, path)
	if errors.Is(err, ErrPassphraseMode) && s.unlocked {
		// The header is wrapped; the key is the one the caller unlocked, if
		// the file still names it.
		if k, ok := s.keys[kid]; ok {
			return kid, k, nil
		}
	}
	if err != nil {
		return "", nil, err
	}
	s.keys[kid] = key
	return kid, key, nil
}

func (s *Store) missingKey() error {
	p := filepath.Join(s.dir, dataKeyName)
	return fail(CodeMissingDataKey, p, "the key that unlocks your ClawBridge secrets is missing ("+p+"); it is never made again while sealed records exist. Restore it from a backup, or set the secrets up again: clawbridge join with a new invite for a device, clawbridge linear connect for a Linear connection, linear key set for an API key")
}

// writerKey is the key new records are sealed under: the rotation's new key
// while one is under way, else data.key, which it makes if no sealed record
// exists. It is re-read on every call, never taken from the cache, so a
// process that slept through a rotation does not seal under a retired key.
func (s *Store) writerKey() (string, []byte, error) {
	dk := filepath.Join(s.dir, dataKeyName)
	_, _, err := s.readKeyFile(dk)
	if err == nil {
		kid, key, err := s.readKeyFile(filepath.Join(s.dir, nextKeyName))
		if err == nil {
			return kid, key, nil
		}
		// A damaged data.key.next is for recoverRotation to judge; it does
		// not stop a write, which Seal checks again afterwards.
		if !errors.Is(err, os.ErrNotExist) && CodeOf(err) != CodeRecordCorrupt {
			return "", nil, err
		}
		return s.currentKey()
	}
	if !s.unlocked && CodeOf(err) == CodeRecordCorrupt && s.unusedDataKey() {
		// Empty or unparseable, and no record exists to be orphaned: it is
		// as good as missing.
		if rerr := fsprivate.Remove(dk); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return "", nil, ioFail(dk, "cannot remove the damaged data key", rerr)
		}
		err = os.ErrNotExist
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	if s.unlocked {
		// The caller holds a key; a data.key that vanished is not made again.
		return "", nil, s.missingKey()
	}
	// data.key is missing: make one only on a store that has nothing to lose.
	if _, err := os.Lstat(filepath.Join(s.dir, nextKeyName)); err == nil {
		return "", nil, s.missingKey()
	}
	has, err := s.hasRecords()
	if err != nil {
		return "", nil, err
	}
	if has {
		return "", nil, s.missingKey()
	}
	return s.createDataKey()
}

// unusedDataKey reports that nothing depends on data.key: no rotation is
// under way and no sealed record exists under any kind.
func (s *Store) unusedDataKey() bool {
	if _, err := os.Lstat(filepath.Join(s.dir, nextKeyName)); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	has, err := s.hasRecords()
	return err == nil && !has
}

// currentKey is data.key's key.
func (s *Store) currentKey() (string, []byte, error) {
	kid, key, err := s.readKeyFile(filepath.Join(s.dir, dataKeyName))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, s.missingKey()
	}
	return kid, key, err
}

// createDataKey makes data.key exclusively. A process that loses the race
// reads the winner's key.
func (s *Store) createDataKey() (string, []byte, error) {
	p := filepath.Join(s.dir, dataKeyName)
	kid, key := randomKid(), make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return "", nil, ioFail(p, "cannot get random bytes", err)
	}
	err := fsprivate.CreateExclusive(p, encodeDataKey(kid, key))
	switch {
	case err == nil:
		s.keys[kid] = key
		return kid, key, nil
	case errors.Is(err, os.ErrExist):
		// Lost the race: use the winner's key, which is whole because it
		// was published by a rename.
		s.tolerateLinks = true
		defer func() { s.tolerateLinks = false }()
		return s.currentKey()
	}
	return "", nil, ioFail(p, "cannot make the data key", err)
}

func randomKid() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("sealed: no random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// keyFor is the data key of kid. A key it does not hold is looked for once
// in data.key and data.key.next, which is how a process outlives a rotation.
func (s *Store) keyFor(kid string) ([]byte, error) {
	if k, ok := s.keys[kid]; ok {
		return k, nil
	}
	if _, _, err := s.readKeyFile(filepath.Join(s.dir, dataKeyName)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, s.missingKey()
		}
		return nil, err
	}
	// An unreadable data.key.next is not an error here: if it is the key
	// sought, the lookup below fails the same way.
	s.readKeyFile(filepath.Join(s.dir, nextKeyName))
	if k, ok := s.keys[kid]; ok {
		return k, nil
	}
	return nil, fail(CodeRecordCorrupt, "", "the record is sealed under a key this store does not have")
}

// hasRecords reports whether any sealed record exists.
func (s *Store) hasRecords() (bool, error) {
	for _, kind := range []string{KindDevice, KindLinear, KindLinearAPI} {
		names, err := s.names(kind)
		if err != nil {
			return false, err
		}
		if len(names) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// names lists the file names ending .sealed in a kind's directory.
func (s *Store) names(kind string) ([]string, error) {
	d, err := s.kindDir(kind, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f, err := fsprivate.OpenDir(d)
	if err != nil {
		if fsprivate.IsSymlinkRefusal(err) {
			return nil, notPrivate(d, err)
		}
		return nil, ioFail(d, "cannot list the directory", err)
	}
	defer f.Close()
	all, err := f.Readdirnames(-1)
	if err != nil {
		return nil, ioFail(d, "cannot list the directory", err)
	}
	var out []string
	for _, n := range all {
		if strings.HasSuffix(n, suffix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) recordPath(kind, id string) string {
	return filepath.Join(s.dir, kind, id+suffix)
}

// open decrypts a record file. The file name's kind and ID are checked
// against the record's before anything is decrypted.
func (s *Store) open(kind, id string) ([]byte, error) {
	if _, err := s.kindDir(kind, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fail(CodeNotFound, s.recordPath(kind, id), "there is no sealed "+kind+" record "+id)
		}
		return nil, err
	}
	p := s.recordPath(kind, id)
	raw, err := readFile(p, maxRecordSize)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fail(CodeNotFound, p, "there is no sealed "+kind+" record "+id)
	}
	if err != nil {
		return nil, err
	}
	rec, nonce, ct, err := decodeRecord(raw)
	if err != nil {
		return nil, fail(CodeRecordCorrupt, p, "the sealed record "+p+" is damaged; set the secret up again")
	}
	if rec.Kind != kind || rec.ID != id {
		return nil, fail(CodeRecordMismatch, p, "the sealed record "+p+" belongs to a different name than its file; set the secret up again")
	}
	key, err := s.keyFor(rec.Kid)
	if err != nil {
		if CodeOf(err) == CodeRecordCorrupt {
			return nil, fail(CodeRecordCorrupt, p, "the sealed record "+p+" is sealed under a key this store does not have; set the secret up again")
		}
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, ioFail(p, "cannot set up the cipher", err)
	}
	pt, err := aead.Open(nil, nonce, ct, aad(kind, id, rec.Kid))
	if err != nil {
		return nil, fail(CodeRecordCorrupt, p, "the sealed record "+p+" does not decrypt; set the secret up again")
	}
	return pt, nil
}

// seal encrypts pt for kind and id under key and returns the record file.
func seal(kind, id, kid string, key, pt []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fail(CodeIO, "", "cannot set up the cipher")
	}
	nonce := make([]byte, nonceLen)
	if _, err = rand.Read(nonce); err != nil {
		return nil, fail(CodeIO, "", "cannot get random bytes")
	}
	ct := aead.Seal(nil, nonce, pt, aad(kind, id, kid))
	b, err := json.Marshal(record{V: recordVersion, Kid: kid, Kind: kind, ID: id, Nonce: b64.EncodeToString(nonce), Ct: b64.EncodeToString(ct)})
	if err != nil {
		return nil, fail(CodeIO, "", "cannot encode the record")
	}
	return append(b, '\n'), nil
}

// write replaces a record file atomically.
func (s *Store) write(d, kind, id string, b []byte) error {
	err := fsprivate.WriteAtomic(d, "."+id+"-", id+suffix, b)
	if err == nil {
		return nil
	}
	p := s.recordPath(kind, id)
	var we *fsprivate.WriteError
	if errors.As(err, &we) && we.Stage == fsprivate.StageRename {
		return ioFail(p, "cannot replace the record (another program may hold it open)", err)
	}
	return ioFail(p, "cannot write the record", err)
}

// Seal stores plaintext, byte for byte, as the sealed record of kind and
// id, replacing any record there.
func (s *Store) Seal(kind, id string, plaintext []byte) error {
	if err := validate(kind, id); err != nil {
		return err
	}
	if len(plaintext) > maxPlaintext {
		return fail(CodeInvalid, "", "the secret is too large to seal")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.kindDir(kind, true)
	if err != nil {
		return err
	}
	// A rotation that finishes while the record is written would leave it
	// under a retired key; the key is checked again afterwards.
	for try := 0; try < 3; try++ {
		kid, key, err := s.writerKey()
		if err != nil {
			return err
		}
		b, err := seal(kind, id, kid, key, plaintext)
		if err != nil {
			return err
		}
		if err = s.write(d, kind, id, b); err != nil {
			return err
		}
		if now, _, err := s.writerKey(); err == nil && now == kid {
			return nil
		}
	}
	return fail(CodeIO, s.recordPath(kind, id), "the data key kept changing while the record was written; try again")
}

// Unseal returns the plaintext of the sealed record of kind and id, or
// ErrNotFound.
func (s *Store) Unseal(kind, id string) ([]byte, error) {
	if err := validate(kind, id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open(kind, id)
}

// Delete removes a record. A record that is not there is not an error.
func (s *Store) Delete(kind, id string) error {
	if err := validate(kind, id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.kindDir(kind, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = fsprivate.Remove(s.recordPath(kind, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ioFail(s.recordPath(kind, id), "cannot remove the record", err)
	}
	if err = fsprivate.SyncDir(d); err != nil {
		return ioFail(d, "cannot sync the directory", err)
	}
	return nil
}

// List returns the IDs of a kind's records in order.
func (s *Store) List(kind string) ([]string, error) {
	if !validKind(kind) {
		return nil, fail(CodeInvalid, "", "unknown kind of sealed record")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.names(kind)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, n := range names {
		if id := strings.TrimSuffix(n, suffix); validate(kind, id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
