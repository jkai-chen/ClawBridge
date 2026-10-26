// Package devicesign is the client side of ClawBridge's own sign-in
// (live/AUTH-INVITE-DESIGN.md §4, §5, §7): where a device's private key is
// kept, and the signature every hosted request carries.
package devicesign

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"clawbridge/internal/devicekey"
	"github.com/zalando/go-keyring"
)

// The places a device key may be kept. The names are the server's
// key_store values.
const (
	// StoreKeyring is the OS store: the macOS Keychain, Windows Credential
	// Manager, or the Secret Service on Linux.
	StoreKeyring = "keyring"
	// StoreFile is a plain file, 0600 in a 0700 directory, chosen
	// explicitly with --key-store file.
	StoreFile = "file"
	// StoreEnv is a CI device's key, read only from EnvKey.
	StoreEnv = "env"
)

// KeyringService names the OS store entries; each entry's account is the
// server's host and the device ID.
const KeyringService = "clawbridge-device"

// EnvKey holds a CI device's private key. It is read from the environment
// only, never from a file the client might commit.
const EnvKey = "CLAWBRIDGE_DEVICE_KEY"

// FileWarning is said whenever a key goes to a plain file.
const FileWarning = "the device key is a plain file: anyone who can read it as you can act as you"

// ErrNoKey reports a device whose key this store doesn't hold.
var ErrNoKey = errors.New("this device's key is not in its store; join again with an invite")

// Location says where one device's key lives.
type Location struct {
	// Store is StoreKeyring, StoreFile or StoreEnv.
	Store string
	// Host is the server's host (and port), part of the OS store's account.
	Host string
	// Home is the ClawBridge state directory; a file key is under its
	// device directory.
	Home string
	// DeviceID is the device the key must derive.
	DeviceID string
}

// ValidStore reports whether s names a store.
func ValidStore(s string) bool { return s == StoreKeyring || s == StoreFile || s == StoreEnv }

func (l Location) account() string { return l.Host + "/" + l.DeviceID }

func (l Location) dir() string { return filepath.Join(l.Home, "device") }

// Path is a file key's path.
func (l Location) Path() string { return filepath.Join(l.dir(), l.DeviceID+".key") }

// NewKey makes a device key and derives its ID.
func NewKey() (ed25519.PrivateKey, string, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, "", errors.New("cannot make a device key")
	}
	return priv, devicekey.ID(pub), nil
}

// EncodeKey is a private key's stored form: its 32-byte seed in unpadded
// base64url.
func EncodeKey(priv ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(priv.Seed())
}

// ParseKey reads EncodeKey's form.
func ParseKey(s string) (ed25519.PrivateKey, error) {
	seed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("a device key must be a 32-byte Ed25519 seed in unpadded base64url")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// keyringErr names a failed OS store use, with the way out.
func keyringErr(operation string) error {
	msg := "cannot " + operation + " the device key in the OS key store"
	if runtime.GOOS == "linux" {
		msg += " (the Secret Service: GNOME Keyring or KWallet)"
	}
	return errors.New(msg + "; unlock it, or on a machine without one join with --key-store file (" + FileWarning + ")")
}

// Save stores a new key, then reads it back and checks it before anything
// is sent. A file key is never overwritten.
func Save(l Location, priv ed25519.PrivateKey) error {
	if devicekey.ID(priv.Public().(ed25519.PublicKey)) != l.DeviceID {
		return errors.New("the key doesn't derive this device ID")
	}
	switch l.Store {
	case StoreKeyring:
		if err := keyring.Set(KeyringService, l.account(), EncodeKey(priv)); err != nil {
			return keyringErr("save")
		}
	case StoreFile:
		if err := os.MkdirAll(l.dir(), 0o700); err != nil {
			return fmt.Errorf("cannot make the device key directory: %w", err)
		}
		if err := checkPrivate(l.dir(), true); err != nil {
			return err
		}
		f, err := os.OpenFile(l.Path(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("cannot write the device key file: %w", err)
		}
		_, err = f.WriteString(EncodeKey(priv) + "\n")
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			_ = os.Remove(l.Path())
			return fmt.Errorf("cannot write the device key file: %w", err)
		}
	default:
		return errors.New("a device key is saved to the keyring or a file")
	}
	back, err := Load(l)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(back, priv) != 1 {
		return errors.New("the device key read back differs from the one saved")
	}
	return nil
}

// Load reads a device's key and checks that it derives the device's ID. A
// file key is refused if it or its directory is open to others, or owned by
// another user.
func Load(l Location) (ed25519.PrivateKey, error) {
	var raw string
	switch l.Store {
	case StoreKeyring:
		v, err := keyring.Get(KeyringService, l.account())
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, ErrNoKey
		}
		if err != nil {
			return nil, keyringErr("read")
		}
		raw = v
	case StoreFile:
		if err := checkPrivate(l.dir(), true); err != nil {
			return nil, err
		}
		if err := checkPrivate(l.Path(), false); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(l.Path())
		if err != nil {
			return nil, fmt.Errorf("cannot read the device key file: %w", err)
		}
		raw = string(b)
	case StoreEnv:
		raw = os.Getenv(EnvKey)
		if raw == "" {
			return nil, errors.New(EnvKey + " is not set")
		}
	default:
		return nil, errors.New("key_store must be keyring, file or env")
	}
	priv, err := ParseKey(raw)
	if err != nil {
		return nil, err
	}
	if id := devicekey.ID(priv.Public().(ed25519.PublicKey)); l.DeviceID != "" && id != l.DeviceID {
		return nil, errors.New("the stored key belongs to another device (" + id + ")")
	}
	return priv, nil
}

// Delete removes a device's key; one already gone is not an error. A CI
// key is the environment's, and is left alone.
func Delete(l Location) error {
	switch l.Store {
	case StoreKeyring:
		if err := keyring.Delete(KeyringService, l.account()); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return keyringErr("remove")
		}
	case StoreFile:
		if err := os.Remove(l.Path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("cannot remove the device key file: %w", err)
		}
	}
	return nil
}

// CheckFile reports a file key's mode problem, for doctor.
func CheckFile(l Location) error {
	if err := checkPrivate(l.dir(), true); err != nil {
		return err
	}
	return checkPrivate(l.Path(), false)
}
