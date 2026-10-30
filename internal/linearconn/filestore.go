package linearconn

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"
)

// The places a connection's credential may be kept, as the device key's
// are.
const (
	// StoreKeyring is the OS store: the macOS Keychain, Windows Credential
	// Manager, or the Secret Service on Linux.
	StoreKeyring = "keyring"
	// StoreFile is a plain file, 0600 in a 0700 directory, chosen
	// explicitly with --key-store file.
	StoreFile = "file"
)

// FileWarning is said whenever a connection goes to a plain file.
const FileWarning = "the Linear connection is a plain file: anyone who can read it as you can act as you in Linear"

// ValidStore reports whether s names a store.
func ValidStore(s string) bool { return s == StoreKeyring || s == StoreFile }

// goos is the OS the key store's wording is for; tests change it.
var goos = runtime.GOOS

// windowsDrives is where WSL mounts Windows' drives, which can't hold a
// private file; tests move it.
var windowsDrives = "/mnt/"

// errNoFile reports a file connection whose file is gone; callers name
// the path.
var errNoFile = errors.New("the Linear connection file is missing")

// keyringDetail names a failed OS store use, why it failed, and the way
// out.
func keyringDetail(cause error) string {
	msg := "cannot save the Linear connection in the OS key store"
	if goos == "linux" {
		msg += " (the Secret Service: GNOME Keyring or KWallet)"
	}
	if why := keyringCause(cause); why != "" {
		msg += " (" + why + ")"
	}
	return msg + "; unlock it, or on a machine without one connect with --key-store file (" + FileWarning + ")"
}

// errNoSecretService is a key store check that found no store to use.
var errNoSecretService = errors.New("no Secret Service on the session bus")

// collectionLocked is go-keyring's failed unlock of a Secret Service
// collection, with the collection's name.
var collectionLocked = regexp.MustCompile(`failed to unlock correct collection '/org/freedesktop/secrets/(?:collection|aliases)/([^']*)'`)

// collectionName is a collection name safe to repeat.
var collectionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// keyringCause says why the OS key store failed, as one of a fixed set of
// phrases: nothing the store said is repeated, so no credential, path or
// control character can reach a message. Empty without an error.
func keyringCause(cause error) string {
	if cause == nil {
		return ""
	}
	said := cause.Error()
	switch {
	case errors.Is(cause, keyring.ErrSetDataTooBig):
		return "the credential is too large for the key store"
	case errors.Is(cause, keyring.ErrNotFound):
		return "the key store has no credential for this profile"
	case errors.Is(cause, errNoSecretService):
		return "no Secret Service is running on the session bus"
	case collectionLocked.MatchString(said):
		if name := collectionLocked.FindStringSubmatch(said)[1]; collectionName.MatchString(name) {
			return "the " + name + " collection is locked"
		}
		return "a collection is locked"
	case strings.Contains(said, "org.freedesktop.Secret.Error.IsLocked"):
		return "a collection is locked"
	case strings.Contains(said, "SystemPrompter") || strings.Contains(said, "org.freedesktop.Secret.Prompt"):
		return "the key store could not show its unlock prompt"
	case strings.Contains(said, "org.freedesktop.Secret.Error.NoSuchObject"):
		return "the key store has no such collection or item"
	case strings.Contains(said, "org.freedesktop.DBus.Error.ServiceUnknown"), strings.Contains(said, "org.freedesktop.DBus.Error.NameHasNoOwner"),
		strings.Contains(said, "couldn't determine address of session bus"), strings.Contains(said, "autolaunch"):
		return "no Secret Service is running on the session bus"
	}
	return "the key store gave an unrecognised error"
}

// keyStoreSaid is a failed key store read's or deletion's detail: why it
// failed.
func keyStoreSaid(cause error) string { return keyringCause(cause) }

// onWindowsDrive reports whether path, through any symlink in it, is on
// one of WSL's Windows drives.
func onWindowsDrive(path string) bool {
	if goos == "windows" {
		return false
	}
	path = filepath.Clean(path)
	// The nearest existing ancestor is resolved: the store's own
	// directories may not be made yet.
	resolved, rest := path, ""
	for p := path; ; {
		if r, e := filepath.EvalSymlinks(p); e == nil {
			resolved = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		rest, p = filepath.Join(filepath.Base(p), rest), parent
	}
	return strings.HasPrefix(resolved+string(filepath.Separator), windowsDrives)
}

func fileProblem(kind string, e error) error {
	var known *Error
	if errors.As(e, &known) {
		return &Error{Kind: kind, Detail: known.Kind}
	}
	return &Error{Kind: kind, Detail: e.Error()}
}

// connectionDir is the file store's directory under the profile
// directory, made 0700 when missing. It is refused on a Windows drive, or
// when it is open to others or owned by another user.
func (m *Manager) connectionDir(create bool) (string, error) {
	d, e := m.dir()
	if e != nil {
		return "", e
	}
	if onWindowsDrive(d) {
		return "", &Error{Kind: "unsafe connection file", Detail: "ClawBridge's state directory " + d + " is under " + windowsDrives + " (WSL's Windows drives and other mounts there can't hold private files); keep ClawBridge's state in the Linux home (~/.clawbridge, or set CLAWBRIDGE_HOME there)"}
	}
	if create {
		if d, e = m.privateDir(); e != nil {
			return "", e
		}
	}
	c := filepath.Join(d, "connections")
	if create {
		if e = os.Mkdir(c, 0o700); e == nil {
			if e = makePrivate(c); e != nil {
				return "", problem("cannot protect connection directory")
			}
		} else if !errors.Is(e, fs.ErrExist) {
			return "", problem("connection storage unavailable")
		}
	}
	if e = checkFile(c, true); e != nil {
		return "", fileProblem("unsafe connection file", e)
	}
	return c, nil
}

// fileStoreUsable checks, before anything is asked of Linear or the
// person, that the file store could be used: not on a Windows drive, and
// the connection directory private if it exists. It creates nothing; the
// directory is made when the credential is written.
func (m *Manager) fileStoreUsable() error {
	d, e := m.dir()
	if e != nil {
		return e
	}
	if onWindowsDrive(d) {
		_, e = m.connectionDir(false)
		return e
	}
	c := filepath.Join(d, "connections")
	if _, e = os.Lstat(c); errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	_, e = m.connectionDir(false)
	return e
}

func (m *Manager) fileGet(id string) (string, error) {
	d, e := m.connectionDir(false)
	if e != nil {
		return "", e
	}
	p := filepath.Join(d, id+".json")
	if e = checkFile(p, false); errors.Is(e, errNoFile) {
		// The record says file: never the keyring instead.
		return "", &Error{Kind: "secure credential unavailable", Detail: "the Linear connection file " + p + " is missing; run clawbridge linear logout --profile " + id + ", then connect again"}
	} else if e != nil {
		return "", fileProblem("secure credential unavailable", e)
	}
	b, e := readPrivate(p)
	if e != nil {
		return "", problem("secure credential unavailable")
	}
	return string(b), nil
}

// readPrivate reads a checked file without following a symlink swapped in
// since the check (where the OS can refuse one), and only if the file opened
// is the one checked.
func readPrivate(p string) ([]byte, error) {
	before, e := os.Lstat(p)
	if e != nil {
		return nil, e
	}
	f, e := os.OpenFile(p, os.O_RDONLY|openNoFollow, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("the file changed while it was opened")
	}
	return io.ReadAll(io.LimitReader(f, maxFile+1))
}

// fileSet replaces the connection's file through a private temporary file;
// a file already there must pass the same checks a read does.
func (m *Manager) fileSet(id, value string) error {
	d, e := m.connectionDir(true)
	if e != nil {
		return e
	}
	p := filepath.Join(d, id+".json")
	if e = checkFile(p, false); e != nil && !errors.Is(e, errNoFile) {
		return fileProblem("secure credential storage failed", e)
	}
	if e = writePrivate(d, id+".json", "connection file", []byte(value)); e != nil {
		return fileProblem("secure credential storage failed", e)
	}
	return nil
}

// The store record is a sidecar, <profile>.store holding "file", beside
// the connection's file, as linearkey records its key's store: profiles.json
// keeps main's schema. No record is the keyring.

func (m *Manager) recordPath(id string) (string, error) {
	d, e := m.dir()
	if e != nil {
		return "", e
	}
	return filepath.Join(d, "connections", id+".store"), nil
}

// RecordedStore names the profile's store from its record alone, without
// reading the credential: for doctor, when the credential can't be read.
func (m *Manager) RecordedStore(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", problem("invalid profile")
	}
	return m.recorded(id)
}

// recorded names the profile's store from its record. A record that can't
// be read is an error, never the keyring.
func (m *Manager) recorded(id string) (string, error) {
	p, e := m.recordPath(id)
	if e != nil {
		return "", e
	}
	if _, e = os.Lstat(p); errors.Is(e, fs.ErrNotExist) {
		return StoreKeyring, nil
	}
	if e = checkFile(filepath.Dir(p), true); e != nil {
		return "", fileProblem("unsafe connection file", e)
	}
	if e = checkFile(p, false); e != nil {
		return "", fileProblem("unsafe connection file", e)
	}
	b, e := readPrivate(p)
	if e != nil || strings.TrimSpace(string(b)) != StoreFile {
		return "", &Error{Kind: "invalid connection store record", Detail: p}
	}
	return StoreFile, nil
}

// record writes the profile's file record, 0600 and atomically.
func (m *Manager) record(id string) error {
	d, e := m.connectionDir(true)
	if e != nil {
		return e
	}
	p := filepath.Join(d, id+".store")
	if e = checkFile(p, false); e != nil && !errors.Is(e, errNoFile) {
		return fileProblem("secure credential storage failed", e)
	}
	if e = writePrivate(d, id+".store", "connection store record", []byte(StoreFile+"\n")); e != nil {
		return fileProblem("secure credential storage failed", e)
	}
	return nil
}

// unrecord removes the profile's record; one already gone is not an error.
func (m *Manager) unrecord(id string) error {
	p, e := m.recordPath(id)
	if e != nil {
		return e
	}
	if e = os.Remove(p); e != nil && !errors.Is(e, fs.ErrNotExist) {
		return problem("connection store record removal failed")
	}
	return nil
}

// fileDelete removes the connection's file; one already gone is not an
// error.
func (m *Manager) fileDelete(id string) error {
	d, e := m.dir()
	if e != nil {
		return e
	}
	if e = os.Remove(filepath.Join(d, "connections", id+".json")); e != nil && !errors.Is(e, fs.ErrNotExist) {
		return problem("secure credential deletion failed")
	}
	return nil
}
