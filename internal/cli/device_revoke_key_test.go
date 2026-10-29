package cli

import (
	"bytes"
	"strings"
	"testing"

	"clawbridge/internal/devicesign"
	"clawbridge/internal/remote"
)

// JR5: revoking this device offers to remove its key from the key store;
// revoking another device never touches local keys. No real key store.
func TestDeviceRevokeOfThisDeviceHandlesItsLocalKey(t *testing.T) {
	isolated(t)
	home, _ := stateHome()
	cfg := Config{CredentialSource: remote.CredentialSourceDevice, Server: "https://cb.example.test", DeviceID: "dev_self", KeyStore: devicesign.StoreKeyring}
	want := devicesign.Location{Store: devicesign.StoreKeyring, Host: "cb.example.test", Home: home, DeviceID: "dev_self"}
	oldDel, oldTerm, oldOS := deleteDeviceKey, stdinTerminal, keyStoreGOOS
	t.Cleanup(func() { deleteDeviceKey, stdinTerminal, keyStoreGOOS = oldDel, oldTerm, oldOS })
	var deleted []devicesign.Location
	deleteDeviceKey = func(l devicesign.Location) error { deleted = append(deleted, l); return nil }
	keyStoreGOOS = "darwin"
	manual := "security delete-generic-password -s clawbridge-device -a cb.example.test/dev_self"
	for name, c := range map[string]struct {
		device, input string
		terminal      bool
		deleted       int
		say           string
		silent        bool
	}{
		"self yes":     {"dev_self", "y\n", true, 1, "Removed this device's key", false},
		"self no":      {"dev_self", "n\n", true, 0, manual, false},
		"self no tty":  {"dev_self", "y\n", false, 0, manual, false},
		"self default": {"dev_self", "\n", true, 0, manual, false},
		"other device": {"dev_other", "y\n", true, 0, "", true},
	} {
		deleted = nil
		stdinTerminal = func() bool { return c.terminal }
		var errOut bytes.Buffer
		offerLocalKeyRemoval(cfg, c.device, strings.NewReader(c.input), &errOut)
		if len(deleted) != c.deleted || (c.deleted == 1 && deleted[0] != want) {
			t.Fatalf("%s: deleted %v", name, deleted)
		}
		out := errOut.String()
		if c.silent && out != "" || !strings.Contains(out, c.say) {
			t.Fatalf("%s: %q", name, out)
		}
		if c.terminal && !c.silent && !strings.Contains(out, "Remove this device's key from the system key store too? [y/N]") {
			t.Fatalf("%s: no question: %q", name, out)
		}
		if !c.terminal && strings.Contains(out, "[y/N]") {
			t.Fatalf("%s: asked without a terminal", name)
		}
	}
	// The file store names the key file; Linux and Windows name their tools.
	file := cfg
	file.KeyStore = devicesign.StoreFile
	deleted = nil
	stdinTerminal = func() bool { return false }
	var errOut bytes.Buffer
	offerLocalKeyRemoval(file, "dev_self", strings.NewReader(""), &errOut)
	if !strings.Contains(errOut.String(), want.Path()) || len(deleted) != 0 {
		t.Fatal(errOut.String())
	}
	for goos, text := range map[string]string{"linux": "secret-tool clear service clawbridge-device username cb.example.test/dev_self", "windows": "cmdkey /delete:clawbridge-device:cb.example.test/dev_self"} {
		keyStoreGOOS = goos
		errOut.Reset()
		offerLocalKeyRemoval(cfg, "dev_self", strings.NewReader(""), &errOut)
		if !strings.Contains(errOut.String(), text) {
			t.Fatal(goos, errOut.String())
		}
	}
}

// Through the command: only a successful revoke of this device reaches it.
func TestDeviceRevokeCommandOffersTheKeyOnlyForThisDevice(t *testing.T) {
	path := googleProfile(t)
	_ = path
	t.Setenv("CLAWBRIDGE_CREDENTIAL_SOURCE", "device")
	t.Setenv("CLAWBRIDGE_DEVICE_ID", "dev_self")
	t.Setenv("CLAWBRIDGE_KEY_STORE", devicesign.StoreKeyring)
	oldDel, oldTerm, oldIn := deleteDeviceKey, stdinTerminal, accountStdin
	t.Cleanup(func() { deleteDeviceKey, stdinTerminal, accountStdin = oldDel, oldTerm, oldIn })
	var deleted []devicesign.Location
	deleteDeviceKey = func(l devicesign.Location) error { deleted = append(deleted, l); return nil }
	stdinTerminal = func() bool { return true }
	for _, c := range []struct {
		device  string
		fail    bool
		deleted int
	}{{"dev_other", false, 0}, {"dev_self", true, 0}, {"dev_self", false, 1}} {
		deleted = nil
		accountStdin = strings.NewReader("y\n")
		r := &accountRecorder{}
		if c.fail {
			r.failOp = "device_revoke"
		}
		withAccountRecorder(t, r)
		_, _ = run("device", "revoke", c.device)
		if len(deleted) != c.deleted {
			t.Fatalf("%+v: deleted %v", c, deleted)
		}
	}
}
