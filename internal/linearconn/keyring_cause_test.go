package linearconn

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// causeStore fails like a locked Secret Service collection, and echoes
// the value it was given, as a careless backend might.
type causeStore struct {
	*memoryStore
	said string
}

func (s causeStore) Set(service, key, value string) error {
	return errors.New(s.said + " [" + value + "]\n\x1b[31mend")
}
func (s causeStore) Get(service, key string) (string, error) {
	return "", errors.New(s.said)
}
func (s causeStore) Delete(service, key string) error { return errors.New(s.said) }

func TestLinearKeyringFailureSaysWhyInAFixedPhrase(t *testing.T) {
	rt, _ := oauthProvider(t)
	m := testManager(t, rt)
	m.config.Store = causeStore{memoryStore: m.config.Store.(*memoryStore), said: "failed to unlock correct collection '/org/freedesktop/secrets/collection/login'"}
	_, e := connectWith(m, "")
	if e == nil || !strings.Contains(e.Error(), "(the login collection is locked)") || !strings.Contains(e.Error(), "--key-store file") {
		t.Fatal(e)
	}
	noSecret(t, e.Error())
	if strings.Contains(e.Error(), "failed to unlock") || strings.ContainsAny(e.Error(), "\n\x1b[") {
		t.Fatal("the store's own words reached the message:", e)
	}
}

func TestLinearKeyringReadAndDeleteSayWhy(t *testing.T) {
	m := testManager(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return response(`{}`), nil }))
	p, e := m.Import(context.Background(), "work", Read, strings.NewReader("token-one"))
	if e != nil {
		t.Fatal(e)
	}
	m.config.Store = causeStore{memoryStore: m.config.Store.(*memoryStore), said: "org.freedesktop.Secret.Error.IsLocked: Cannot get secret of a locked object"}
	if _, e = m.Status(context.Background(), p.ID); e == nil || e.Error() != "Linear connection: secure credential unavailable: a collection is locked" {
		t.Fatal(e)
	}
	if e = m.Logout(context.Background(), p.ID); e == nil || e.Error() != "Linear connection: secure credential deletion failed: a collection is locked" {
		t.Fatal(e)
	}
}

func TestLinearKeyringCausePhrases(t *testing.T) {
	secret := "lin_oauth_syntheticaccesstoken0000000000000000"
	for _, c := range []struct {
		cause error
		want  string
	}{
		{nil, ""},
		{keyring.ErrSetDataTooBig, "the credential is too large for the key store"},
		{fmt.Errorf("wrapped: %w", keyring.ErrSetDataTooBig), "the credential is too large for the key store"},
		{keyring.ErrNotFound, "the key store has no credential for this profile"},
		{errNoSecretService, "no Secret Service is running on the session bus"},
		{errors.New("failed to unlock correct collection '/org/freedesktop/secrets/collection/Default_keyring'"), "the Default_keyring collection is locked"},
		{errors.New("failed to unlock correct collection '/org/freedesktop/secrets/aliases/default'"), "the default collection is locked"},
		// A name that isn't a plain name is never repeated.
		{errors.New("failed to unlock correct collection '/org/freedesktop/secrets/collection/" + secret + "/x'"), "a collection is locked"},
		{errors.New("failed to unlock correct collection '/org/freedesktop/secrets/collection/" + strings.Repeat("a", 65) + "'"), "a collection is locked"},
		{errors.New("org.freedesktop.Secret.Error.IsLocked: Cannot create an item in a locked collection"), "a collection is locked"},
		{errors.New("The name org.gnome.keyring.SystemPrompter was not provided by any .service files"), "the key store could not show its unlock prompt"},
		{errors.New("org.freedesktop.Secret.Error.NoSuchObject: No such item"), "the key store has no such collection or item"},
		{errors.New("org.freedesktop.DBus.Error.ServiceUnknown: The name org.freedesktop.secrets was not provided"), "no Secret Service is running on the session bus"},
		{errors.New("dbus: couldn't determine address of session bus"), "no Secret Service is running on the session bus"},
		// Anything else carries nothing the store said.
		{errors.New("backend: " + secret + " refresh-old initial-secret " + base64.StdEncoding.EncodeToString([]byte(secret))), "the key store gave an unrecognised error"},
		{errors.New("\x1b[31m‮weird\u009b"), "the key store gave an unrecognised error"},
	} {
		if got := keyringCause(c.cause); got != c.want {
			t.Errorf("%v: %q, want %q", c.cause, got, c.want)
		}
	}
}
