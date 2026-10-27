//go:build darwin

package linearkey

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// K6: on macOS the key goes to the keychain only on `security -i`'s
// stdin; no command line carries it. Reads and removes name only the
// service and the profile.
func TestKeychainKeepsTheKeyOffTheCommandLine(t *testing.T) {
	noAgent(t)
	type call struct {
		args  []string
		stdin string
	}
	var calls []call
	stored := ""
	old := security
	security = func(args []string, stdin string) ([]byte, error) {
		calls = append(calls, call{append([]string{}, args...), stdin})
		switch {
		case len(args) == 1 && args[0] == "-i":
			stored = strings.Fields(stdin)[len(strings.Fields(stdin))-1]
			return nil, nil
		case args[0] == "find-generic-password":
			if stored == "" {
				return []byte("security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain."), errors.New("exit 44")
			}
			return []byte(stored + "\n"), nil
		case args[0] == "delete-generic-password":
			stored = ""
		}
		return nil, nil
	}
	t.Cleanup(func() { security = old })
	s := Store{Dir: filepath.Join(t.TempDir(), "keys")}
	if err := s.Set("profile-1", testKey, StoreKeyring); err != nil {
		t.Fatal(err)
	}
	c, err := s.Open("profile-1")
	if err != nil || c.key != testKey {
		t.Fatal(err)
	}
	if err = s.Set("profile-1", testKey, StoreKeyring); err != nil {
		t.Fatal(err)
	}
	if err = s.Remove("profile-1"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Open("profile-1"); !errors.Is(err, ErrNoKey) {
		t.Fatal(err)
	}
	sawStdin := false
	for _, c := range calls {
		if strings.Contains(strings.Join(c.args, " "), testKey) {
			t.Fatal("the key was on a command line:", c.args)
		}
		sawStdin = sawStdin || strings.Contains(c.stdin, "add-generic-password -U -s "+KeyringService+" -a profile-1 -w "+testKey)
	}
	if !sawStdin {
		t.Fatal("the key never went on stdin:", calls)
	}
	// The second set removed the first item before adding again.
	deletes := 0
	for _, c := range calls {
		if c.args[0] == "delete-generic-password" {
			deletes++
		}
	}
	if deletes != 2 {
		t.Fatal("deletes:", deletes)
	}
}
