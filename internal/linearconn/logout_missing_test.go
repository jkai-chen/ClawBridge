package linearconn

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// missingStore deletes like the native keyring: a missing entry is an error.
type missingStore struct {
	*memoryStore
	fail error
}

func (s missingStore) Delete(service, key string) error {
	if s.fail != nil {
		return s.fail
	}
	return s.memoryStore.Delete(service, key)
}

func TestLinearLogoutRemovesAProfileWhoseCredentialIsAlreadyGone(t *testing.T) {
	m := testManager(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return response(`{}`), nil }))
	p, e := m.Import(context.Background(), "work", Read, strings.NewReader("token-one"))
	if e != nil {
		t.Fatal(e)
	}
	store := missingStore{memoryStore: m.config.Store.(*memoryStore), fail: errors.New("secret backend content")}
	m.config.Store = store
	if e = m.Logout(context.Background(), p.ID); e == nil {
		t.Fatal("a failing credential deletion was ignored")
	}
	store.fail = keyring.ErrNotFound
	m.config.Store = store
	if e = m.Logout(context.Background(), p.ID); e != nil {
		t.Fatal("logout stuck on an already-missing credential", e)
	}
	if _, e = m.Status(context.Background(), p.ID); e == nil {
		t.Fatal("profile kept after logout")
	}
}
