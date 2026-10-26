package linearapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

const (
	appUser = "a0000000-0000-4000-8000-0000000000a1"
	org     = "b0000000-0000-4000-8000-0000000000b1"
	secret  = "s3cret-value-never-logged"
)

var key = []byte("0123456789abcdef0123456789abcdef")

// memStore is the bus's sealed-token store in memory: one live row per
// app and organisation, published by compare-and-swap.
type memStore struct {
	mu        sync.Mutex
	rows      []bus.AppToken
	live      map[string]int
	minted    int
	recordErr error
}

func (m *memStore) RecordMintedAppToken(context.Context, string, string, time.Time, time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recordErr != nil {
		return m.recordErr
	}
	m.minted++
	return nil
}

func (m *memStore) LiveAppToken(_ context.Context, app, o string) (bus.AppToken, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, i := range m.live {
		if i >= 0 && (o == "" || k == app+"/"+o) {
			return m.rows[i], true, nil
		}
	}
	return bus.AppToken{}, false, nil
}
func (m *memStore) PublishAppToken(_ context.Context, t bus.AppToken, replaces string) (bus.AppToken, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := t.AppClientID + "/" + t.LinearOrgID
	if i, ok := m.live[k]; ok && i >= 0 && m.rows[i].TokenSHA256 != replaces {
		return m.rows[i], false, nil
	}
	m.rows = append(m.rows, t)
	m.live[k] = len(m.rows) - 1
	return t, true, nil
}
func (m *memStore) AppTokensMinted(context.Context, string, time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.minted, nil
}

// linear fakes api.linear.app: the token endpoint and GraphQL viewer.
type linear struct {
	mu         sync.Mutex
	viewerFail int
	mints      int
	viewers    int
	tokenFail  []int
	scope      string
	app        bool
	forms      []string
	auths      []string
}

func (l *linear) RoundTrip(r *http.Request) (*http.Response, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	reply := func(status int, v any) (*http.Response, error) {
		raw, _ := json.Marshal(v)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}}, nil
	}
	switch r.URL.String() {
	case tokenURL:
		l.mints++
		l.forms = append(l.forms, string(body))
		user, pass, _ := r.BasicAuth()
		l.auths = append(l.auths, user+":"+pass)
		if len(l.tokenFail) > 0 {
			status := l.tokenFail[0]
			l.tokenFail = l.tokenFail[1:]
			return reply(status, map[string]any{"error": "x"})
		}
		scope := l.scope
		if scope == "" {
			scope = "app:assignable app:mentionable comments:create issues:create read write"
		}
		return reply(200, map[string]any{"access_token": fmt.Sprintf("lin_oauth_token_%d", l.mints), "token_type": "Bearer", "expires_in": 2591999, "scope": scope})
	case graphqlURL:
		l.viewers++
		if l.viewerFail != 0 {
			return reply(l.viewerFail, map[string]any{})
		}
		return reply(200, map[string]any{"data": map[string]any{"viewer": map[string]any{"id": appUser, "app": l.app, "organization": map[string]any{"id": org}}}})
	}
	return nil, errors.New("unexpected " + r.URL.String())
}

func newApp(t *testing.T, s Store, l *linear, now *time.Time) *App {
	t.Helper()
	a, err := New(Config{ClientID: "client-1", Secret: secret, Key: key, Store: s, HTTP: &http.Client{Transport: l}, Now: func() time.Time { return *now }})
	if err != nil || a == nil {
		t.Fatal(a, err)
	}
	return a
}

func TestAppMintsSealsAndSharesOneToken(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	store := &memStore{live: map[string]int{}}
	l := &linear{app: true}
	a := newApp(t, store, l, &now)
	token, id, err := a.Token(context.Background())
	if err != nil || token != "lin_oauth_token_1" || id.AppUserID != appUser || id.LinearOrgID != org {
		t.Fatal(token, id, err)
	}
	if l.forms[0] != "grant_type=client_credentials&scope=read%2Cwrite%2Ccomments%3Acreate%2Cissues%3Acreate%2Capp%3Aassignable%2Capp%3Amentionable" || l.auths[0] != "client-1:"+secret {
		t.Fatal("mint request", l.forms[0], l.auths[0])
	}
	row := store.rows[0]
	if strings.Contains(row.Ciphertext, "lin_oauth") || row.TokenSHA256 != digest(token) || row.LinearOrgID != org {
		t.Fatal("token stored unsealed or misfiled", row)
	}
	// The cache answers without Linear; a second instance adopts the row.
	if again, _, _ := a.Token(context.Background()); again != token || l.mints != 1 {
		t.Fatal("cached token not reused", l.mints)
	}
	b := newApp(t, store, l, &now)
	if other, id, err := b.Token(context.Background()); err != nil || other != token || id.LinearOrgID != org || l.mints != 1 {
		t.Fatal("a second instance minted instead of adopting", other, err, l.mints)
	}
	// A sealed row moved to another organisation does not open.
	moved := row
	moved.LinearOrgID = "b0000000-0000-4000-8000-0000000000b2"
	if _, err = a.open(moved); err == nil {
		t.Fatal("a moved ciphertext opened")
	}
	// Near expiry, the token is minted again in place of the old one.
	now = now.Add(26 * 24 * time.Hour)
	if fresh, _, err := a.Token(context.Background()); err != nil || fresh == token || l.mints != 2 {
		t.Fatal("token near expiry not replaced", fresh, err, l.mints)
	}
}

// An upgrade that changes the scopes: the stored token, minted under the
// old set, is never adopted. The first call mints under the new set in its
// place (Linear revokes the old token), and every instance adopts that.
func TestAScopeChangeReplacesTheStoredToken(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	store := &memStore{live: map[string]int{}}
	l := &linear{app: true}
	a := newApp(t, store, l, &now)
	old, _, err := a.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The row as v0.3.3 sealed it: a live token of the old scope set.
	store.rows[0].ScopesDigest = digest("read comments:create issues:create app:assignable app:mentionable")
	if err = a.seal(old, &store.rows[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = a.open(store.rows[0]); err != nil {
		t.Fatal("the old-scope row does not open", err)
	}
	b := newApp(t, store, l, &now)
	fresh, _, err := b.Token(context.Background())
	if err != nil || fresh == old || l.mints != 2 || len(store.rows) != 2 || store.rows[1].ScopesDigest != scopesDigest() {
		t.Fatal("a token of the old scopes was adopted", fresh == old, err, l.mints, len(store.rows))
	}
	if !HasScope("write") || HasScope("admin") {
		t.Fatal("HasScope")
	}
	if got, _, _ := newApp(t, store, l, &now).Token(context.Background()); got != fresh || l.mints != 2 {
		t.Fatal("the new token was not shared", l.mints)
	}
}

// A 401 re-mints only the token that failed; a token another instance
// already replaced is adopted, never handed back dead.
func TestRemintReplacesOnlyTheDeadToken(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	store := &memStore{live: map[string]int{}}
	l := &linear{app: true}
	a, b := newApp(t, store, l, &now), newApp(t, store, l, &now)
	first, _, _ := a.Token(context.Background())
	b.Token(context.Background())
	now = now.Add(2 * time.Minute)
	second, _, err := a.Remint(context.Background(), first)
	if err != nil || second == first || l.mints != 2 {
		t.Fatal(second, err)
	}
	// b still holds the dead token; its re-mint adopts a's, without minting.
	got, _, err := b.Remint(context.Background(), first)
	if err != nil || got != second || l.mints != 2 {
		t.Fatal("re-mint handed back a dead token or minted twice", got, err, l.mints)
	}
}

func TestMintRefusals(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		l    *linear
		code string
	}{
		"bad secret":   {&linear{app: true, tokenFail: []int{401}}, "app_unavailable"},
		"two 5xx":      {&linear{app: true, tokenFail: []int{500, 502}}, "unavailable"},
		"other scopes": {&linear{app: true, scope: "read write"}, "scopes_mismatch"},
		"not an app":   {&linear{app: false}, "not_an_app"},
	} {
		a := newApp(t, &memStore{live: map[string]int{}}, c.l, &now)
		var e *Error
		if _, _, err := a.Token(context.Background()); !errors.As(err, &e) || e.Code != c.code {
			t.Fatal(name, err)
		}
	}
	// One 5xx is retried.
	l := &linear{app: true, tokenFail: []int{503}}
	if _, _, err := newApp(t, &memStore{live: map[string]int{}}, l, &now).Token(context.Background()); err != nil || l.mints != 2 {
		t.Fatal("one 5xx not retried", err, l.mints)
	}
	// The mint limit and the pace.
	store := &memStore{live: map[string]int{}, minted: mintLimit}
	var e *Error
	if _, _, err := newApp(t, store, &linear{app: true}, &now).Token(context.Background()); !errors.As(err, &e) || e.Code != "app_token_limit" {
		t.Fatal("limit", err)
	}
	a := newApp(t, &memStore{live: map[string]int{}}, &linear{app: true}, &now)
	first, _, _ := a.Token(context.Background())
	if _, _, err := a.Remint(context.Background(), first); !errors.As(err, &e) || e.Code != "mint_paced" {
		t.Fatal("re-mint within a minute", err)
	}
}

// Errors carry codes only: neither the secret nor a token appears.
func TestErrorsNeverCarryTheSecretOrToken(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for _, l := range []*linear{{app: true, tokenFail: []int{400}}, {app: false}, {app: true, scope: "read"}} {
		_, _, err := newApp(t, &memStore{live: map[string]int{}}, l, &now).Token(context.Background())
		if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "lin_oauth") {
			t.Fatal("leak", err)
		}
	}
	if a, err := New(Config{}); a != nil || err != nil {
		t.Fatal("an unconfigured app is off, not an error", a, err)
	}
	if _, err := New(Config{ClientID: "c", Secret: secret, Key: []byte("short"), Store: &memStore{}}); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("bad key", err)
	}
}

// A token Linear issued is counted at once, so one lost to a later step
// still counts toward the limit; two lost in a row stop minting for an
// hour; a token that cannot be counted is not used.
func TestMintedTokensAreCountedBeforeAnythingElse(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	store := &memStore{live: map[string]int{}}
	l := &linear{app: true, viewerFail: 503}
	a := newApp(t, store, l, &now)
	var e *Error
	if _, _, err := a.Token(context.Background()); err == nil || store.minted != 1 {
		t.Fatal("a token lost after the mint was not counted", err, store.minted)
	}
	now = now.Add(2 * time.Minute)
	a.Token(context.Background())
	now = now.Add(2 * time.Minute)
	if _, _, err := a.Token(context.Background()); !errors.As(err, &e) || e.Code != "mint_backoff" || l.mints != 2 {
		t.Fatal("no back-off after two lost mints", err, l.mints)
	}
	now = now.Add(time.Hour)
	l.viewerFail = 0
	if token, _, err := a.Token(context.Background()); err != nil || token == "" || store.minted != 3 {
		t.Fatal("minting did not resume after the back-off", err, store.minted)
	}
	// Uncountable: the token is not used.
	store = &memStore{live: map[string]int{}, recordErr: errors.New("database down")}
	l = &linear{app: true}
	if token, _, err := newApp(t, store, l, &now).Token(context.Background()); err == nil || token != "" || l.viewers != 0 {
		t.Fatal("an uncounted token was used", token, err)
	}
	// A refused token at viewer is reported as such.
	l = &linear{app: true, viewerFail: 401}
	if _, _, err := newApp(t, &memStore{live: map[string]int{}}, l, &now).Token(context.Background()); !errors.As(err, &e) || e.Code != "token_rejected" {
		t.Fatal(err)
	}
}
