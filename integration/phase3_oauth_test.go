package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/auth"
	"clawbridge/internal/bus"
	"clawbridge/internal/mirror"
	"clawbridge/internal/remote"
)

type oauthRecordingStore struct {
	mu      sync.Mutex
	values  map[string]auth.Credential
	saves   int
	deletes int
	loads   int
}

func (s *oauthRecordingStore) Load(_ context.Context, key string) (auth.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	value, ok := s.values[key]
	if !ok {
		return auth.Credential{}, auth.ErrCredentialNotFound
	}
	return value, nil
}

func (s *oauthRecordingStore) Save(_ context.Context, key string, value auth.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]auth.Credential{}
	}
	s.values[key] = value
	s.saves++
	return nil
}

func (s *oauthRecordingStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	s.deletes++
	return nil
}

func (s *oauthRecordingStore) snapshot() []auth.Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]auth.Credential, 0, len(s.values))
	for _, value := range s.values {
		out = append(out, value)
	}
	return out
}

const (
	reviewerOAuthClientID       = "reviewer-client.apps.googleusercontent.com"
	reviewerBrowserClientSecret = "synthetic-review-browser-installed-app-secret"
	reviewerDeviceClientSecret  = "synthetic-review-device-installed-app-secret"
)

func oauthTestConfig(serverURL string, store auth.Store) auth.Config {
	return auth.Config{
		ClientID:     reviewerOAuthClientID,
		ClientSecret: reviewerBrowserClientSecret,
		Endpoints:    auth.Endpoints{AuthorizationURL: "https://accounts.example.test/auth", TokenURL: serverURL + "/token", DeviceAuthURL: serverURL + "/device"},
		Store:        store,
		Verify: func(_ context.Context, token, audience string) (auth.Identity, error) {
			if token != "synthetic-id-token" {
				return auth.Identity{}, errors.New("unexpected synthetic ID token")
			}
			if audience != reviewerOAuthClientID {
				return auth.Identity{}, errors.New("unexpected audience")
			}
			return auth.Identity{Principal: "reviewer@example.test", Subject: "reviewer-subject"}, nil
		},
	}
}

func TestOAuthBrowserPKCEStateFailureAndSingleUse(t *testing.T) {
	var exchanges atomic.Int32
	var formLock sync.Mutex
	var form url.Values
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		formLock.Lock()
		form = r.PostForm
		formLock.Unlock()
		exchanges.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "synthetic-access-must-not-persist",
			"refresh_token": "synthetic-refresh-persist-only",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"id_token":      "synthetic-id-token",
		})
	}))
	defer tokenServer.Close()

	store := &oauthRecordingStore{}
	opened := make(chan string, 1)
	config := oauthTestConfig(tokenServer.URL, store)
	config.OpenBrowser = func(_ context.Context, target string) error {
		opened <- target
		return nil
	}
	type loginResult struct {
		credential auth.Credential
		err        error
	}
	result := make(chan loginResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		credential, err := auth.BrowserLogin(ctx, config)
		result <- loginResult{credential: credential, err: err}
	}()

	target := <-opened
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	redirect, err := url.Parse(query.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Port() == "" || redirect.Port() == "0" {
		t.Fatalf("unsafe callback redirect: %q", query.Get("redirect_uri"))
	}
	if query.Get("response_type") != "code" || query.Get("access_type") != "offline" ||
		query.Get("scope") != "openid email" || query.Get("code_challenge_method") != "S256" ||
		query.Get("state") == "" || query.Get("code_challenge") == "" {
		t.Fatalf("incomplete browser authorization request: %s", target)
	}
	if query.Get("state") == query.Get("code_challenge") {
		t.Fatal("OAuth state and PKCE challenge must be independent random values")
	}

	wrong, err := http.Get(query.Get("redirect_uri") + "?code=ignored-wrong-state&state=" + url.QueryEscape(query.Get("state")+"x"))
	if err != nil {
		t.Fatal(err)
	}
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong state callback status=%d, want 400", wrong.StatusCode)
	}
	if exchanges.Load() != 0 || len(store.snapshot()) != 0 {
		t.Fatal("wrong state reached token exchange or durable storage")
	}

	valid, err := http.Get(query.Get("redirect_uri") + "?code=synthetic-code&state=" + url.QueryEscape(query.Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	valid.Body.Close()
	if valid.StatusCode != http.StatusOK {
		t.Fatalf("valid callback status=%d, want 200", valid.StatusCode)
	}
	completed := <-result
	if completed.err != nil {
		t.Fatal(completed.err)
	}

	formLock.Lock()
	exchanged := form
	formLock.Unlock()
	verifier := exchanged.Get("code_verifier")
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != query.Get("code_challenge") {
		t.Fatal("token exchange verifier does not match the S256 challenge")
	}
	if exchanged.Get("code") != "synthetic-code" || exchanged.Get("redirect_uri") != query.Get("redirect_uri") {
		t.Fatalf("token exchange lost code or redirect binding: %v", exchanged)
	}
	if exchanged.Get("client_secret") != reviewerBrowserClientSecret {
		t.Fatal("browser token exchange did not use the matching installed-app secret")
	}
	if exchanges.Load() != 1 {
		t.Fatalf("token exchanges=%d, want exactly one", exchanges.Load())
	}

	credentials := store.snapshot()
	if len(credentials) != 1 {
		t.Fatalf("stored credentials=%d, want one", len(credentials))
	}
	if credentials[0].RefreshToken != "synthetic-refresh-persist-only" ||
		credentials[0].Principal != "reviewer@example.test" || credentials[0].Subject != "reviewer-subject" {
		t.Fatalf("unexpected durable credential: %#v", credentials[0])
	}
	durable, _ := json.Marshal(credentials[0])
	for _, forbidden := range []string{"synthetic-access-must-not-persist", "synthetic-id-token", "synthetic-code", verifier} {
		if bytes.Contains(durable, []byte(forbidden)) {
			t.Fatalf("durable credential contains short-lived OAuth material %q: %s", forbidden, durable)
		}
	}

	// BrowserLogin closes its one-shot listener after the first accepted callback.
	if replay, replayErr := http.Get(query.Get("redirect_uri") + "?code=second-code&state=" + url.QueryEscape(query.Get("state"))); replayErr == nil {
		replay.Body.Close()
		t.Fatalf("callback listener remained available after completion: status=%d", replay.StatusCode)
	}
	if exchanges.Load() != 1 {
		t.Fatal("callback replay reached the token endpoint")
	}
}

type deviceResponseMode string

const (
	deviceSlowDown deviceResponseMode = "slow_down"
	deviceDenied   deviceResponseMode = "denial"
	deviceExpiry   deviceResponseMode = "expiry"
)

func TestOAuthDeviceFlowPendingSlowDownDenialAndExpiry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        deviceResponseMode
		expiresIn   int
		wantSuccess bool
		minElapsed  time.Duration
		maxElapsed  time.Duration
	}{
		{name: "slow-down-then-success", mode: deviceSlowDown, expiresIn: 20, wantSuccess: true, minElapsed: 5 * time.Second, maxElapsed: 15 * time.Second},
		{name: "denial", mode: deviceDenied, expiresIn: 20, wantSuccess: false, maxElapsed: 5 * time.Second},
		{name: "expiry", mode: deviceExpiry, expiresIn: 2, wantSuccess: false, maxElapsed: 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var polls atomic.Int32
			var pollTimes []time.Time
			var timesMu sync.Mutex
			tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/device":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"device_code":      "synthetic-device-code",
						"user_code":        "REVIEW-ONE",
						"verification_url": "https://www.google.com/device",
						"expires_in":       tc.expiresIn,
						"interval":         1,
					})
				case "/token":
					if err := r.ParseForm(); err != nil {
						t.Error(err)
						return
					}
					if got := r.PostForm.Get("client_id"); got != reviewerOAuthClientID {
						t.Errorf("device token poll client_id=%q, want %q", got, reviewerOAuthClientID)
					}
					if got := r.PostForm.Get("client_secret"); got != reviewerDeviceClientSecret {
						t.Error("device token poll did not use the matching installed-app secret")
					}
					poll := polls.Add(1)
					timesMu.Lock()
					pollTimes = append(pollTimes, time.Now())
					timesMu.Unlock()
					switch tc.mode {
					case deviceSlowDown:
						if poll == 1 {
							w.WriteHeader(http.StatusBadRequest)
							_ = json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
							return
						}
					case deviceDenied:
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
						return
					case deviceExpiry:
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"access_token":  "synthetic-device-access",
						"refresh_token": "synthetic-device-refresh",
						"token_type":    "Bearer",
						"expires_in":    3600,
						"id_token":      "synthetic-id-token",
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer tokenServer.Close()

			store := &oauthRecordingStore{}
			config := oauthTestConfig(tokenServer.URL, store)
			config.ClientSecret = reviewerDeviceClientSecret
			config.OpenBrowser = func(context.Context, string) error {
				t.Error("device login attempted to open a browser")
				return errors.New("browser must not be opened")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
			defer cancel()
			var prompts atomic.Int32
			started := time.Now()
			credential, err := auth.DeviceLogin(ctx, config, func(prompt auth.DevicePrompt) {
				prompts.Add(1)
				if prompt.UserCode != "REVIEW-ONE" || prompt.VerificationURL != "https://www.google.com/device" || prompt.ExpiresIn <= 0 {
					t.Errorf("invalid device prompt: %+v", prompt)
				}
			})
			elapsed := time.Since(started)
			if prompts.Load() != 1 {
				t.Fatalf("device prompt count=%d, want one", prompts.Load())
			}
			if tc.maxElapsed > 0 && elapsed > tc.maxElapsed {
				t.Fatalf("device flow took %s, above %s bound", elapsed, tc.maxElapsed)
			}
			if tc.minElapsed > 0 && elapsed < tc.minElapsed {
				t.Fatalf("device flow completed in %s before slow_down interval %s", elapsed, tc.minElapsed)
			}
			if tc.mode == deviceSlowDown && polls.Load() < 2 {
				t.Fatalf("slow_down did not retry: polls=%d", polls.Load())
			}
			if tc.mode == deviceSlowDown {
				timesMu.Lock()
				delay := pollTimes[1].Sub(pollTimes[0])
				timesMu.Unlock()
				if delay < 5*time.Second {
					t.Fatalf("slow_down retry delay=%s, want at least five seconds beyond the initial interval", delay)
				}
			}
			if tc.wantSuccess {
				if err != nil || credential.RefreshToken != "synthetic-device-refresh" {
					t.Fatalf("device approval failed: %#v %v", credential, err)
				}
				if len(store.snapshot()) != 1 {
					t.Fatalf("successful device login stored %d credentials", len(store.snapshot()))
				}
			} else {
				if err == nil {
					t.Fatalf("terminal device failure succeeded: %#v", credential)
				}
				if len(store.snapshot()) != 0 {
					t.Fatalf("terminal device failure persisted %d credentials", len(store.snapshot()))
				}
				for _, sentinel := range []string{"synthetic-device-code", "REVIEW-ONE", "synthetic-device-access", "synthetic-device-refresh", "synthetic-id-token"} {
					if strings.Contains(err.Error(), sentinel) {
						t.Fatalf("terminal device failure echoed %q: %v", sentinel, err)
					}
				}
			}
		})
	}
}

func TestOAuthRuntimeRefreshesAndCachesOnlyIdentityToken(t *testing.T) {
	var refreshes atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		refreshes.Add(1)
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "synthetic-runtime-refresh" {
			t.Errorf("unexpected refresh request fields")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "synthetic-runtime-access",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     "synthetic-id-token",
		})
	}))
	defer tokenServer.Close()

	store := &oauthRecordingStore{values: map[string]auth.Credential{
		auth.Key("reviewer-client.apps.googleusercontent.com"): {
			Version:      1,
			ClientID:     "reviewer-client.apps.googleusercontent.com",
			Principal:    "reviewer@example.test",
			Subject:      "reviewer-subject",
			RefreshToken: "synthetic-runtime-refresh",
		},
	}}
	config := oauthTestConfig(tokenServer.URL, store)
	source, err := auth.NewTokenSource(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		token, err := source.Token(context.Background())
		if err != nil || token != "synthetic-id-token" {
			t.Fatalf("runtime token %d: %q %v", i, token, err)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh requests=%d, want one in-memory mint", refreshes.Load())
	}
	if store.loads != 1 || store.saves != 0 {
		t.Fatalf("keychain loads=%d saves=%d, want one load and no runtime save", store.loads, store.saves)
	}
	credentials := store.snapshot()
	if len(credentials) != 1 {
		t.Fatalf("durable credentials changed during runtime mint: %d", len(credentials))
	}
	durable, _ := json.Marshal(credentials[0])
	if bytes.Contains(durable, []byte("synthetic-runtime-access")) || bytes.Contains(durable, []byte("synthetic-id-token")) {
		t.Fatalf("runtime tokens became durable: %s", durable)
	}
}

func TestOAuthExpectedPrincipalIsRejectedBeforeCredentialSave(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/device" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "synthetic-device-code",
				"user_code":        "PRINCIPAL-REJECT",
				"verification_url": "https://www.google.com/device",
				"expires_in":       600,
				"interval":         1,
			})
			return
		}
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "synthetic-access-not-durable",
			"refresh_token": "synthetic-refresh-must-not-save",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"id_token":      "synthetic-id-token",
		})
	}))
	defer tokenServer.Close()

	t.Run("browser", func(t *testing.T) {
		store := &oauthRecordingStore{}
		opened := make(chan string, 1)
		config := oauthTestConfig(tokenServer.URL, store)
		config.ExpectedPrincipal = "different@example.test"
		config.OpenBrowser = func(_ context.Context, target string) error {
			opened <- target
			return nil
		}
		result := make(chan error, 1)
		go func() {
			_, err := auth.BrowserLogin(context.Background(), config)
			result <- err
		}()
		target, err := url.Parse(<-opened)
		if err != nil {
			t.Fatal(err)
		}
		callback := target.Query().Get("redirect_uri") + "?code=synthetic-code&state=" + url.QueryEscape(target.Query().Get("state"))
		response, err := http.Get(callback)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("callback status=%d", response.StatusCode)
		}
		assertPrincipalRejectedBeforeSave(t, <-result, store)
	})

	t.Run("device", func(t *testing.T) {
		store := &oauthRecordingStore{}
		config := oauthTestConfig(tokenServer.URL, store)
		config.ClientSecret = reviewerDeviceClientSecret
		config.ExpectedPrincipal = "different@example.test"
		var prompts atomic.Int32
		_, err := auth.DeviceLogin(context.Background(), config, func(auth.DevicePrompt) {
			prompts.Add(1)
		})
		if prompts.Load() != 1 {
			t.Fatalf("device prompts=%d, want one", prompts.Load())
		}
		assertPrincipalRejectedBeforeSave(t, err, store)
	})
}

func assertPrincipalRejectedBeforeSave(t *testing.T, err error, store *oauthRecordingStore) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "signed-in Google principal does not match") {
		t.Fatalf("principal mismatch error=%v", err)
	}
	store.mu.Lock()
	saves := store.saves
	values := len(store.values)
	store.mu.Unlock()
	if saves != 0 || values != 0 {
		t.Fatalf("principal mismatch saved credentials: saves=%d values=%d", saves, values)
	}
	if strings.Contains(err.Error(), "synthetic-refresh-must-not-save") || strings.Contains(err.Error(), "synthetic-id-token") {
		t.Fatalf("principal rejection echoed OAuth material: %v", err)
	}
}

func TestPhase3CredentialFailureStopsBeforeTransportAndMirror(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "mirror.db")
	binding := mirror.Binding{
		Server: "http://127.0.0.1:1", Audience: "review-audience",
		Principal: "reviewer@example.test", Workspace: "workspace-id",
		Project: "project-id", Session: "reviewer", Instance: "reviewer-instance",
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := mirror.Save(context.Background(), cachePath, mirror.Snapshot{
		Binding: binding, UpdatedAt: now,
		Data: map[string]any{
			"workspace": map[string]any{"id": "workspace-id"},
			"project":   map[string]any{"id": "project-id"},
			"session":   map[string]any{"id": "reviewer", "name": "reviewer", "instance": "reviewer-instance"},
			"principal": binding.Principal, "verified": true,
			"identity_provenance": "google-id-token", "storage_mode": "cloud",
			"snapshot_at":   now,
			"sessions":      []any{map[string]any{"id": "reviewer", "name": "reviewer", "instance": "reviewer-instance", "status": "active"}},
			"messages":      []any{map[string]any{"id": "must-not-be-returned", "sequence": 1, "body": "cached-private-body", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}},
			"sent_messages": []any{},
			"events":        []any{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	config := remote.Config{
		URL: "http://127.0.0.1:1", Audience: "review-audience",
		Principal: binding.Principal, CachePath: cachePath,
	}
	request := bus.Request{Operation: "inbox", Workspace: "workspace-id", Project: "project-id", Session: "reviewer", Args: map[string]any{"instance": "reviewer-instance"}}
	for name, source := range map[string]remote.TokenSource{
		"credential-read": remote.TokenSourceFunc(func(context.Context) (string, error) {
			return "", errors.New("synthetic-keychain-failure-secret")
		}),
		"refresh-or-mint": remote.TokenSourceFunc(func(context.Context) (string, error) {
			return "", errors.New("synthetic-refresh-failure-secret")
		}),
	} {
		t.Run(name, func(t *testing.T) {
			client, err := remote.NewWithTokenSource(config, source)
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Execute(context.Background(), request)
			if err == nil {
				t.Fatalf("credential failure returned result source %q with cached body", result.Source)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("credential failure echoed injected secret: %v", err)
			}
		})
	}
	after, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("credential failure changed mirror bytes")
	}
}

func TestOAuthStagedCredentialsRollbackNewAndReplacedEntries(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/device" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "synthetic-device-code",
				"user_code":        "ROLLBACK-DEVICE",
				"verification_url": "https://www.google.com/device",
				"expires_in":       600,
				"interval":         1,
			})
			return
		}
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "synthetic-access-not-durable",
			"refresh_token": "synthetic-new-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"id_token":      "synthetic-id-token",
		})
	}))
	defer tokenServer.Close()

	newCredential := auth.Credential{
		Version: 1, ClientID: "reviewer-client.apps.googleusercontent.com",
		Principal: "reviewer@example.test", Subject: "reviewer-subject",
		RefreshToken: "synthetic-new-refresh",
	}
	priorCredential := auth.Credential{
		Version: 1, ClientID: newCredential.ClientID,
		Principal: newCredential.Principal, Subject: newCredential.Subject,
		RefreshToken: "synthetic-prior-refresh",
	}
	for _, tc := range []struct {
		name    string
		initial *auth.Credential
	}{
		{name: "new-entry"},
		{name: "replaced-entry", initial: &priorCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &oauthRecordingStore{}
			if tc.initial != nil {
				store.values = map[string]auth.Credential{auth.Key(newCredential.ClientID): *tc.initial}
			}
			transaction, err := auth.StageCredential(context.Background(), store, newCredential.ClientID, newCredential)
			if err != nil {
				t.Fatal(err)
			}
			if len(store.snapshot()) != 1 || store.snapshot()[0].RefreshToken != newCredential.RefreshToken {
				t.Fatal("staged credential was not saved")
			}
			if err := transaction.Rollback(context.Background()); err != nil {
				t.Fatal(err)
			}
			values := store.snapshot()
			if tc.initial == nil {
				if len(values) != 0 {
					t.Fatalf("rollback retained newly created credential: %#v", values)
				}
				return
			}
			if len(values) != 1 || values[0] != *tc.initial {
				t.Fatalf("rollback did not restore prior credential: got=%#v want=%#v", values, *tc.initial)
			}
		})
	}

	for _, flow := range []string{"browser", "device"} {
		t.Run("staged-login-"+flow, func(t *testing.T) {
			store := &oauthRecordingStore{values: map[string]auth.Credential{
				auth.Key(newCredential.ClientID): priorCredential,
			}}
			config := oauthTestConfig(tokenServer.URL, store)
			var credential auth.Credential
			var transaction *auth.CredentialTransaction
			var err error
			if flow == "browser" {
				opened := make(chan string, 1)
				config.OpenBrowser = func(_ context.Context, target string) error {
					opened <- target
					return nil
				}
				result := make(chan struct {
					credential  auth.Credential
					transaction *auth.CredentialTransaction
					err         error
				}, 1)
				go func() {
					c, tx, e := auth.BrowserLoginStaged(context.Background(), config)
					result <- struct {
						credential  auth.Credential
						transaction *auth.CredentialTransaction
						err         error
					}{c, tx, e}
				}()
				target, parseErr := url.Parse(<-opened)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				callback := target.Query().Get("redirect_uri") + "?code=synthetic-code&state=" + url.QueryEscape(target.Query().Get("state"))
				response, requestErr := http.Get(callback)
				if requestErr != nil {
					t.Fatal(requestErr)
				}
				response.Body.Close()
				completed := <-result
				credential, transaction, err = completed.credential, completed.transaction, completed.err
			} else {
				config.ClientSecret = reviewerDeviceClientSecret
				credential, transaction, err = auth.DeviceLoginStaged(context.Background(), config, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if credential.RefreshToken != newCredential.RefreshToken || transaction == nil {
				t.Fatalf("staged login result=%#v transaction=%v", credential, transaction)
			}
			if err = transaction.Rollback(context.Background()); err != nil {
				t.Fatal(err)
			}
			values := store.snapshot()
			if len(values) != 1 || values[0] != priorCredential {
				t.Fatalf("failed config transaction did not restore prior keychain entry: %#v", values)
			}
			durable, _ := json.Marshal(values[0])
			for _, forbidden := range []string{"synthetic-access-not-durable", "synthetic-id-token"} {
				if bytes.Contains(durable, []byte(forbidden)) {
					t.Fatalf("durable keychain entry contains short-lived material %q: %s", forbidden, durable)
				}
			}
		})
	}
}

func TestOAuthDeviceClientKeyPromotionRuntimeAndLogoutStayCoherent(t *testing.T) {
	const browserClientID = "browser-client.apps.googleusercontent.com"
	const deviceClientID = "device-client.apps.googleusercontent.com"
	var deviceAcquisitions atomic.Int32
	var refreshes atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/device" {
			deviceAcquisitions.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if got := r.PostForm.Get("client_id"); got != deviceClientID {
				t.Errorf("device acquisition client_id=%q, want %q", got, deviceClientID)
			}
			if got := r.PostForm.Get("client_secret"); got != "" {
				t.Errorf("device acquisition sent unexpected client_secret=%q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "synthetic-device-code",
				"user_code":        "DEVICE-KEY",
				"verification_url": "https://www.google.com/device",
				"expires_in":       600,
				"interval":         1,
			})
			return
		}
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.PostForm.Get("grant_type") == "refresh_token" {
			refreshes.Add(1)
			if got := r.PostForm.Get("client_id"); got != deviceClientID {
				t.Errorf("runtime refresh client_id=%q, want promoted %q", got, deviceClientID)
			}
			if got := r.PostForm.Get("client_secret"); got != reviewerDeviceClientSecret {
				t.Error("runtime refresh did not use the promoted device installed-app secret")
			}
		} else {
			if got := r.PostForm.Get("client_secret"); got != reviewerDeviceClientSecret {
				t.Error("device token poll did not use the matching installed-app secret")
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "synthetic-device-access",
			"refresh_token": "synthetic-device-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"id_token":      "synthetic-id-token",
		})
	}))
	defer tokenServer.Close()

	store := &oauthRecordingStore{}
	loginConfig := oauthTestConfig(tokenServer.URL, store)
	loginConfig.ClientID = deviceClientID
	loginConfig.ClientSecret = reviewerDeviceClientSecret
	loginConfig.Verify = func(_ context.Context, token, audience string) (auth.Identity, error) {
		if token != "synthetic-id-token" || audience != deviceClientID {
			return auth.Identity{}, errors.New("unexpected synthetic device identity token or audience")
		}
		return auth.Identity{Principal: "reviewer@example.test", Subject: "reviewer-subject"}, nil
	}
	credential, transaction, err := auth.DeviceLoginStaged(context.Background(), loginConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if deviceAcquisitions.Load() != 1 || credential.ClientID != deviceClientID {
		t.Fatalf("device acquisition=%d credential=%#v", deviceAcquisitions.Load(), credential)
	}
	if _, ok := store.values[auth.Key(deviceClientID)]; !ok {
		t.Fatalf("device credential was not keyed by acquisition client ID: %#v", store.values)
	}
	if _, ok := store.values[auth.Key(browserClientID)]; ok {
		t.Fatal("device credential was keyed by browser client ID")
	}

	// Device login promotes this client ID into trusted oauth_client_id.
	activeClientID := deviceClientID
	if auth.Key(activeClientID) != auth.Key(credential.ClientID) {
		t.Fatal("promoted active client does not address the staged device credential")
	}
	transaction.Commit()

	runtimeConfig := oauthTestConfig(tokenServer.URL, store)
	runtimeConfig.ClientID = activeClientID
	runtimeConfig.ClientSecret = reviewerDeviceClientSecret
	runtimeConfig.ExpectedPrincipal = credential.Principal
	runtimeConfig.Verify = loginConfig.Verify
	source, err := auth.NewTokenSource(context.Background(), runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token(context.Background())
	if err != nil || token != "synthetic-id-token" || refreshes.Load() != 1 {
		t.Fatalf("runtime refresh token=%q requests=%d err=%v", token, refreshes.Load(), err)
	}
	if err = auth.Logout(context.Background(), store, activeClientID); err != nil {
		t.Fatal(err)
	}
	if len(store.snapshot()) != 0 {
		t.Fatalf("logout used a different key and left credential behind: %#v", store.snapshot())
	}
	if err = auth.Logout(context.Background(), store, activeClientID); err != nil {
		t.Fatalf("idempotent logout failed: %v", err)
	}
}

func TestPhase3CredentialSourcesAreExclusiveAndRepositoryCannotInject(t *testing.T) {
	p := newProcessFixture(t)
	gitAt(t, p.dir, "init", "--quiet")
	home := filepath.Join(p.dir, "phase3-state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.toml")
	env := append(append([]string{}, p.env...), "CLAWBRIDGE_HOME="+home)
	runConfig := func(config string, args ...string) ([]byte, []byte, error) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(p.binary, args...)
		cmd.Dir = p.dir
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	}

	for _, tc := range []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "oauth-with-gcloud-identity",
			config: "mode='cloud'\nserver='https://clawbridge.example.test'\naudience='desktop-client'\nprincipal='human@example.test'\ncredential_source='oauth'\noauth_client_id='desktop-client'\naccount='human@example.test'\n",
			want:   "cannot be combined",
		},
		{
			name:   "gcloud-with-oauth-client",
			config: "mode='cloud'\nserver='https://clawbridge.example.test'\naudience='audience'\nprincipal='human@example.test'\ncredential_source='gcloud'\noauth_client_id='desktop-client'\naccount='human@example.test'\nimpersonate_service_account='agent@example.test'\n",
			want:   "only with credential_source=oauth",
		},
		{
			name:   "adc-without-explicit-file",
			config: "mode='cloud'\nserver='https://clawbridge.example.test'\naudience='audience'\nprincipal='human@example.test'\ncredential_source='adc'\naccount='human@example.test'\nimpersonate_service_account='agent@example.test'\n",
			want:   "requires credentials_file",
		},
		{
			name:   "unknown-backend-is-not-inferred",
			config: "mode='cloud'\nserver='https://clawbridge.example.test'\naudience='audience'\ncredential_source='metadata'\n",
			want:   "unsupported credential source",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runConfig(tc.config, "whoami")
			if err == nil {
				t.Fatalf("exclusive backend configuration succeeded: stdout=%s", stdout)
			}
			combined := string(stdout) + string(stderr)
			if !strings.Contains(combined, tc.want) {
				t.Fatalf("failure did not identify exclusive backend contract: %s", combined)
			}
			for _, sentinel := range []string{"desktop-client", "agent@example.test", "human@example.test"} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("configuration failure echoed credential-routing sentinel %q: %s", sentinel, combined)
				}
			}
		})
	}

	// A repository file may select scope, but cannot inject a trusted cloud route
	// or switch authority mode. A malicious cloud mode must leave the trusted
	// local route effective.
	localConfig := "mode='local'\nworkspace='review-workspace'\ndb='" + p.db + "'\n"
	if err := os.WriteFile(configPath, []byte(localConfig), 0600); err != nil {
		t.Fatal(err)
	}
	repo := "mode='cloud'\nserver='https://attacker.example.test'\naudience='attacker-audience'\ncredential_source='oauth'\noauth_client_id='attacker-client'\nworkspace='attacker-workspace'\nproject='attacker-project'\n"
	if err := os.WriteFile(filepath.Join(p.dir, ".clawbridge.toml"), []byte(repo), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p.binary, "setup", "--local", "--workspace", "review-workspace")
	cmd.Dir = p.dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("repository cloud mode overrode trusted local authority: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	combined := stdout.String() + stderr.String()
	for _, injected := range []string{"attacker.example.test", "attacker-audience", "attacker-client", "attacker-workspace", "attacker-project"} {
		if strings.Contains(combined, injected) {
			t.Fatalf("repository route was accepted or echoed: %q in %s", injected, combined)
		}
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "mode='local'") && !strings.Contains(string(after), "mode = 'local'") && !strings.Contains(string(after), "mode = \"local\"") {
		t.Fatalf("trusted local mode was not preserved: %s", after)
	}
	for _, injected := range []string{"attacker.example.test", "attacker-audience", "attacker-client", "credential_source"} {
		if strings.Contains(string(after), injected) {
			t.Fatalf("repository injected %q into trusted configuration: %s", injected, after)
		}
	}

	// With no repository mode override, untrusted scope can overlay trusted local
	// routing and must not change its mode, DB, or credential-related bytes.
	repo = "workspace='review-workspace'\nproject='repo-project'\nserver='https://attacker.example.test'\naudience='attacker-audience'\ncredential_source='oauth'\noauth_client_id='attacker-client'\n"
	if err := os.WriteFile(filepath.Join(p.dir, ".clawbridge.toml"), []byte(repo), 0600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(p.binary, "setup", "--local", "--workspace", "review-workspace")
	cmd.Dir = p.dir
	cmd.Env = env
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("safe repository scope could not overlay trusted local routing: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	after, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "mode='local'") && !strings.Contains(string(after), "mode = 'local'") && !strings.Contains(string(after), "mode = \"local\"") {
		t.Fatalf("trusted local mode was not preserved after safe repository overlay: %s", after)
	}
	for _, injected := range []string{"attacker.example.test", "attacker-audience", "attacker-client", "credential_source"} {
		if strings.Contains(string(after), injected) {
			t.Fatalf("repository injected %q into trusted configuration: %s", injected, after)
		}
	}

	// The reverse direction is equally authoritative: a repository mode=local
	// cannot downgrade a trusted cloud route.
	cloudConfig := "mode='cloud'\nserver='https://trusted.example.test'\naudience='trusted-client'\nprincipal='reviewer@example.test'\ncache='" + filepath.Join(home, "trusted-cache.db") + "'\nworkspace='review-workspace'\ncredential_source='oauth'\noauth_client_id='trusted-client'\n"
	if err := os.WriteFile(configPath, []byte(cloudConfig), 0600); err != nil {
		t.Fatal(err)
	}
	repo = "mode='local'\nworkspace='review-workspace'\nproject='repo-project'\ndb='" + filepath.Join(p.dir, "attacker.db") + "'\n"
	if err := os.WriteFile(filepath.Join(p.dir, ".clawbridge.toml"), []byte(repo), 0600); err != nil {
		t.Fatal(err)
	}
	// Credential discovery happens before any HTTP request, so a missing test
	// keychain safely fails closed. The diagnostic must identify OAuth/keychain,
	// proving the trusted cloud route survived repository mode=local.
	stdout.Reset()
	stderr.Reset()
	cmd = exec.Command(p.binary, "whoami")
	cmd.Dir = p.dir
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		t.Fatalf("cloud route unexpectedly succeeded without a test OAuth credential: stdout=%s", stdout.String())
	}
	combined = stdout.String() + stderr.String()
	if strings.Contains(combined, "local-authority") || strings.Contains(strings.ToLower(combined), "sqlite") {
		t.Fatalf("repository downgraded trusted cloud routing to local authority: %s", combined)
	}
	if !strings.Contains(strings.ToLower(combined), "oauth") && !strings.Contains(strings.ToLower(combined), "keychain") {
		t.Fatalf("trusted cloud mode did not remain effective: %s", combined)
	}
	for _, injected := range []string{"attacker.db"} {
		if strings.Contains(combined, injected) {
			t.Fatalf("repository local route was accepted or echoed: %q in %s", injected, combined)
		}
	}
}
