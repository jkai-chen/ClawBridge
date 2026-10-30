package linearconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

type memoryStore struct {
	mu     sync.Mutex
	values map[string]string
	fail   bool
}

func (s *memoryStore) Get(service, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[service+key]
	if !ok {
		return "", errors.New("secret backend content")
	}
	return v, nil
}
func (s *memoryStore) Set(service, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("secret backend content")
	}
	s.values[service+key] = value
	return nil
}
func (s *memoryStore) Delete(service, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, service+key)
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func testManager(t *testing.T, rt http.RoundTripper) *Manager {
	t.Helper()
	return New(Config{Directory: filepath.Join(t.TempDir(), "linear"), Store: &memoryStore{values: map[string]string{}}, HTTPClient: &http.Client{Transport: rt}, Verify: func(ctx context.Context, c *http.Client, a Access) (Identity, error) {
		r, e := http.NewRequestWithContext(ctx, "POST", Endpoint(a), strings.NewReader(`{}`))
		if e != nil {
			return Identity{}, e
		}
		resp, e := c.Do(r)
		if e != nil {
			return Identity{}, e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return Identity{}, errors.New("unsafe provider failure")
		}
		return Identity{AccountLabel: "operator"}, nil
	}})
}
func TestLinearImportIsolationLogoutAndNoFileSecrets(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	m := testManager(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get("Authorization"))
		mu.Unlock()
		return response(`{}`), nil
	}))
	p, e := m.Import(context.Background(), "work", Read, strings.NewReader("token-one\n"))
	if e != nil {
		t.Fatal(e)
	}
	p2, e := m.Import(context.Background(), "personal", Read, strings.NewReader("token-two"))
	if e != nil {
		t.Fatal(e)
	}
	client, e := m.HTTPClient(context.Background(), p.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = client.Get("https://evil.example/mcp"); e == nil {
		t.Fatal("foreign destination accepted")
	}
	if e = m.Logout(context.Background(), p.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = client.Get(Endpoint(Read)); e == nil {
		t.Fatal("logged out client remained authorized")
	}
	if _, e = m.Status(context.Background(), p2.ID); e != nil {
		t.Fatal("other profile erased", e)
	}
	profiles, e := m.Profiles()
	if e != nil || len(profiles) != 1 || profiles[0].ID != p2.ID {
		t.Fatalf("profiles=%v err=%v", profiles, e)
	}
	b, e := os.ReadFile(filepath.Join(m.config.Directory, "profiles.json"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "token-one") || strings.Contains(string(b), "token-two") || strings.Contains(string(b), "client_id") {
		t.Fatal("credential leaked into profile file")
	}
	if len(tokens) != 2 {
		t.Fatal("unexpected network call", tokens)
	}
}
func TestLinearVerificationAndSecureStoreFailureDoNotCommit(t *testing.T) {
	m := testManager(t, roundTripFunc(func(r *http.Request) (*http.Response, error) { return response(`{}`), nil }))
	m.config.Verify = func(context.Context, *http.Client, Access) (Identity, error) {
		return Identity{}, errors.New("token-secret")
	}
	if _, e := m.Import(context.Background(), "work", Read, strings.NewReader("secret")); e == nil || strings.Contains(e.Error(), "token-secret") {
		t.Fatal(e)
	}
	ps, e := m.Profiles()
	if e != nil || len(ps) != 0 {
		t.Fatal(ps, e)
	}
	m.config.Verify = func(context.Context, *http.Client, Access) (Identity, error) { return Identity{}, nil }
	m.config.Store.(*memoryStore).fail = true
	if _, e = m.Import(context.Background(), "work", Read, strings.NewReader("secret")); e == nil {
		t.Fatal("store failure ignored")
	}
	ps, e = m.Profiles()
	if e != nil || len(ps) != 0 {
		t.Fatal(ps, e)
	}
}
func TestLinearOAuthPKCEDCRAndRefreshRotation(t *testing.T) {
	var mu sync.Mutex
	refreshes := 0
	var challenge string
	var dispatched []string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource":
			return response(`{"resource":"https://mcp.linear.app/mcp","authorization_servers":["https://mcp.linear.app"]}`), nil
		case "/.well-known/oauth-authorization-server":
			return response(`{"issuer":"https://mcp.linear.app","authorization_endpoint":"https://mcp.linear.app/authorize","token_endpoint":"https://mcp.linear.app/token","registration_endpoint":"https://mcp.linear.app/register","code_challenge_methods_supported":["S256"],"token_endpoint_auth_methods_supported":["none"],"grant_types_supported":["authorization_code","refresh_token"]}`), nil
		case "/register":
			var in map[string]any
			if json.NewDecoder(r.Body).Decode(&in) != nil {
				t.Error("bad registration")
			}
			if in["token_endpoint_auth_method"] != "none" || in["scope"] != "read write" {
				t.Error(in)
			}
			b, _ := json.Marshal(map[string]any{"client_id": "client-id", "redirect_uris": in["redirect_uris"], "token_endpoint_auth_method": "none"})
			return response(string(b)), nil
		case "/token":
			if e := r.ParseForm(); e != nil {
				t.Error(e)
			}
			if r.Form.Get("grant_type") == "authorization_code" {
				if r.Form.Get("resource") != Origin+"/mcp" {
					t.Error("incorrect verified OAuth resource")
				}
				if oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != challenge {
					t.Error("PKCE mismatch")
				}
				return response(`{"access_token":"initial-secret","refresh_token":"refresh-old","token_type":"Bearer","expires_in":1}`), nil
			}
			mu.Lock()
			refreshes++
			mu.Unlock()
			if r.Form.Get("refresh_token") != "refresh-old" {
				t.Error("wrong refresh")
			}
			return response(`{"access_token":"rotated-secret","refresh_token":"refresh-new","token_type":"Bearer","expires_in":3600}`), nil
		case "/mcp":
			mu.Lock()
			dispatched = append(dispatched, r.Header.Get("Authorization"))
			mu.Unlock()
			return response(`{}`), nil
		}
		return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
	})
	m := testManager(t, rt)
	p, e := m.Connect(context.Background(), ConnectOptions{Name: "work", Access: Write, RedirectURL: "http://127.0.0.1:44999/callback", FetchCode: func(ctx context.Context, a *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		u, e := url.Parse(a.URL)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		challenge = q.Get("code_challenge")
		if q.Get("resource") != Origin+"/mcp" {
			t.Error("incorrect verified OAuth resource")
		}
		if q.Get("code_challenge_method") != "S256" || len(q.Get("state")) < 32 {
			t.Error(q)
		}
		return &auth.AuthorizationResult{Code: "code", State: q.Get("state"), Iss: Origin}, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	client, e := m.HTTPClient(context.Background(), p.ID)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, e := client.Post(Endpoint(Write), "application/json", strings.NewReader(`{}`))
			if e != nil {
				t.Error(e)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("refreshes=%d", refreshes)
	}
	for i, v := range dispatched {
		want := "Bearer rotated-secret"
		if i == 0 {
			want = "Bearer initial-secret"
		}
		if v != want {
			t.Errorf("dispatch[%d]=%s", i, v)
		}
	}
	c, e := m.readFrom(StoreKeyring, p.ID)
	if e != nil || c.Token.RefreshToken != "refresh-new" {
		t.Fatal("rotation not saved", e)
	}
}
func TestLinearRefreshSaveFailurePreventsDispatchAndImportNeverRefreshes(t *testing.T) {
	n := 0
	m := testManager(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n++
		if r.URL.Path == "/token" {
			return response(`{"access_token":"new","refresh_token":"rotated","token_type":"Bearer","expires_in":3600}`), nil
		}
		return response(`{}`), nil
	}))
	p, e := m.Import(context.Background(), "work", Read, strings.NewReader("old"))
	if e != nil {
		t.Fatal(e)
	}
	c, _ := m.readFrom(StoreKeyring, p.ID)
	c.Token.Expiry = time.Unix(1, 0)
	c.Token.RefreshToken = "refresh"
	c.ClientID = "client"
	c.Imported = false
	if e = m.writeTo(StoreKeyring, p.ID, c); e != nil {
		t.Fatal(e)
	}
	client, e := m.HTTPClient(context.Background(), p.ID)
	if e != nil {
		t.Fatal(e)
	}
	m.config.Store.(*memoryStore).fail = true
	if _, e = client.Get(Endpoint(Read)); e == nil {
		t.Fatal("save failure ignored")
	}
	if n != 2 {
		t.Fatalf("dispatched after failure: n=%d", n)
	}
	m.config.Store.(*memoryStore).fail = false
	c.Imported = true
	if e = m.writeTo(StoreKeyring, p.ID, c); e != nil {
		t.Fatal(e)
	}
	if _, e = client.Get(Endpoint(Read)); e == nil {
		t.Fatal("import fabricated refresh")
	}
	if n != 2 {
		t.Fatal("import attempted network refresh")
	}
}
func TestLinearOAuthRejectsForeignMetadataBeforeRegistration(t *testing.T) {
	n := 0
	m := testManager(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n++
		return response(`{"resource":"https://mcp.linear.app/mcp","authorization_servers":["https://evil.example"]}`), nil
	}))
	_, e := m.Connect(context.Background(), ConnectOptions{Name: "work", Access: Read, RedirectURL: "http://127.0.0.1:44999/callback", FetchCode: func(context.Context, *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		t.Fatal("fetch invoked")
		return nil, nil
	}})
	if e == nil || n != 1 {
		t.Fatal(e, n)
	}
}
func TestLinearLoopbackStatePathMethodAndCancellation(t *testing.T) {
	var redirect string
	state := "expected-state"
	calls := 0
	urlValue, fetch, close, e := loopback(state, func(authURL string) error {
		calls++
		u, _ := url.Parse(redirect)
		bad := *u
		bad.Path = "/wrong"
		resp, e := http.Get(bad.String() + "?state=" + state + "&code=x")
		if e != nil {
			return e
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Error(resp.StatusCode)
		}
		resp, e = http.Post(redirect+"?state="+state+"&code=x", "", nil)
		if e != nil {
			return e
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Error(resp.StatusCode)
		}
		resp, e = http.Get(redirect + "?state=incorrect&code=x")
		if e != nil {
			return e
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Error(resp.StatusCode)
		}
		resp, e = http.Get(redirect + "?state=" + state + "&code=accepted&iss=" + url.QueryEscape(Origin))
		if e != nil {
			return e
		}
		resp.Body.Close()
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	defer close()
	redirect = urlValue
	r, e := fetch(context.Background(), &auth.AuthorizationArgs{URL: "unused"})
	if e != nil || r.Code != "accepted" || calls != 1 {
		t.Fatal(r, e, calls)
	}
	_, fetch2, close2, e := loopback(state, func(string) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	defer close2()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = fetch2(ctx, &auth.AuthorizationArgs{}); e == nil {
		t.Fatal("cancel ignored")
	}
}
