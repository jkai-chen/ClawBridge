package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/auth"
	"clawbridge/internal/remote"
)

type loginMemoryStore struct {
	values map[string]auth.Credential
	saves  int
	logins []auth.Config
}

func (s *loginMemoryStore) Load(_ context.Context, key string) (auth.Credential, error) {
	value, ok := s.values[key]
	if !ok {
		return auth.Credential{}, auth.ErrCredentialNotFound
	}
	return value, nil
}

func (s *loginMemoryStore) Save(_ context.Context, key string, value auth.Credential) error {
	if s.values == nil {
		s.values = map[string]auth.Credential{}
	}
	s.values[key] = value
	s.saves++
	return nil
}

func (s *loginMemoryStore) Delete(_ context.Context, key string) error {
	delete(s.values, key)
	return nil
}

func stageLoginCredential(t *testing.T, store auth.Store, clientID, principal, refresh string) *auth.CredentialTransaction {
	t.Helper()
	transaction, err := auth.StageCredential(context.Background(), store, clientID, auth.Credential{
		ClientID: clientID, Principal: principal, Subject: "subject", RefreshToken: refresh,
	})
	if err != nil {
		t.Fatal(err)
	}
	return transaction
}

func installLoginTestHooks(t *testing.T, store auth.Store, credential auth.Credential) {
	t.Helper()
	originalBrowser, originalDevice := browserLoginStaged, deviceLoginStaged
	originalStore, originalWrite := loginCredentialStore, writeLoginConfig
	originalDiscovery := discoverLoginOAuth
	t.Cleanup(func() {
		browserLoginStaged, deviceLoginStaged = originalBrowser, originalDevice
		loginCredentialStore, writeLoginConfig = originalStore, originalWrite
		discoverLoginOAuth = originalDiscovery
	})
	loginCredentialStore = func(string) auth.Store { return store }
	browserLoginStaged = func(ctx context.Context, config auth.Config) (auth.Credential, *auth.CredentialTransaction, error) {
		if memory, ok := store.(*loginMemoryStore); ok {
			memory.logins = append(memory.logins, config)
		}
		if config.ExpectedPrincipal != credential.Principal && config.ExpectedPrincipal != "" {
			return auth.Credential{}, nil, errors.New("the signed-in Google principal does not match the trusted principal; no credential was saved")
		}
		return credential, stageLoginCredential(t, config.Store, config.ClientID, credential.Principal, credential.RefreshToken), nil
	}
	deviceLoginStaged = func(ctx context.Context, config auth.Config, prompt func(auth.DevicePrompt)) (auth.Credential, *auth.CredentialTransaction, error) {
		if memory, ok := store.(*loginMemoryStore); ok {
			memory.logins = append(memory.logins, config)
		}
		return credential, stageLoginCredential(t, config.Store, config.ClientID, credential.Principal, credential.RefreshToken), nil
	}
}

func failLoginAcquisition(t *testing.T) {
	t.Helper()
	browserLoginStaged = func(context.Context, auth.Config) (auth.Credential, *auth.CredentialTransaction, error) {
		t.Fatal("OAuth browser acquisition started")
		return auth.Credential{}, nil, nil
	}
	deviceLoginStaged = func(context.Context, auth.Config, func(auth.DevicePrompt)) (auth.Credential, *auth.CredentialTransaction, error) {
		t.Fatal("OAuth device acquisition started")
		return auth.Credential{}, nil, nil
	}
}

func writeCloudLoginConfig(t *testing.T) string {
	t.Helper()
	dir := isolated(t)
	home := filepath.Join(dir, "state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	body := "mode='cloud'\nserver='https://trusted.example.test'\naudience='audience'\nprincipal='alice@example.test'\ncache='cache.db'\ncredential_source='oauth'\noauth_client_id='desktop-client.apps.googleusercontent.com'\noauth_client_secret='must-not-be-logged'\noauth_device_client_id='device-client.apps.googleusercontent.com'\noauth_device_client_secret='device-client-credential'\noauth_keyring_service='review-keyring'\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runLogin(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, diagnostics bytes.Buffer
	err := Run(context.Background(), args, &out, &diagnostics)
	return out.String(), diagnostics.String(), err
}

func TestLoginConfigFailureDeletesNewCredentialAndRestoresPriorEntry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal string
		initial   *auth.Credential
		deletes   bool
	}{
		{name: "new-entry", principal: "alice@example.test", deletes: true},
		{name: "replaced-entry", principal: "alice@example.test", initial: &auth.Credential{
			Version: 1, ClientID: "desktop-client.apps.googleusercontent.com",
			Principal: "alice@example.test", Subject: "old", RefreshToken: "prior-refresh",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = writeCloudLoginConfig(t)
			store := &loginMemoryStore{}
			key := auth.Key("desktop-client.apps.googleusercontent.com")
			if tc.initial != nil {
				store.values = map[string]auth.Credential{key: *tc.initial}
			}
			installLoginTestHooks(t, store, auth.Credential{
				ClientID:  "desktop-client.apps.googleusercontent.com",
				Principal: tc.principal, Subject: "new", RefreshToken: "new-refresh",
			})
			writeLoginConfig = func(string, Config) error { return errors.New("synthetic trusted write failure") }
			out, diagnostics, err := runLogin(t, "login")
			if err == nil || out != "" || strings.Contains(diagnostics, "logged_in") {
				t.Fatalf("failed login reported success: out=%q err=%v", out, err)
			}
			if !strings.Contains(err.Error(), "keychain state were preserved") {
				t.Fatalf("unexpected rollback error: %v", err)
			}
			if tc.deletes {
				if _, ok := store.values[key]; ok {
					t.Fatal("new keychain entry survived config write failure")
				}
			} else if store.values[key].RefreshToken != tc.initial.RefreshToken {
				t.Fatalf("prior credential was not restored: %#v", store.values[key])
			}
		})
	}
}

func TestLoginPrincipalMismatchFailsBeforeKeychainSave(t *testing.T) {
	path := writeCloudLoginConfig(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store := &loginMemoryStore{}
	installLoginTestHooks(t, store, auth.Credential{
		ClientID:  "desktop-client.apps.googleusercontent.com",
		Principal: "bob@example.test", Subject: "wrong", RefreshToken: "synthetic-wrong-refresh",
	})
	out, diagnostics, err := runLogin(t, "login")
	if err == nil || out != "" || !strings.Contains(err.Error(), "does not match the trusted principal") {
		t.Fatalf("principal mismatch did not fail closed: out=%q diag=%q err=%v", out, diagnostics, err)
	}
	if store.saves != 0 || len(store.values) != 0 {
		t.Fatal("principal mismatch saved a keychain credential")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatal("principal mismatch changed trusted configuration")
	}
	for _, secret := range []string{"must-not-be-logged", "device-client-credential", "synthetic-wrong-refresh"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(diagnostics, secret) {
			t.Fatal("principal mismatch output exposed credential material")
		}
	}
}

func TestDeviceLoginPromotesDeviceClientAndCredential(t *testing.T) {
	path := writeCloudLoginConfig(t)
	store := &loginMemoryStore{}
	credential := auth.Credential{
		ClientID:  "device-client.apps.googleusercontent.com",
		Principal: "alice@example.test", Subject: "new", RefreshToken: "device-refresh",
	}
	installLoginTestHooks(t, store, credential)
	out, _, err := runLogin(t, "login", "--device")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"logged_in":true`) || !strings.Contains(out, "device-client.apps.googleusercontent.com") {
		t.Fatalf("device login output=%s", out)
	}
	deviceKey := auth.Key("device-client.apps.googleusercontent.com")
	if store.values[deviceKey].RefreshToken != "device-refresh" {
		t.Fatal("credential was not keyed under the device client")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config := string(raw)
	if !strings.Contains(config, "oauth_client_id = 'device-client.apps.googleusercontent.com'") ||
		!strings.Contains(config, "oauth_device_client_id = 'device-client.apps.googleusercontent.com'") ||
		!strings.Contains(config, "oauth_client_secret = 'device-client-credential'") ||
		!strings.Contains(config, "oauth_device_client_secret = 'device-client-credential'") {
		t.Fatalf("device client was not promoted: %s", config)
	}
	if strings.Contains(config, "must-not-be-logged") {
		t.Fatalf("Desktop client secret survived device promotion: %s", config)
	}
	if strings.Contains(out, "must-not-be-logged") || strings.Contains(out, "device-refresh") {
		t.Fatalf("login output exposed secret material: %s", out)
	}
	out, _, err = runLogin(t, "logout")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"client_id":"device-client.apps.googleusercontent.com"`) {
		t.Fatalf("logout did not use the promoted active client: %s", out)
	}
	if _, ok := store.values[deviceKey]; ok {
		t.Fatal("logout did not remove the promoted device client credential")
	}
}

func TestBrowserLoginRetainsDesktopSecretWhenOriginallyConfigured(t *testing.T) {
	path := writeCloudLoginConfig(t)
	store := &loginMemoryStore{}
	installLoginTestHooks(t, store, auth.Credential{
		ClientID:  "desktop-client.apps.googleusercontent.com",
		Principal: "alice@example.test", Subject: "new", RefreshToken: "browser-refresh",
	})
	if _, _, err := runLogin(t, "login"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "oauth_client_secret = 'must-not-be-logged'") ||
		!strings.Contains(string(raw), "oauth_device_client_secret = 'device-client-credential'") {
		t.Fatalf("browser login removed the configured Desktop client secret: %s", raw)
	}
}

func TestCredentialConfigWriteDeletesClearedInstalledAppCredentials(t *testing.T) {
	path := writeCloudLoginConfig(t)
	err := writeCredentialSourceConfig(path, Config{
		Mode: "cloud", Server: "https://trusted.example.test",
		Principal: "alice@example.test", Cache: "cache.db",
		CredentialSource:    "oauth",
		OAuthClientID:       "desktop-client.apps.googleusercontent.com",
		Audience:            "desktop-client.apps.googleusercontent.com",
		OAuthDeviceClientID: "device-client.apps.googleusercontent.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, cleared := range []string{
		"oauth_client_secret", "oauth_device_client_secret",
		"must-not-be-logged", "device-client-credential",
	} {
		if strings.Contains(string(raw), cleared) {
			t.Fatalf("cleared installed-app credential %q survived config promotion", cleared)
		}
	}
}

func TestBrowserLoginDiscoversClientAndPromotesAudience(t *testing.T) {
	path := writeCloudLoginConfig(t)
	body := "mode='cloud'\nserver='https://trusted.example.test'\nprincipal='alice@example.test'\ncache='cache.db'\ncredential_source='oauth'\noauth_client_secret='browser-client-secret'\noauth_device_client_id='device-client.apps.googleusercontent.com'\noauth_keyring_service='review-keyring'\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	store := &loginMemoryStore{}
	installLoginTestHooks(t, store, auth.Credential{
		ClientID:  "browser-client.apps.googleusercontent.com",
		Principal: "alice@example.test", Subject: "new", RefreshToken: "browser-refresh",
	})
	var discoveredServer string
	discoverLoginOAuth = func(_ context.Context, server string, _ *http.Client) (remote.OAuthDiscovery, error) {
		discoveredServer = server
		return remote.OAuthDiscovery{
			BrowserClientID:     "browser-client.apps.googleusercontent.com",
			BrowserClientSecret: "discovered-browser-credential",
			DeviceClientID:      "device-client.apps.googleusercontent.com",
			DeviceClientSecret:  "discovered-device-credential",
		}, nil
	}
	out, _, err := runLogin(t, "login")
	if err != nil {
		t.Fatal(err)
	}
	if discoveredServer != "https://trusted.example.test" {
		t.Fatalf("discovery used unexpected server %q", discoveredServer)
	}
	if len(store.logins) != 1 || store.logins[0].ClientID != "browser-client.apps.googleusercontent.com" ||
		store.logins[0].ClientSecret != "discovered-browser-credential" {
		t.Fatal("browser acquisition did not use the discovered browser ID and matching credential")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config := string(raw)
	for _, want := range []string{
		"audience = 'browser-client.apps.googleusercontent.com'",
		"oauth_client_id = 'browser-client.apps.googleusercontent.com'",
		"oauth_client_secret = 'discovered-browser-credential'",
		"oauth_device_client_id = 'device-client.apps.googleusercontent.com'",
		"oauth_device_client_secret = 'discovered-device-credential'",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("browser promotion omitted %q: %s", want, config)
		}
	}
	if !strings.Contains(out, `"oauth_client_id":"browser-client.apps.googleusercontent.com"`) {
		t.Fatalf("login output has the wrong promoted client: %s", out)
	}
}

func TestDeviceLoginDiscoversClientAndPromotesDeviceCredential(t *testing.T) {
	path := writeCloudLoginConfig(t)
	body := "mode='cloud'\nserver='https://trusted.example.test'\nprincipal='alice@example.test'\ncache='cache.db'\ncredential_source='oauth'\noauth_client_id='browser-client.apps.googleusercontent.com'\noauth_client_secret='desktop-client-secret'\noauth_keyring_service='review-keyring'\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	store := &loginMemoryStore{}
	installLoginTestHooks(t, store, auth.Credential{
		ClientID:  "device-client.apps.googleusercontent.com",
		Principal: "alice@example.test", Subject: "new", RefreshToken: "device-refresh",
	})
	discoverLoginOAuth = func(context.Context, string, *http.Client) (remote.OAuthDiscovery, error) {
		return remote.OAuthDiscovery{
			BrowserClientID:     "browser-client.apps.googleusercontent.com",
			BrowserClientSecret: "discovered-browser-credential",
			DeviceClientID:      "device-client.apps.googleusercontent.com",
			DeviceClientSecret:  "discovered-device-credential",
		}, nil
	}
	out, _, err := runLogin(t, "login", "--device")
	if err != nil {
		t.Fatal(err)
	}
	if len(store.logins) != 1 || store.logins[0].ClientID != "device-client.apps.googleusercontent.com" ||
		store.logins[0].ClientSecret != "discovered-device-credential" {
		t.Fatal("device acquisition did not use the discovered device ID and matching credential")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config := string(raw)
	for _, want := range []string{
		"audience = 'device-client.apps.googleusercontent.com'",
		"oauth_client_id = 'device-client.apps.googleusercontent.com'",
		"oauth_client_secret = 'discovered-device-credential'",
		"oauth_device_client_id = 'device-client.apps.googleusercontent.com'",
		"oauth_device_client_secret = 'discovered-device-credential'",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("device promotion omitted %q: %s", want, config)
		}
	}
	if strings.Contains(config, "desktop-client-secret") {
		t.Fatalf("device promotion retained the Desktop secret: %s", config)
	}
	if !strings.Contains(out, `"oauth_client_id":"device-client.apps.googleusercontent.com"`) {
		t.Fatalf("login output has the wrong promoted client: %s", out)
	}
	for i := 0; i < 2; i++ {
		logout, _, logoutErr := runLogin(t, "logout")
		if logoutErr != nil {
			t.Fatalf("logout %d after device promotion: %v", i+1, logoutErr)
		}
		if !strings.Contains(logout, `"client_id":"device-client.apps.googleusercontent.com"`) {
			t.Fatalf("logout did not identify the promoted device key: %s", logout)
		}
	}
	if _, ok := store.values[auth.Key("device-client.apps.googleusercontent.com")]; ok {
		t.Fatal("logout left the promoted device credential")
	}
}

func TestExplicitOAuthClientIDSkipsDiscovery(t *testing.T) {
	for _, device := range []bool{false, true} {
		t.Run(map[bool]string{false: "browser", true: "device"}[device], func(t *testing.T) {
			_ = writeCloudLoginConfig(t)
			store := &loginMemoryStore{}
			clientID := "desktop-client.apps.googleusercontent.com"
			if device {
				clientID = "device-client.apps.googleusercontent.com"
			}
			installLoginTestHooks(t, store, auth.Credential{
				ClientID: clientID, Principal: "alice@example.test", Subject: "new", RefreshToken: "refresh",
			})
			discoverLoginOAuth = func(context.Context, string, *http.Client) (remote.OAuthDiscovery, error) {
				t.Fatal("discovery ran for an explicitly configured client ID")
				return remote.OAuthDiscovery{}, nil
			}
			args := []string{"login"}
			if device {
				args = append(args, "--device")
			}
			if _, _, err := runLogin(t, args...); err != nil {
				t.Fatal(err)
			}
			if len(store.logins) != 1 {
				t.Fatalf("missing OAuth acquisition config: %#v", store.logins)
			}
			got := store.logins[0]
			wantSecret := "must-not-be-logged"
			if device {
				wantSecret = "device-client-credential"
			}
			if got.ClientID != clientID || got.ClientSecret != wantSecret || got.ExpectedPrincipal != "alice@example.test" {
				t.Fatalf("wrong explicit OAuth client mapping: %#v", got)
			}
		})
	}
}

func TestLoginDiscoveryFailuresStopBeforeAcquisition(t *testing.T) {
	for _, tc := range []struct {
		name       string
		device     bool
		discovery  remote.OAuthDiscovery
		err        error
		wantPhrase string
	}{
		{name: "browser-empty", discovery: remote.OAuthDiscovery{DeviceClientID: "device"}, wantPhrase: "empty browser_client_id"},
		{name: "browser-secret-empty", discovery: remote.OAuthDiscovery{BrowserClientID: "browser"}, wantPhrase: "empty browser_client_secret"},
		{name: "device-empty", device: true, discovery: remote.OAuthDiscovery{BrowserClientID: "browser"}, wantPhrase: "empty device_client_id"},
		{name: "device-secret-empty", device: true, discovery: remote.OAuthDiscovery{DeviceClientID: "device"}, wantPhrase: "empty device_client_secret"},
		{name: "malformed", err: errors.New("synthetic malformed discovery"), wantPhrase: "cannot discover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCloudLoginConfig(t)
			body := "mode='cloud'\nserver='https://trusted.example.test'\nprincipal='alice@example.test'\ncache='cache.db'\ncredential_source='oauth'\n"
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			store := &loginMemoryStore{}
			installLoginTestHooks(t, store, auth.Credential{})
			failLoginAcquisition(t)
			discoverLoginOAuth = func(context.Context, string, *http.Client) (remote.OAuthDiscovery, error) {
				return tc.discovery, tc.err
			}
			args := []string{"login"}
			if tc.device {
				args = append(args, "--device")
			}
			out, _, err := runLogin(t, args...)
			if err == nil || out != "" || !strings.Contains(err.Error(), tc.wantPhrase) {
				t.Fatalf("discovery failure: out=%q err=%v", out, err)
			}
			if store.saves != 0 {
				t.Fatal("discovery failure changed keychain state")
			}
		})
	}
}

func TestRunNormalCloudCommandRejectsIncompleteLoginRoute(t *testing.T) {
	path := writeCloudLoginConfig(t)
	body := "mode='cloud'\nserver='https://trusted.example.test'\nprincipal='alice@example.test'\ncache='cache.db'\ncredential_source='oauth'\noauth_device_client_id='device-client.apps.googleusercontent.com'\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	discoverLoginOAuth = func(context.Context, string, *http.Client) (remote.OAuthDiscovery, error) {
		t.Fatal("normal cloud command attempted login discovery")
		return remote.OAuthDiscovery{}, nil
	}
	out, _, err := runLogin(t, "whoami")
	if err == nil || out != "" || !strings.Contains(err.Error(), "cloud mode requires trusted server and audience") {
		t.Fatalf("incomplete normal route did not fail closed: out=%q err=%v", out, err)
	}
}

func TestLoginDiscoveryConfigWriteFailureRollsBackPromotedCredential(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prior *auth.Credential
	}{
		{name: "new-device-entry"},
		{name: "replaced-device-entry", prior: &auth.Credential{
			Version: 1, ClientID: "device-client.apps.googleusercontent.com",
			Principal: "alice@example.test", Subject: "prior", RefreshToken: "prior-refresh",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCloudLoginConfig(t)
			body := "mode='cloud'\nserver='https://trusted.example.test'\nprincipal='alice@example.test'\ncache='cache.db'\ncredential_source='oauth'\noauth_client_id='desktop-client.apps.googleusercontent.com'\noauth_client_secret='browser-secret'\n"
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			store := &loginMemoryStore{}
			if tc.prior != nil {
				store.values = map[string]auth.Credential{auth.Key(tc.prior.ClientID): *tc.prior}
			}
			installLoginTestHooks(t, store, auth.Credential{
				ClientID:  "device-client.apps.googleusercontent.com",
				Principal: "alice@example.test", Subject: "new", RefreshToken: "new-refresh",
			})
			discoverLoginOAuth = func(context.Context, string, *http.Client) (remote.OAuthDiscovery, error) {
				return remote.OAuthDiscovery{
					DeviceClientID:     "device-client.apps.googleusercontent.com",
					DeviceClientSecret: "discovered-device-credential",
				}, nil
			}
			writeLoginConfig = func(string, Config) error { return errors.New("synthetic trusted write failure") }
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			out, _, err := runLogin(t, "login", "--device")
			if err == nil || out != "" {
				t.Fatalf("failed login reported success: out=%q err=%v", out, err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("trusted configuration changed after failed login")
			}
			key := auth.Key("device-client.apps.googleusercontent.com")
			if tc.prior == nil {
				if _, ok := store.values[key]; ok {
					t.Fatal("new promoted keychain entry survived failed config write")
				}
			} else if got := store.values[key]; got.RefreshToken != tc.prior.RefreshToken || got.Subject != tc.prior.Subject {
				t.Fatalf("prior keychain credential was not restored: %#v", got)
			}
		})
	}
}

func TestLoginHelpReturnsNormally(t *testing.T) {
	var out, diagnostics bytes.Buffer
	if err := Run(context.Background(), []string{"login", "--help"}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostics.String(), "Usage of login") {
		t.Fatalf("login help was not written: %q", diagnostics.String())
	}
}

func TestUseGcloudFallsBackToBrowserAndClearsLegacyRoute(t *testing.T) {
	path := writeCloudLoginConfig(t)
	store := &loginMemoryStore{}
	installLoginTestHooks(t, store, auth.Credential{
		ClientID:  "desktop-client.apps.googleusercontent.com",
		Principal: "alice@example.test", Subject: "new", RefreshToken: "browser-refresh",
	})
	dir := filepath.Dir(path)
	shimDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(shimDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "gcloud"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, _, err := runLogin(t, "login", "--use-gcloud")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"credential_source":"oauth"`) {
		t.Fatalf("gcloud failure did not fall back to browser OAuth: %s", out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"account =", "impersonate_service_account =", "credentials_file ="} {
		if strings.Contains(string(raw), stale) {
			t.Fatalf("OAuth fallback retained a cross-backend field %q: %s", stale, raw)
		}
	}
}

func TestUseGcloudProbeCanUseSeparateExplicitGcloudProfile(t *testing.T) {
	path := writeCloudLoginConfig(t)
	dir := filepath.Dir(path)
	shimDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(shimDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "gcloud"), []byte("#!/bin/sh\nprintf '%s' 'synthetic-profile-token'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	store := &loginMemoryStore{}
	installLoginTestHooks(t, store, auth.Credential{
		ClientID:  "desktop-client.apps.googleusercontent.com",
		Principal: "alice@example.test", Subject: "unused", RefreshToken: "unused",
	})
	out, _, err := runLogin(t,
		"--account", "human@example.test",
		"--impersonate-service-account", "agent@example.test",
		"--credential-source", "gcloud",
		"login", "--use-gcloud",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"credential_source":"gcloud"`) || !strings.Contains(out, "agent@example.test") {
		t.Fatalf("gcloud profile probe did not succeed: %s", out)
	}
	if store.saves != 0 {
		t.Fatal("successful gcloud probe saved an OAuth credential")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "human@example.test") || strings.Contains(string(raw), "agent@example.test") {
		t.Fatalf("temporary gcloud profile was persisted: %s", raw)
	}
	config, _, err := resolveConfig(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if config.CredentialSource != "oauth" || config.OAuthClientID != "desktop-client.apps.googleusercontent.com" {
		t.Fatalf("trusted OAuth route was lost: %#v", config)
	}
}

func TestUseGcloudFallbackCannotInheritInvalidMixedProfile(t *testing.T) {
	path := writeCloudLoginConfig(t)
	store := &loginMemoryStore{}
	installLoginTestHooks(t, store, auth.Credential{
		ClientID:  "desktop-client.apps.googleusercontent.com",
		Principal: "alice@example.test", Subject: "unused", RefreshToken: "browser-refresh",
	})
	dir := filepath.Dir(path)
	shimDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(shimDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "gcloud"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, _, err := runLogin(t,
		"--account", "human@example.test",
		"--impersonate-service-account", "agent@example.test",
		"--credential-source", "gcloud",
		"login", "--use-gcloud",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"credential_source":"oauth"`) {
		t.Fatalf("failed gcloud profile did not fall back: %s", out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"human@example.test", "agent@example.test", "credential_source = 'gcloud'"} {
		if strings.Contains(string(raw), value) {
			t.Fatalf("fallback retained gcloud probe value %q: %s", value, raw)
		}
	}
	resolved, _, err := resolveConfig(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Account != "" || resolved.ImpersonateServiceAccount != "" || resolved.CredentialSource != "oauth" {
		t.Fatalf("trusted route retained mixed backend settings: %#v", resolved)
	}
}

func TestUseGcloudSucceedsWithoutOAuthConfiguration(t *testing.T) {
	dir := isolated(t)
	home := filepath.Join(dir, "state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(shimDir, 0700); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nprintf '%s' 'synthetic-gcloud-token'\n"
	if err := os.WriteFile(filepath.Join(shimDir, "gcloud"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	body := "mode='cloud'\nserver='https://trusted.example.test'\naudience='audience'\naccount='human@example.test'\nimpersonate_service_account='agent@example.test'\nprincipal='agent@example.test'\ncache='cache.db'\ncredential_source='gcloud'\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runLogin(t, "login", "--use-gcloud")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"credential_source":"gcloud"`) {
		t.Fatalf("gcloud login did not succeed: %s", out)
	}
}

func TestBrowserAfterDevicePromotionDiscoversDesktopOrFailsWithoutMutation(t *testing.T) {
	for _, outcome := range []string{"success", "unavailable", "same-device", "acquisition-failure"} {
		t.Run(outcome, func(t *testing.T) {
			path := writeCloudLoginConfig(t)
			original := "mode='cloud'\nserver='https://trusted.example.test'\nprincipal='alice@example.test'\ncache='cache.db'\ncredential_source='oauth'\naudience='device-client.apps.googleusercontent.com'\noauth_client_id='device-client.apps.googleusercontent.com'\noauth_client_secret='device-credential'\noauth_device_client_id='device-client.apps.googleusercontent.com'\noauth_device_client_secret='device-credential'\n"
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			prior := auth.Credential{ClientID: "device-client.apps.googleusercontent.com", Principal: "alice@example.test", RefreshToken: "prior-device-refresh"}
			store := &loginMemoryStore{values: map[string]auth.Credential{prior.ClientID: prior}}
			installLoginTestHooks(t, store, auth.Credential{ClientID: "browser-client.apps.googleusercontent.com", Principal: prior.Principal, RefreshToken: "browser-refresh"})
			calls := 0
			discoverLoginOAuth = func(_ context.Context, server string, _ *http.Client) (remote.OAuthDiscovery, error) {
				calls++
				if server != "https://trusted.example.test" {
					t.Fatal("untrusted discovery route")
				}
				if outcome == "unavailable" {
					return remote.OAuthDiscovery{}, errors.New("discovery unavailable")
				}
				id := "browser-client.apps.googleusercontent.com"
				if outcome == "same-device" {
					id = prior.ClientID
				}
				return remote.OAuthDiscovery{BrowserClientID: id, BrowserClientSecret: "browser-credential", DeviceClientID: prior.ClientID, DeviceClientSecret: "device-credential"}, nil
			}
			if outcome == "acquisition-failure" {
				browserLoginStaged = func(context.Context, auth.Config) (auth.Credential, *auth.CredentialTransaction, error) {
					return auth.Credential{}, nil, errors.New("acquisition rejected")
				}
			}
			_, _, err := runLogin(t, "login")
			if calls != 1 {
				t.Fatal("known promoted device did not trigger discovery")
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if outcome != "success" {
				if err == nil {
					t.Fatal("failed transition accepted")
				}
				if string(raw) != original || store.saves != 0 || len(store.values) != 1 {
					t.Fatal("failed transition mutated route or keychain")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(store.logins) != 1 || store.logins[0].ClientID != "browser-client.apps.googleusercontent.com" || store.logins[0].ClientSecret != "browser-credential" {
					t.Fatal("browser acquisition used device pair")
				}
				if !strings.Contains(string(raw), "audience = 'browser-client.apps.googleusercontent.com'") {
					t.Fatal("browser audience not promoted")
				}
			}
			if store.values[prior.ClientID] != prior {
				t.Fatal("prior device credential changed")
			}
		})
	}
}
