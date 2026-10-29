package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// lockoutStore answers LauncherWanted for good only, and counts lookups;
// any other method panics, so a test proves what reached the store.
type lockoutStore struct {
	ModelStore
	good    string
	lookups int
}

func (s *lockoutStore) LauncherWanted(_ context.Context, token string) ([]bus.LauncherAgent, error) {
	s.lookups++
	if token == s.good {
		return []bus.LauncherAgent{}, nil
	}
	return nil, bus.ErrModelUnauthorized
}

// countingBody counts reads of a request body.
type countingBody struct{ reads int }

func (b *countingBody) Read(p []byte) (int, error) {
	b.reads++
	return copy(p, "{}"), nil
}

func (b *countingBody) Close() error { return nil }

// A locked-out address costs no body read and no lookup (G9-2); a token
// that authenticated lately still gets through, a token new to the server
// waits out the lockout, and the lockout ends on time.
func TestModelLockoutRefusesBeforeTheWork(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	good := "cbl_" + strings.Repeat("g", 43)
	store := &lockoutStore{good: good}
	h := newModelHandler(store, nil, 1, func() time.Time { return now }, nil, nil)
	send := func(method, path, token, address string, body *countingBody) int {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		if body != nil {
			r.Body = body
		}
		r.RemoteAddr, r.TLS = address+":443", &tls.ConnectionState{}
		r.Header.Set("ClawBridge-Model-API", modelAPIVersion)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	wanted := func(token, address string) int { return send(http.MethodGet, LauncherWantedPath, token, address, nil) }
	if code := wanted(good, "192.0.2.1"); code != 200 {
		t.Fatal(code)
	}
	// Each guess has its own prefix: only the address locks out.
	for i := range modelAuthFailures {
		if code := wanted(fmt.Sprintf("cbl_%08d", i)+strings.Repeat("x", 35), "192.0.2.1"); code != 401 {
			t.Fatal(i, code)
		}
	}
	lookups := store.lookups
	if code := wanted("cbl_guess000"+strings.Repeat("x", 35), "192.0.2.1"); code != 429 || store.lookups != lookups {
		t.Fatal("a locked-out guess was looked up:", code, store.lookups-lookups)
	}
	body := &countingBody{}
	if code := send(http.MethodPost, AgentRenewPath, "cba_"+strings.Repeat("y", 43), "192.0.2.1", body); code != 429 || body.reads != 0 {
		t.Fatal("a locked-out request's body was read:", code, body.reads)
	}
	if code := wanted(good, "192.0.2.1"); code != 200 || store.lookups != lookups+1 {
		t.Fatal("a known token was locked out:", code)
	}
	// Another address isn't locked out.
	if code := wanted("cbl_other000"+strings.Repeat("x", 35), "198.51.100.1"); code != 401 {
		t.Fatal(code)
	}
	// A valid token the server hasn't seen waits out the lockout.
	fresh := "cbl_" + strings.Repeat("f", 43)
	store.good = fresh
	lookups = store.lookups
	if code := wanted(fresh, "192.0.2.1"); code != 429 || store.lookups != lookups {
		t.Fatal(code)
	}
	now = now.Add(modelLockout)
	if code := wanted(fresh, "192.0.2.1"); code != 200 {
		t.Fatal("the lockout did not end:", code)
	}
}

// An IPv6 client's /64 is one address to the lockout, as to the other
// pre-auth limits (G9-1).
func TestModelLockoutKeysIPv6On64(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &lockoutStore{good: "cbl_" + strings.Repeat("g", 43)}
	h := newModelHandler(store, nil, 1, func() time.Time { return now }, nil, nil)
	wanted := func(i int, address string) int {
		r := httptest.NewRequest(http.MethodGet, LauncherWantedPath, nil)
		r.RemoteAddr, r.TLS = "["+address+"]:443", &tls.ConnectionState{}
		r.Header.Set("ClawBridge-Model-API", modelAPIVersion)
		r.Header.Set("Authorization", "Bearer "+fmt.Sprintf("cbl_%08d", i)+strings.Repeat("x", 35))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := range modelAuthFailures {
		wanted(i, fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	if code := wanted(99, "2001:db8:1:2:ffff::1"); code != 429 {
		t.Fatal("rotating within a /64 dodged the lockout:", code)
	}
}

// renewStore answers LauncherWanted like lockoutStore and renews only the
// valid credential, counting each lookup.
type renewStore struct {
	lockoutStore
	valid    string
	renewals int
}

func (s *renewStore) RenewAgentTokens(_ context.Context, credential, _ string) ([]bus.ModelProjectToken, error) {
	s.renewals++
	if credential == s.valid {
		return []bus.ModelProjectToken{}, nil
	}
	return nil, bus.ErrModelUnauthorized
}

// A call refused before the lookup (a malformed body) never makes a token
// known, and a token the lookup refuses is forgotten: during a lockout both
// get 429, with no body read and no lookup (G9-2).
func TestModelLockoutKnowsOnlyAnsweredTokens(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &renewStore{lockoutStore: lockoutStore{good: "cbl_" + strings.Repeat("g", 43)}}
	h := newModelHandler(store, nil, 1, func() time.Time { return now }, nil, nil)
	send := func(method, path, token, body string) (int, *onceBody) {
		t.Helper()
		b := &onceBody{data: body}
		r := httptest.NewRequest(method, path, nil)
		r.Body = b
		r.RemoteAddr, r.TLS = "192.0.2.1:443", &tls.ConnectionState{}
		r.Header.Set("ClawBridge-Model-API", modelAPIVersion)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, b
	}
	guess := "cba_" + strings.Repeat("r", 43)
	revoked := "cba_" + strings.Repeat("v", 43)
	secret := `{"binding_secret":"` + strings.Repeat("C", 43) + `"}`
	store.valid = revoked
	if code, _ := send(http.MethodPost, AgentRenewPath, revoked, secret); code != 200 {
		t.Fatal(code)
	}
	store.valid = ""
	for range 3 {
		if code, _ := send(http.MethodPost, AgentRenewPath, guess, `{"bad":1}`); code != 400 {
			t.Fatal(code)
		}
	}
	if code, _ := send(http.MethodPost, AgentRenewPath, revoked, secret); code != 401 {
		t.Fatal(code)
	}
	for i := range modelAuthFailures {
		send(http.MethodGet, LauncherWantedPath, fmt.Sprintf("cbl_%08d", i)+strings.Repeat("x", 35), "")
	}
	renewals := store.renewals
	for _, token := range []string{guess, revoked} {
		code, b := send(http.MethodPost, AgentRenewPath, token, `{"bad":1}`)
		if code != 429 || b.reads != 0 || store.renewals != renewals {
			t.Fatalf("%s passed a lockout: %d, %d reads, %d lookups", token[:8], code, b.reads, store.renewals-renewals)
		}
	}
}

// onceBody serves data once, counting reads.
type onceBody struct {
	data  string
	reads int
}

func (b *onceBody) Read(p []byte) (int, error) {
	b.reads++
	n := copy(p, b.data)
	b.data = b.data[n:]
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (b *onceBody) Close() error { return nil }

// One token prefix guessed from many addresses locks out too, before the
// lookup.
func TestModelLockoutByPrefix(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &lockoutStore{good: "cbl_" + strings.Repeat("g", 43)}
	h := newModelHandler(store, nil, 1, func() time.Time { return now }, nil, nil)
	wanted := func(i int) int {
		r := httptest.NewRequest(http.MethodGet, LauncherWantedPath, nil)
		r.RemoteAddr, r.TLS = fmt.Sprintf("192.0.2.%d:443", i+1), &tls.ConnectionState{}
		r.Header.Set("ClawBridge-Model-API", modelAPIVersion)
		r.Header.Set("Authorization", "Bearer cbl_samepref"+fmt.Sprintf("%035d", i))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := range modelAuthFailures {
		wanted(i)
	}
	lookups := store.lookups
	if code := wanted(99); code != 429 || store.lookups != lookups {
		t.Fatal("a locked-out prefix was looked up:", code)
	}
}

// recentTokens forgets a token after modelLockout and, full of fresh
// tokens, remembers no new one.
func TestRecentTokens(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := &recentTokens{now: func() time.Time { return now }, seen: map[string]time.Time{}}
	r.add("a")
	if !r.has("a") || r.has("b") {
		t.Fatal("add")
	}
	now = now.Add(modelLockout)
	if r.has("a") {
		t.Fatal("kept past modelLockout")
	}
	for i := range modelLockoutKeys {
		r.seen[fmt.Sprint("k", i)] = now
	}
	r.add("new")
	if r.has("new") || len(r.seen) != modelLockoutKeys {
		t.Fatal("grew past its bound")
	}
	now = now.Add(modelLockout)
	r.add("new")
	if !r.has("new") || len(r.seen) != 1 {
		t.Fatal("stale entries were not pruned:", len(r.seen))
	}
}

// mintStore mints one agent credential for its launcher, and that
// credential redeems to one project token; any other credential is refused.
type mintStore struct {
	lockoutStore
	minted, project string
	redeems, hellos int
}

func (s *mintStore) LauncherCredential(_ context.Context, token, _ string) (string, time.Time, error) {
	if token != s.good {
		return "", time.Time{}, bus.ErrModelUnauthorized
	}
	return s.minted, time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), nil
}

func (s *mintStore) RedeemAgentCredential(_ context.Context, credential, _, _ string) (bus.AgentConfig, error) {
	s.redeems++
	if credential != s.minted {
		return bus.AgentConfig{}, bus.ErrModelUnauthorized
	}
	return bus.AgentConfig{Projects: []bus.ModelProjectToken{{ProjectID: "p1", Token: s.project}}}, nil
}

func (s *mintStore) ModelHello(_ context.Context, token string, _ int) (bus.ModelHello, error) {
	s.hellos++
	if token != s.project {
		return bus.ModelHello{}, bus.ErrModelUnauthorized
	}
	return bus.ModelHello{}, nil
}

// Agents on one launcher share its address. A dead agent that keeps failing
// locks the address out, but the launcher's newly minted credential, and
// the project token it redeems to, are known from the answers that handed
// them out, so the next agent starts (#158 review).
func TestModelLockoutLetsAFreshlyMintedCredentialIn(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &mintStore{lockoutStore: lockoutStore{good: "cbl_" + strings.Repeat("g", 43)}, minted: "cba_" + strings.Repeat("m", 43), project: "cbm_" + strings.Repeat("p", 43)}
	h := newModelHandler(store, nil, 1, func() time.Time { return now }, nil, nil)
	send := func(method, path, token, body string) int {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr, r.TLS = "192.0.2.1:443", &tls.ConnectionState{}
		r.Header.Set("ClawBridge-Model-API", modelAPIVersion)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	// The launcher has been polling all along.
	if code := send(http.MethodGet, LauncherWantedPath, store.good, ""); code != 200 {
		t.Fatal(code)
	}
	// The dead agent's revoked credential keeps failing until the address
	// is locked out.
	dead := "cba_" + strings.Repeat("d", 43)
	config := `{"instance_id":"agent-1"}`
	for range modelAuthFailures {
		if code := send(http.MethodPost, AgentConfigPath, dead, config); code != 401 {
			t.Fatal(code)
		}
	}
	if code := send(http.MethodPost, AgentConfigPath, dead, config); code != 429 {
		t.Fatal("the address wasn't locked out:", code)
	}
	// The launcher, known from its polls, mints a credential for a new agent.
	if code := send(http.MethodPost, LauncherCredentialPath, store.good, `{"owner_ref":"agent-2"}`); code != 200 {
		t.Fatal("the known launcher was locked out:", code)
	}
	if code := send(http.MethodPost, AgentConfigPath, store.minted, `{"instance_id":"agent-2"}`); code != 200 {
		t.Fatal("the freshly minted credential was locked out:", code)
	}
	if code := send(http.MethodGet, ModelHelloPath, store.project, ""); code != 200 {
		t.Fatal("the project token agent/config handed out was locked out:", code)
	}
	// A credential nobody handed out still waits out the lockout, unread.
	redeems := store.redeems
	if code := send(http.MethodPost, AgentConfigPath, "cba_"+strings.Repeat("n", 43), config); code != 429 || store.redeems != redeems {
		t.Fatal("an unknown credential passed the lockout:", code)
	}
}
