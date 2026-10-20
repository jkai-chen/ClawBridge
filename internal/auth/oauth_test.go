package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	mu      sync.Mutex
	values  map[string]Credential
	loads   int
	deletes int
}

func (s *memoryStore) Load(_ context.Context, key string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	value, ok := s.values[key]
	if !ok {
		return Credential{}, ErrCredentialNotFound
	}
	return value, nil
}

func (s *memoryStore) Save(_ context.Context, key string, value Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]Credential{}
	}
	s.values[key] = value
	return nil
}

func (s *memoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	delete(s.values, key)
	return nil
}

func TestBrowserLoginUsesPKCEStateAndSavesOnlyDurableCredential(t *testing.T) {
	var exchanged url.Values
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		exchanged = r.PostForm
		if exchanged.Get("redirect_uri") == "" || exchanged.Get("code_verifier") == "" {
			t.Errorf("exchange omitted PKCE or redirect: %v", exchanged)
		}
		if exchanged.Get("client_secret") != "synthetic-browser-client-credential" {
			t.Errorf("browser exchange omitted the matching installed-app credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "synthetic-access",
			"refresh_token": "synthetic-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"id_token":      "synthetic-id-token",
		})
	}))
	defer server.Close()
	store := &memoryStore{}
	opened := make(chan string, 1)
	config := Config{
		ClientID:     "client.apps.googleusercontent.com",
		ClientSecret: "synthetic-browser-client-credential",
		Endpoints:    Endpoints{AuthorizationURL: "https://accounts.example.test/auth", TokenURL: server.URL, DeviceAuthURL: server.URL + "/device"},
		Store:        store,
		OpenBrowser: func(_ context.Context, target string) error {
			opened <- target
			return nil
		},
		Verify: func(_ context.Context, token, audience string) (Identity, error) {
			if token != "synthetic-id-token" || audience != "client.apps.googleusercontent.com" {
				return Identity{}, errors.New("unexpected token or audience")
			}
			return Identity{Principal: "Alice@Example.Test", Subject: "subject-1"}, nil
		},
	}
	result := make(chan struct {
		credential Credential
		err        error
	}, 1)
	go func() {
		credential, err := BrowserLogin(context.Background(), config)
		result <- struct {
			credential Credential
			err        error
		}{credential, err}
	}()
	target := <-opened
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" ||
		query.Get("state") == "" || query.Get("access_type") != "offline" ||
		query.Get("scope") != "openid email" {
		t.Fatalf("authorization parameters are incomplete: %s", target)
	}
	redirect, err := url.Parse(query.Get("redirect_uri"))
	if err != nil || redirect.Hostname() != "127.0.0.1" {
		t.Fatalf("unsafe callback redirect: %q", query.Get("redirect_uri"))
	}
	callbackURL := query.Get("redirect_uri") + "?code=synthetic-code&state=" + url.QueryEscape(query.Get("state"))
	response, err := http.Get(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d", response.StatusCode)
	}
	completed := <-result
	if completed.err != nil {
		t.Fatal(completed.err)
	}
	if completed.credential.Principal != "alice@example.test" || completed.credential.Subject != "subject-1" ||
		completed.credential.RefreshToken != "synthetic-refresh" || completed.credential.ClientID != config.ClientID {
		t.Fatalf("unexpected durable credential: %+v", completed.credential)
	}
	if exchanged.Get("code") != "synthetic-code" || exchanged.Get("code_verifier") == query.Get("code_challenge") {
		t.Fatalf("exchange did not use the authorization code and PKCE verifier")
	}
	if len(store.values) != 1 {
		t.Fatalf("saved %d credentials", len(store.values))
	}
	for _, value := range store.values {
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), "synthetic-access") || strings.Contains(string(encoded), "synthetic-id-token") {
			t.Fatalf("keychain value contains a short-lived token: %s", encoded)
		}
	}
}

func TestBrowserLoginRejectsWrongStateAndOnlyAllowsOneCallback(t *testing.T) {
	store := &memoryStore{}
	opened := make(chan string, 1)
	result := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, err := BrowserLogin(ctx, Config{
			ClientID:  "client",
			Endpoints: Endpoints{AuthorizationURL: "https://accounts.example.test", TokenURL: "https://accounts.example.test/token", DeviceAuthURL: "https://accounts.example.test/device"},
			Store:     store,
			OpenBrowser: func(_ context.Context, target string) error {
				opened <- target
				return nil
			},
			Verify: func(context.Context, string, string) (Identity, error) {
				return Identity{Principal: "alice@example.test", Subject: "subject"}, nil
			},
		})
		result <- err
	}()
	target := <-opened
	redirect := mustQuery(t, target, "redirect_uri")
	response, err := http.Get(redirect + "?code=code&state=wrong")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong state status %d", response.StatusCode)
	}
	if len(store.values) != 0 {
		t.Fatal("wrong state saved a credential")
	}
	cancel()
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled browser flow: %v", err)
	}
}

func TestDeviceLoginAndRuntimeTokenRefresh(t *testing.T) {
	var polls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/device":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "synthetic-device",
				"user_code":        "CODE-ONE",
				"verification_url": "https://www.google.com/device",
				"expires_in":       600,
				"interval":         1,
			})
		case r.URL.Path == "/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				return
			}
			if r.Form.Get("client_secret") != "synthetic-device-client-credential" {
				t.Error("device token or refresh request omitted the matching installed-app credential")
			}
			if r.Form.Get("grant_type") == "refresh_token" {
				if r.Form.Get("refresh_token") != "synthetic-refresh" {
					t.Errorf("wrong refresh token")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "synthetic-access",
					"token_type":   "Bearer",
					"expires_in":   3600,
					"id_token":     "synthetic-id-token",
				})
				return
			}
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "synthetic-access",
				"refresh_token": "synthetic-refresh",
				"token_type":    "Bearer",
				"expires_in":    3600,
				"id_token":      "synthetic-id-token",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer tokenServer.Close()
	store := &memoryStore{}
	config := Config{
		ClientID:     "client.apps.googleusercontent.com",
		ClientSecret: "synthetic-device-client-credential",
		Endpoints:    Endpoints{AuthorizationURL: "https://accounts.example.test", TokenURL: tokenServer.URL + "/token", DeviceAuthURL: tokenServer.URL + "/device"},
		Store:        store,
		OpenBrowser: func(context.Context, string) error {
			return errors.New("browser must not be used for device login")
		},
		Verify: func(context.Context, string, string) (Identity, error) {
			return Identity{Principal: "bob@example.test", Subject: "subject-2"}, nil
		},
	}
	var prompt DevicePrompt
	credential, err := DeviceLogin(context.Background(), config, func(value DevicePrompt) { prompt = value })
	if err != nil {
		t.Fatal(err)
	}
	if prompt.UserCode != "CODE-ONE" || prompt.VerificationURL != "https://www.google.com/device" || credential.Principal != "bob@example.test" {
		t.Fatalf("device login result: %+v %+v", prompt, credential)
	}
	store.mu.Lock()
	store.loads = 0
	store.mu.Unlock()
	source, err := NewTokenSource(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		token, err := source.Token(context.Background())
		if err != nil || token != "synthetic-id-token" {
			t.Fatalf("token %d: %q %v", i, token, err)
		}
	}
	if store.loads != 1 {
		t.Fatalf("runtime credential loaded %d times, want once after login staging", store.loads)
	}
}

func TestMissingCredentialIsNotAnEmptyLogin(t *testing.T) {
	store := &memoryStore{}
	_, err := NewTokenSource(context.Background(), Config{
		ClientID:  "client",
		Endpoints: Endpoints{AuthorizationURL: "https://accounts.example.test", TokenURL: "https://accounts.example.test/token", DeviceAuthURL: "https://accounts.example.test/device"},
		Store:     store,
	})
	if err == nil || !strings.Contains(err.Error(), "run clawbridge login") {
		t.Fatalf("missing credential error: %v", err)
	}
}

func TestOAuthConfigRequiresCompleteExplicitEndpoints(t *testing.T) {
	_, err := BrowserLogin(context.Background(), Config{
		ClientID:  "client",
		Endpoints: Endpoints{AuthorizationURL: "https://accounts.example.test"},
		Store:     &memoryStore{},
	})
	if err == nil {
		t.Fatal("incomplete endpoint configuration was accepted")
	}
	_ = time.Second
}

func mustQuery(t *testing.T, raw, name string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	value := parsed.Query().Get(name)
	if value == "" {
		t.Fatalf("missing query parameter %s", name)
	}
	return value
}
