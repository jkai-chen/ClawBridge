// Package auth checks the OS credential store and opens the system browser.
// It held Google sign-in's OAuth flows until v1.0 removed them; what is left
// of them here only deletes the refresh token a Google sign-in stored.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/zalando/go-keyring"
)

// LegacyKeyringService is the OS keychain service a Google sign-in stored
// its refresh token under, unless the profile named another.
const LegacyKeyringService = "clawbridge-oauth"

// LegacyKey is the keychain account a Google sign-in's refresh token was
// stored under: a hash of the OAuth client ID.
func LegacyKey(clientID string) string {
	sum := sha256.Sum256([]byte(clientID))
	return "oauth-" + base64.RawURLEncoding.EncodeToString(sum[:16])
}

// keyringDelete is the native delete; tests replace it.
var keyringDelete = keyring.Delete

// DeleteLegacyCredential removes the refresh token a Google sign-in stored
// for clientID in service (empty is LegacyKeyringService). It reports
// whether an item was there; a missing one is not an error.
func DeleteLegacyCredential(ctx context.Context, service, clientID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if strings.TrimSpace(clientID) == "" {
		return false, errors.New("no OAuth client ID names a keychain item")
	}
	if strings.TrimSpace(service) == "" {
		service = LegacyKeyringService
	}
	err := keyringDelete(service, LegacyKey(clientID))
	if errors.Is(err, keyring.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, nativeStoreError("remove")
	}
	return true, nil
}
