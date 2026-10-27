//go:build darwin

package linearkey

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
)

// On macOS the key is kept as the device key is (go-keyring's form: an
// item made by /usr/bin/security, owner's decision 27 Oct 21:01Z), but
// written here so that the key goes only on `security -i`'s stdin, never
// in a command line another process can list (K6), and a test can prove it.

// security runs /usr/bin/security with args and stdin; tests replace it.
var security = func(args []string, stdin string) ([]byte, error) {
	cmd := exec.Command("/usr/bin/security", args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

func keychainErr(operation string) error {
	return errors.New("cannot " + operation + " the Linear API key in the macOS keychain; unlock it, or use --key-store file")
}

func osSet(profile, key string) error {
	// The key and the profile ID are checked to letters, digits, dot,
	// underscore and hyphen, so the line needs no quoting.
	if _, err := security([]string{"-i"}, "add-generic-password -U -s "+KeyringService+" -a "+profile+" -w "+key+"\n"); err != nil {
		return keychainErr("save")
	}
	return nil
}

func osGet(profile string) (string, error) {
	out, err := security([]string{"find-generic-password", "-s", KeyringService, "-a", profile, "-w"}, "")
	if err != nil {
		if bytes.Contains(out, []byte("could not be found")) {
			return "", ErrNoKey
		}
		return "", keychainErr("read")
	}
	return strings.TrimSpace(string(out)), nil
}

func osDelete(profile string) error {
	out, err := security([]string{"delete-generic-password", "-s", KeyringService, "-a", profile}, "")
	if err != nil && !bytes.Contains(out, []byte("could not be found")) {
		return keychainErr("remove")
	}
	return nil
}
