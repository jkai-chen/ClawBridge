package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"clawbridge/internal/auth"
	"github.com/pelletier/go-toml/v2"
)

// v1.0 removed Google sign-in. A profile written before it may still carry
// its settings; they load with a one-line warning, never an error, and the
// refresh token a Google sign-in kept in the OS keychain is deleted once,
// best effort.

// legacyGoogleKeys are the profile keys Google sign-in used.
var legacyGoogleKeys = []string{
	"audience", "account", "impersonate_service_account", "credentials_file",
	"oauth_client_id", "oauth_client_secret", "oauth_device_client_id", "oauth_device_client_secret",
	"oauth_keyring_service",
}

// Seams for tests: where the notices go, and the keychain delete.
var (
	legacyNotices          io.Writer = os.Stderr
	deleteLegacyCredential           = auth.DeleteLegacyCredential
)

var legacyWarned sync.Map

// legacyCleanupMarker, in the state directory, records that the keychain
// cleanup ran, so it runs once.
const legacyCleanupMarker = "google-signin-removed"

// noteLegacyGoogle warns, once per file and process, about Google sign-in
// keys left in the trusted profile at path, naming the keys but never their
// values, and then deletes the keychain item they name. A repository's
// .clawbridge.toml never routes credentials, so it is never read here.
func noteLegacyGoogle(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	values := map[string]any{}
	if toml.Unmarshal(raw, &values) != nil {
		return
	}
	var found []string
	for _, k := range legacyGoogleKeys {
		if v, ok := values[k].(string); ok && strings.TrimSpace(v) != "" {
			found = append(found, k)
		}
	}
	if len(found) == 0 {
		return
	}
	if _, seen := legacyWarned.LoadOrStore(path, true); seen {
		return
	}
	fmt.Fprintf(legacyNotices, "warning: %s has Google sign-in settings (%s), unused since v1.0; remove them (docs/operations/google-sign-in-removal.md)\n", path, strings.Join(found, ", "))
	service, _ := values["oauth_keyring_service"].(string)
	var clients []string
	for _, k := range []string{"oauth_client_id", "oauth_device_client_id"} {
		if v, _ := values[k].(string); strings.TrimSpace(v) != "" {
			clients = append(clients, strings.TrimSpace(v))
		}
	}
	cleanLegacyKeychain(service, clients)
}

// cleanLegacyKeychain deletes, once per state directory, the refresh token a
// Google sign-in stored for each OAuth client. Each outcome is one line; a
// failure says what to remove by hand, and is not retried.
func cleanLegacyKeychain(service string, clients []string) {
	if len(clients) == 0 {
		return
	}
	home, err := stateHome()
	if err != nil {
		return
	}
	marker := filepath.Join(home, legacyCleanupMarker)
	if _, err = os.Stat(marker); err == nil {
		return
	}
	if strings.TrimSpace(service) == "" {
		service = auth.LegacyKeyringService
	}
	for _, client := range clients {
		removed, err := deleteLegacyCredential(context.Background(), service, client)
		switch {
		case err != nil:
			fmt.Fprintf(legacyNotices, "warning: could not delete the old Google sign-in credential from the OS keychain (service %s, account %s); remove it by hand\n", service, auth.LegacyKey(client))
		case removed:
			fmt.Fprintf(legacyNotices, "Deleted the old Google sign-in credential from the OS keychain (service %s).\n", service)
		}
	}
	if os.MkdirAll(home, 0700) == nil {
		_ = os.WriteFile(marker, []byte("the Google sign-in keychain cleanup ran\n"), 0600)
	}
}

// legacyRoute is the Google sign-in part of a profile's notify-state
// binding: the old keys' values as v0.3 read them (the trusted file, then
// the environment), so a profile migrated to a device key keeps its
// binding, and its notify state, across the upgrade. They sign nothing in.
type legacyRoute struct {
	audience, account, impersonate, oauthClientID, keyringService, credentialsFile string
}

func readLegacyRoute(path string) legacyRoute {
	var r legacyRoute
	values := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && toml.Unmarshal(raw, &values) == nil {
		str := func(k string) string { v, _ := values[k].(string); return v }
		r = legacyRoute{str("audience"), str("account"), str("impersonate_service_account"), str("oauth_client_id"), str("oauth_keyring_service"), str("credentials_file")}
	}
	for _, e := range []struct {
		dest *string
		env  string
	}{
		{&r.audience, "CLAWBRIDGE_AUDIENCE"}, {&r.account, "CLAWBRIDGE_ACCOUNT"},
		{&r.impersonate, "CLAWBRIDGE_IMPERSONATE_SERVICE_ACCOUNT"}, {&r.oauthClientID, "CLAWBRIDGE_OAUTH_CLIENT_ID"},
		{&r.keyringService, "CLAWBRIDGE_OAUTH_KEYRING_SERVICE"}, {&r.credentialsFile, "CLAWBRIDGE_CREDENTIALS_FILE"},
	} {
		if v := os.Getenv(e.env); v != "" {
			*e.dest = v
		}
	}
	// v0.3 resolved a relative credentials file against the state directory.
	if r.credentialsFile != "" && !filepath.IsAbs(r.credentialsFile) {
		if home, err := stateHome(); err == nil {
			r.credentialsFile = filepath.Join(home, r.credentialsFile)
		}
	}
	return r
}
