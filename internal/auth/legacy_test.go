package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// The keychain item a Google sign-in left is named by the OAuth client ID,
// deleted once, and a missing one is no error.
func TestDeleteLegacyCredential(t *testing.T) {
	original := keyringDelete
	t.Cleanup(func() { keyringDelete = original })
	items := map[string]bool{"clawbridge-oauth/" + LegacyKey("desktop.example.test"): true, "custom/" + LegacyKey("device.example.test"): true}
	keyringDelete = func(service, user string) error {
		if !items[service+"/"+user] {
			return keyring.ErrNotFound
		}
		delete(items, service+"/"+user)
		return nil
	}
	ctx := context.Background()
	if removed, err := DeleteLegacyCredential(ctx, "", "desktop.example.test"); !removed || err != nil {
		t.Fatal(removed, err)
	}
	if removed, err := DeleteLegacyCredential(ctx, "", "desktop.example.test"); removed || err != nil {
		t.Fatal("a second delete:", removed, err)
	}
	if removed, err := DeleteLegacyCredential(ctx, "custom", "device.example.test"); !removed || err != nil || len(items) != 0 {
		t.Fatal(removed, err, items)
	}
	if _, err := DeleteLegacyCredential(ctx, "", " "); err == nil {
		t.Fatal("no client ID was accepted")
	}
	keyringDelete = func(string, string) error { return errors.New("locked") }
	if removed, err := DeleteLegacyCredential(ctx, "", "desktop.example.test"); removed || err == nil {
		t.Fatal("a failed delete was reported as done")
	}
}
