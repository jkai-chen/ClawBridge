//go:build !darwin

package linearkey

import (
	"errors"
	"runtime"

	"github.com/zalando/go-keyring"
)

// Elsewhere the key is kept as the device key is: Windows Credential
// Manager, or the Secret Service on Linux, through go-keyring, which
// passes it through the platform API, never a command line.

func keyringErr(operation string) error {
	msg := "cannot " + operation + " the Linear API key in the OS key store"
	if runtime.GOOS == "linux" {
		msg += " (the Secret Service: GNOME Keyring or KWallet)"
	}
	return errors.New(msg + "; unlock it, or use --key-store file")
}

func osSet(profile, key string) error {
	if keyring.Set(KeyringService, profile, key) != nil {
		return keyringErr("save")
	}
	return nil
}

func osGet(profile string) (string, error) {
	v, err := keyring.Get(KeyringService, profile)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNoKey
	}
	if err != nil {
		return "", keyringErr("read")
	}
	return v, nil
}

func osDelete(profile string) error {
	if err := keyring.Delete(KeyringService, profile); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return keyringErr("remove")
	}
	return nil
}
