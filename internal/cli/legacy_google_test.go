package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/auth"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/notify"
	"clawbridge/internal/remote"
	"clawbridge/internal/server"
	"github.com/zalando/go-keyring"
)

// Every command that needed Google sign-in (removed in v1.0) ends with exit
// 2 and the swap runbook's own text, before reading any profile.
func TestGoogleCommandsRefused(t *testing.T) {
	isolated(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"device", "enroll"}, "clawbridge device enroll: Google sign-in was removed in v1.0; a new machine joins with an invite: ask an owner for an invite (or a recovery invite) and run clawbridge join; see docs/operations/google-sign-in-removal.md"},
		{[]string{"device", "enroll", "--label", "laptop"}, "clawbridge device enroll: Google sign-in was removed in v1.0; a new machine joins with an invite: ask an owner for an invite (or a recovery invite) and run clawbridge join; see docs/operations/google-sign-in-removal.md"},
		{[]string{"login"}, "clawbridge login: Google sign-in was removed in v1.0; a new machine joins with an invite: ask an owner for an invite (or a recovery invite) and run clawbridge join; see docs/operations/google-sign-in-removal.md"},
		{[]string{"login", "--device"}, "clawbridge login: Google sign-in was removed in v1.0; a new machine joins with an invite: ask an owner for an invite (or a recovery invite) and run clawbridge join; see docs/operations/google-sign-in-removal.md"},
		{[]string{"logout"}, "clawbridge logout: Google sign-in was removed in v1.0; a new machine joins with an invite: ask an owner for an invite (or a recovery invite) and run clawbridge join; see docs/operations/google-sign-in-removal.md"},
		{[]string{"setup", "join", "--server", "https://cb.example.test"}, "clawbridge setup join: Google sign-in was removed in v1.0; a new machine joins with an invite: ask an owner for an invite (or a recovery invite) and run clawbridge join; see docs/operations/google-sign-in-removal.md"},
	} {
		err := Run(context.Background(), c.args, &bytes.Buffer{}, &bytes.Buffer{})
		var exit *ExitCodeError
		if !errors.As(err, &exit) || exit.Code != 2 || !exit.Whole || err.Error() != c.want {
			t.Errorf("%v: %#v", c.args, err)
		}
	}
	// The usage says the same.
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"device", "--help"}, &out, &bytes.Buffer{}); err != nil || !strings.Contains(out.String(), "device enroll   (removed in v1.0 with Google sign-in") {
		t.Fatal(err, out.String())
	}
}

// A start of serve that names Google exits 2 with the server's own text.
func TestServeGoogleStartExitsTwo(t *testing.T) {
	isolated(t)
	err := Run(context.Background(), []string{"serve", "--auth", "google", "--public-origin", "https://cb.example.test", "--bootstrap-owner", "owner@example.test"}, &bytes.Buffer{}, &bytes.Buffer{})
	var exit *ExitCodeError
	if !errors.As(err, &exit) || exit.Code != 2 || !exit.Whole || err.Error() != "clawbridge serve: Google sign-in was removed in v1.0; set CLAWBRIDGE_AUTH=device after every member has an active device (docs/operations/google-sign-in-removal.md#before-you-switch)" {
		t.Fatalf("%#v", err)
	}
	// Every form exits 2 with the same text: a space, any case, and
	// CLAWBRIDGE_AUTH as well as --auth.
	for _, args := range [][]string{{"--auth", "google device"}, {"--auth", "Google,Device"}, {"--auth", "device GOOGLE"}} {
		err = Run(context.Background(), append(append([]string{"serve"}, args...), "--public-origin", "https://cb.example.test", "--bootstrap-owner", "owner@example.test"), &bytes.Buffer{}, &bytes.Buffer{})
		if !errors.As(err, &exit) || exit.Code != 2 || !exit.Whole || err.Error() != server.GoogleAuthRefused {
			t.Errorf("%v: %#v", args, err)
		}
	}
	for _, value := range []string{"google", "google device", "GOOGLE,device"} {
		t.Setenv("CLAWBRIDGE_AUTH", value)
		err = Run(context.Background(), []string{"serve", "--public-origin", "https://cb.example.test", "--bootstrap-owner", "owner@example.test"}, &bytes.Buffer{}, &bytes.Buffer{})
		if !errors.As(err, &exit) || exit.Code != 2 || !exit.Whole || err.Error() != server.GoogleAuthRefused {
			t.Errorf("CLAWBRIDGE_AUTH=%q: %#v", value, err)
		}
	}
	t.Setenv("CLAWBRIDGE_AUTH", "")
	err = Run(context.Background(), []string{"serve", "--audience", "x", "--public-origin", "https://cb.example.test", "--bootstrap-owner", "owner@example.test"}, &bytes.Buffer{}, &bytes.Buffer{})
	if !errors.As(err, &exit) || exit.Code != 2 || err.Error() != "clawbridge serve: --audience was for Google sign-in, removed in v1.0 (docs/operations/google-sign-in-removal.md#before-you-switch)" {
		t.Fatalf("%#v", err)
	}
}

// Google sign-in's global flags are refused with how to join, and never
// echo their value.
func TestGoogleGlobalFlagsRefused(t *testing.T) {
	for _, flag := range []string{"--audience", "--impersonate-service-account", "--credentials-file", "--oauth-client-id", "--oauth-client-secret", "--oauth-device-client-id", "--oauth-device-client-secret", "--oauth-keyring-service"} {
		for _, args := range [][]string{{flag, "synthetic-value", "whoami"}, {flag + "=synthetic-value", "whoami"}} {
			_, _, err := extractGlobals(args)
			if err == nil || !strings.Contains(err.Error(), flag+" was for Google sign-in, removed in v1.0") || strings.Contains(err.Error(), "synthetic-value") {
				t.Errorf("%v: %v", args, err)
			}
		}
	}
	// --account is register's and update's own flag, not a global one.
	if _, rest, err := extractGlobals([]string{"register", "--account", "acct"}); err != nil || strings.Join(rest, " ") != "register --account acct" {
		t.Fatal(rest, err)
	}
}

// A profile written before v1.0 loads with one warning line naming its
// Google keys (never their values); the refresh token a Google sign-in
// stored in the OS keychain is deleted once; a profile that still signs in
// with Google says how to join.
func TestLegacyGoogleProfile(t *testing.T) {
	dir := isolated(t)
	home := filepath.Join(dir, "state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	var notices bytes.Buffer
	oldOut, oldDelete := legacyNotices, deleteLegacyCredential
	t.Cleanup(func() { legacyNotices, deleteLegacyCredential = oldOut, oldDelete })
	legacyNotices = &notices
	var deleted []string
	deleteLegacyCredential = func(_ context.Context, service, client string) (bool, error) {
		deleted = append(deleted, service+"/"+client)
		return client == "desktop.example.test", nil
	}
	enrolled := "mode='cloud'\nserver='https://cb.example.test'\nworkspace='acme'\nprincipal='ada@example.com'\ncache='cache.db'\ncredential_source='device'\ndevice_id='dev_abcdefghjkmnpqrstvwxyz0123'\nkey_store='keyring'\n" +
		"audience='desktop.example.test'\noauth_client_id='desktop.example.test'\noauth_client_secret='synthetic-browser-secret'\noauth_device_client_id='device.example.test'\noauth_device_client_secret='synthetic-device-secret'\n"
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte(enrolled), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _, err := resolveConfig(context.Background(), Config{})
	if err != nil || c.CredentialSource != "device" {
		t.Fatal("an old device profile with Google keys did not load:", err)
	}
	if _, _, err = resolveConfig(context.Background(), Config{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(notices.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "warning: "+path+" has Google sign-in settings (audience, oauth_client_id, oauth_client_secret, oauth_device_client_id, oauth_device_client_secret), unused since v1.0") ||
		lines[1] != "Deleted the old Google sign-in credential from the OS keychain (service "+auth.LegacyKeyringService+")." || strings.Contains(notices.String(), "synthetic-") {
		t.Fatalf("notices: %q", notices.String())
	}
	if strings.Join(deleted, ",") != auth.LegacyKeyringService+"/desktop.example.test,"+auth.LegacyKeyringService+"/device.example.test" {
		t.Fatal("keychain deletes:", deleted)
	}
	// Once: another process (a fresh warning) doesn't delete again.
	legacyWarned.Delete(path)
	notices.Reset()
	if _, _, err = resolveConfig(context.Background(), Config{}); err != nil || len(deleted) != 2 || strings.Count(notices.String(), "\n") != 1 {
		t.Fatal("the cleanup ran twice:", deleted, notices.String())
	}
	// A failed delete says what to remove by hand, without the value.
	legacyWarned.Delete(path)
	_ = os.Remove(filepath.Join(home, legacyCleanupMarker))
	notices.Reset()
	deleteLegacyCredential = func(context.Context, string, string) (bool, error) { return false, errors.New("locked") }
	if _, _, err = resolveConfig(context.Background(), Config{}); err != nil || !strings.Contains(notices.String(), "could not delete the old Google sign-in credential from the OS keychain (service clawbridge-oauth, account "+auth.LegacyKey("desktop.example.test")+"); remove it by hand") {
		t.Fatal(notices.String())
	}
	// A profile that never moved to a device key is refused, saying how to
	// join.
	for _, source := range []string{"oauth", "gcloud", "adc", ""} {
		body := "mode='cloud'\nserver='https://cb.example.test'\nprincipal='ada@example.com'\ncache='cache.db'\ncredential_source='" + source + "'\n"
		if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err = resolveConfig(context.Background(), Config{}); err == nil || err.Error() != remote.GoogleRouteRemoved {
			t.Errorf("%q: %v", source, err)
		}
	}
}

// useDeviceRoute makes the environment's cloud profile sign with a fresh
// device key for server, kept in a mock keyring: the only sign-in since
// v1.0.
func useDeviceRoute(t *testing.T, server string) {
	t.Helper()
	keyring.MockInit()
	key, id, err := devicesign.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	if err = devicesign.Save(devicesign.Location{Store: devicesign.StoreKeyring, Host: u.Host, DeviceID: id}, key); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWBRIDGE_CREDENTIAL_SOURCE", "device")
	t.Setenv("CLAWBRIDGE_DEVICE_ID", id)
	t.Setenv("CLAWBRIDGE_KEY_STORE", devicesign.StoreKeyring)
}

// The swap (docs/operations/google-sign-in-removal.md): a member whose
// profile still signs in with Google is refused with how to join, joins
// with an invite into the same profile, and then signs with the device key;
// the server, device keys only, refuses a Bearer token with its own text.
func TestGoogleProfileJoinsWithAnInvite(t *testing.T) {
	f := newJoinFixture(t)
	home, _ := stateHome()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	old := "mode = \"cloud\"\nserver = \"" + f.url + "\"\nworkspace = \"acme\"\nprincipal = \"ada@example.com\"\ncache = \"cache.db\"\ncredential_source = \"oauth\"\naudience = \"desktop.example.test\"\noauth_client_id = \"desktop.example.test\"\noauth_client_secret = \"synthetic-browser-secret\"\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("whoami"); err == nil || !strings.Contains(err.Error(), remote.GoogleRouteRemoved) {
		t.Fatal("a Google profile signed in:", err)
	}
	if _, err := run("--server", f.url, "join", "--email", "ada@example.com", "--code", "CBJ-4K7P-WQ9D-2MXH-T6RB"); err != nil {
		t.Fatal(err)
	}
	if p := profile(t, path); p["credential_source"] != "device" || p["device_id"] != f.keys[0] {
		t.Fatal("the profile didn't switch to the key:", p)
	}
	if out, err := run("device", "list"); err != nil || len(f.exec.ids) != 1 || f.exec.ids[0].Subject != f.keys[0] {
		t.Fatal("the new key didn't sign:", out, err)
	}
	req, _ := http.NewRequest(http.MethodPost, f.url+"/v1/execute", strings.NewReader(`{"operation":"whoami"}`))
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiJ9.e30.c2ln")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Error struct{ Code, Message string }
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	if res.StatusCode != 401 || body.Error.Code != "unauthenticated" || body.Error.Message != server.BearerRefused {
		t.Fatal(res.StatusCode, body)
	}
}

// A profile migrated to a device key before v1.0 (by device enroll or join)
// kept its Google keys, which were part of the notify-state binding. The
// binding is unchanged after the upgrade, so its state is still the
// notifier's: no "corrupt or mismatched" refusal and no reset.
func TestMigratedProfileKeepsItsNotifyBinding(t *testing.T) {
	dir := isolated(t)
	home := filepath.Join(dir, "state")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	profile := "mode = 'cloud'\nserver = 'https://cb.example.test'\nprincipal = 'ada@example.com'\ncache = 'cache.db'\n" +
		"credential_source = 'device'\ndevice_id = 'dev_abcdefghjkmnpqrstvwxyz0123'\nkey_store = 'keyring'\n" +
		"audience = 'desktop.example.test'\noauth_client_id = 'desktop.example.test'\noauth_client_secret = 'synthetic-secret'\noauth_keyring_service = 'review-keyring'\n"
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	// The binding v0.3.13's notify computed for this profile.
	old, _ := json.Marshal([]string{"cloud", "https://cb.example.test", "desktop.example.test", "device", "ada@example.com", "", "", "desktop.example.test", "review-keyring", "", "dev_abcdefghjkmnpqrstvwxyz0123"})
	digest := sha256.Sum256(old)
	want := notify.Binding{Authority: hex.EncodeToString(digest[:]), Principal: "ada@example.com", Workspace: "acme", Project: "api", Session: "agent", Instance: "inst-1", Adapter: "json"}
	saved, savedExec := runNotifyWatcher, newNotifyExecutor
	t.Cleanup(func() { runNotifyWatcher, newNotifyExecutor = saved, savedExec })
	newNotifyExecutor = func(Config) (notify.Executor, func(), error) { return nil, func() {}, nil }
	var got notify.Binding
	runNotifyWatcher = func(_ context.Context, c notify.Config) error { got = c.Binding; return nil }
	args := []string{"--workspace", "acme", "--project", "api", "--session", "agent", "--instance", "inst-1", "notify", "--adapter", "json", "--state", filepath.Join(dir, "notify.json")}
	if err := Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the migrated profile's binding changed:\n got %+v\nwant %+v", got, want)
	}
	// The legacy environment variables count as they did.
	t.Setenv("CLAWBRIDGE_AUDIENCE", "env-audience.example.test")
	if err := Run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err != nil || got.Authority == want.Authority {
		t.Fatal("the environment's audience no longer counts:", err)
	}
}
