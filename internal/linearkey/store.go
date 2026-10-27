package linearkey

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The places a key may be kept.
const (
	// StoreKeyring is the OS store. On macOS its item trusts no
	// application, so every read asks the person (K4).
	StoreKeyring = "keyring"
	// StoreFile is a 0600 file, chosen explicitly with --key-store file.
	StoreFile = "file"
)

// KeyringService names the OS store entries; each one's account is the
// Linear profile ID.
const KeyringService = "clawbridge-linear-api-key"

// FileWarning is said, and confirmed, before a key goes to a file (L2).
const FileWarning = "the Linear API key will be a plain 0600 file: any program running as you can read it, and it acts as you everywhere in Linear"

// ErrNoKey reports a profile with no key saved.
var ErrNoKey = errors.New("no Linear API key is saved for this profile; run clawbridge linear key set")

var (
	keyPattern     = regexp.MustCompile(`^lin_api_[A-Za-z0-9]{16,128}$`)
	profilePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// ValidKey reports whether s has the shape of a Linear personal API key.
func ValidKey(s string) bool { return keyPattern.MatchString(s) }

// Store keeps keys under Dir (ClawBridge's state directory's linear/keys):
// a file key, and each profile's marker naming its store, which lets
// doctor say a key is saved without reading it.
type Store struct{ Dir string }

func (s Store) path(profile, ext string) string { return filepath.Join(s.Dir, profile+ext) }

func validProfile(profile string) error {
	if !profilePattern.MatchString(profile) {
		return errors.New("invalid Linear profile ID")
	}
	return nil
}

// Recorded names the store a profile's key was saved to, from its marker
// alone: the key itself is not read.
func (s Store) Recorded(profile string) (string, bool) {
	if validProfile(profile) != nil {
		return "", false
	}
	b, err := os.ReadFile(s.path(profile, ".store"))
	if err != nil {
		return "", false
	}
	store := strings.TrimSpace(string(b))
	return store, store == StoreKeyring || store == StoreFile
}

// Set saves key for profile, replacing any key it had in either store.
func (s Store) Set(profile, key, store string) error {
	if err := guard(); err != nil {
		return err
	}
	if !ValidKey(key) {
		return errors.New("that is not a Linear personal API key (lin_api_...)")
	}
	if store != StoreKeyring && store != StoreFile {
		return errors.New("--key-store must be keyring or file")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return errors.New("cannot make the Linear key directory")
	}
	if err := checkPrivate(s.Dir, true); err != nil {
		return err
	}
	// Remove also checks the profile ID, before anything is written.
	if err := s.Remove(profile); err != nil {
		return err
	}
	switch store {
	case StoreKeyring:
		if err := osSet(profile, key); err != nil {
			return err
		}
	case StoreFile:
		if err := writePrivate(s.path(profile, ".key"), key+"\n"); err != nil {
			return err
		}
	}
	return writePrivate(s.path(profile, ".store"), store+"\n")
}

// Open reads profile's key and returns a client that sends it.
func (s Store) Open(profile string) (*Client, error) {
	if err := guard(); err != nil {
		return nil, err
	}
	store, ok := s.Recorded(profile)
	if !ok {
		return nil, ErrNoKey
	}
	var key string
	switch store {
	case StoreKeyring:
		v, err := osGet(profile)
		if err != nil {
			return nil, err
		}
		key = v
	case StoreFile:
		if err := checkPrivate(s.Dir, true); err != nil {
			return nil, err
		}
		if err := checkPrivate(s.path(profile, ".key"), false); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(s.path(profile, ".key"))
		if err != nil {
			return nil, errors.New("cannot read the Linear API key file")
		}
		key = strings.TrimSpace(string(b))
	}
	if !ValidKey(key) {
		return nil, errors.New("the saved Linear API key is unreadable; run clawbridge linear key set again")
	}
	return NewClient(key)
}

// NewClient is a client for a key not yet saved, to check it first.
func NewClient(key string) (*Client, error) {
	if err := guard(); err != nil {
		return nil, err
	}
	if !ValidKey(key) {
		return nil, errors.New("that is not a Linear personal API key (lin_api_...)")
	}
	return &Client{key: key, http: httpClient}, nil
}

// Remove deletes profile's key, wherever its marker says it is, and the
// marker; a key already gone is not an error.
func (s Store) Remove(profile string) error {
	// Removing reads nothing, so any process may: linear logout does.
	if err := validProfile(profile); err != nil {
		return err
	}
	// Only a key the marker says is in the OS store is deleted there: a
	// machine without one (a headless Linux) keeps working with files.
	if store, _ := s.Recorded(profile); store == StoreKeyring {
		if err := osDelete(profile); err != nil {
			return err
		}
	}
	for _, ext := range []string{".key", ".store"} {
		if err := os.Remove(s.path(profile, ext)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cannot remove the Linear API key's %s file", strings.TrimPrefix(ext, "."))
		}
	}
	return nil
}

// writePrivate replaces path with content, 0600, through a temporary file.
func writePrivate(path, content string) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("cannot write the Linear API key's files")
	}
	_, err = f.WriteString(content)
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return errors.New("cannot write the Linear API key's files")
	}
	return nil
}
