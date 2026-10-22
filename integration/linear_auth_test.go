package integration

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/linearconn"
	"github.com/modelcontextprotocol/go-sdk/auth"
)

// Explicitly injected storage only: never construct or access NativeStore.
type l1MemoryCredentials struct {
	mu       sync.Mutex
	values   map[string]string
	sets     int
	gets     int
	failSet  bool
	afterSet func()
}

func (s *l1MemoryCredentials) Get(service, account string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	v, ok := s.values[service+"/"+account]
	if !ok {
		return "", errors.New("synthetic credential absent")
	}
	return v, nil
}
func (s *l1MemoryCredentials) Set(service, account, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets++
	if s.failSet {
		return errors.New("synthetic-private-store-error")
	}
	s.values[service+"/"+account] = value
	if s.afterSet != nil {
		hook := s.afterSet
		s.afterSet = nil
		hook()
	}
	return nil
}
func (s *l1MemoryCredentials) Delete(service, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, service+"/"+account)
	return nil
}
func (s *l1MemoryCredentials) snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

const l1SyntheticAccess = "l1-synthetic-access-credential"
const l1SyntheticRefresh = "l1-synthetic-refresh-credential"
const l1SyntheticClient = "l1-synthetic-public-client"
const l1SyntheticCode = "l1-synthetic-code"

func l1AuthResponse(status int, value any) *http.Response {
	b, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b)))}
}

type l1OAuthFixture struct {
	t                                            *testing.T
	dir                                          string
	store                                        *l1MemoryCredentials
	manager                                      *linearconn.Manager
	challenge                                    string
	state                                        string
	access                                       linearconn.Access
	mode                                         string
	acquisitions, exchanges, refreshes, requests atomic.Int64
	now                                          time.Time
	refreshHook                                  func(*http.Request) error
}

func newL1OAuth(t *testing.T, access linearconn.Access, mode string) *l1OAuthFixture {
	t.Helper()
	f := &l1OAuthFixture{t: t, dir: filepath.Join(t.TempDir(), "profiles"), store: &l1MemoryCredentials{values: map[string]string{}}, access: access, mode: mode, now: time.Now().UTC()}
	f.manager = linearconn.New(linearconn.Config{Directory: f.dir, Store: f.store, Now: func() time.Time { return f.now }, HTTPClient: &http.Client{Transport: transportFunc(f.roundTrip)}, Verify: func(ctx context.Context, client *http.Client, a linearconn.Access) (linearconn.Identity, error) {
		if mode == "verification" {
			return linearconn.Identity{}, errors.New(l1SyntheticAccess + " raw provider body")
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, linearconn.Endpoint(a), strings.NewReader(`{}`))
		if err != nil {
			return linearconn.Identity{}, err
		}
		response, err := client.Do(r)
		if err != nil {
			return linearconn.Identity{}, err
		}
		response.Body.Close()
		return linearconn.Identity{AccountLabel: "synthetic account", WorkspaceID: l1Workspace, WorkspaceLabel: "untrusted display label"}, nil
	}})
	return f
}
func (f *l1OAuthFixture) roundTrip(r *http.Request) (*http.Response, error) {
	f.requests.Add(1)
	if r.URL.Scheme != "https" || r.URL.Host != "mcp.linear.app" || r.URL.User != nil {
		f.t.Errorf("nonofficial original transport URL: %s", r.URL.Host)
		return nil, errors.New("unexpected original URL")
	}
	if r.URL.Path != "/mcp" && r.URL.Path != "/mcp/readonly" && r.Header.Get("Authorization") != "" {
		f.t.Error("credential sent into public discovery/registration/token endpoint")
	}
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource":
		servers := []string{linearconn.Origin}
		if f.mode == "foreign-issuer" {
			servers = []string{"https://attacker.invalid"}
		}
		return l1AuthResponse(200, map[string]any{"resource": linearconn.Origin + "/mcp", "authorization_servers": servers}), nil
	case "/.well-known/oauth-authorization-server":
		tokenURL := linearconn.Origin + "/token"
		registerURL := linearconn.Origin + "/register"
		if f.mode == "foreign-token" {
			tokenURL = "https://attacker.invalid/token"
		}
		if f.mode == "foreign-registration" {
			registerURL = "https://attacker.invalid/register"
		}
		return l1AuthResponse(200, map[string]any{"issuer": linearconn.Origin, "authorization_endpoint": linearconn.Origin + "/authorize", "token_endpoint": tokenURL, "registration_endpoint": registerURL, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}}), nil
	case "/register":
		if r.Method != http.MethodPost {
			f.t.Error("registration method")
		}
		var input map[string]any
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			f.t.Error("invalid DCR request")
		}
		scope := "read"
		if f.access == linearconn.Write {
			scope = "read write"
		}
		if input["token_endpoint_auth_method"] != "none" || input["scope"] != scope || input["application_type"] != "native" {
			f.t.Errorf("DCR public-native scope mismatch: %v", input)
		}
		method := "none"
		if f.mode == "confidential-dcr" {
			method = "client_secret_basic"
		}
		return l1AuthResponse(201, map[string]any{"client_id": l1SyntheticClient, "redirect_uris": input["redirect_uris"], "token_endpoint_auth_method": method}), nil
	case "/token":
		if r.Method != http.MethodPost || r.ParseForm() != nil {
			f.t.Error("invalid token request")
		}
		if r.Form.Get("client_id") != l1SyntheticClient || r.Form.Get("client_secret") != "" {
			f.t.Error("public client mismatch or secret sent")
		}
		if r.Form.Get("grant_type") == "authorization_code" {
			f.exchanges.Add(1)
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge || r.Form.Get("code") != l1SyntheticCode || r.Form.Get("resource") != linearconn.Origin+"/mcp" {
				f.t.Error("PKCE/code/resource mismatch")
			}
		} else {
			f.refreshes.Add(1)
			if f.refreshHook != nil {
				if err := f.refreshHook(r); err != nil {
					return nil, err
				}
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != l1SyntheticRefresh {
				f.t.Error("refresh credential not preserved")
			}
		}
		if f.mode == "token-error" {
			return l1AuthResponse(400, map[string]any{"error": "invalid_grant", "error_description": l1SyntheticRefresh + " private diagnostic"}), nil
		}
		token, refresh := l1SyntheticAccess, l1SyntheticRefresh
		expires := 3600
		if f.refreshes.Load() > 0 {
			token += "-rotated"
			refresh += "-rotated"
			// The refresh library uses wall time, while the manager's injected
			// clock advances two hours. Keep the new token valid under both.
			expires = 10800
		}
		return l1AuthResponse(200, map[string]any{"access_token": token, "refresh_token": refresh, "token_type": "Bearer", "expires_in": expires}), nil
	case "/mcp", "/mcp/readonly":
		if r.URL.String() != linearconn.Endpoint(f.access) {
			f.t.Error("credential sent into wrong access route")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+l1SyntheticAccess && got != "Bearer "+l1SyntheticAccess+"-rotated" {
			f.t.Error("missing/mismatched provider Authorization")
		}
		return l1AuthResponse(200, map[string]any{}), nil
	default:
		f.t.Errorf("unexpected synthetic transport path %s", r.URL.Path)
		return nil, errors.New("unexpected synthetic request")
	}
}
func (f *l1OAuthFixture) inspectAuthorization(raw string) *url.URL {
	f.t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		f.t.Fatal(err)
	}
	q := u.Query()
	scope := "read"
	if f.access == linearconn.Write {
		scope = "read write"
	}
	if u.Scheme != "https" || u.Host != "mcp.linear.app" || u.Path != "/authorize" || q.Get("client_id") != l1SyntheticClient || q.Get("scope") != scope || q.Get("code_challenge_method") != "S256" || len(q.Get("state")) < 32 || q.Get("resource") != linearconn.Origin+"/mcp" {
		f.t.Fatal("authorization routing/state/scope/PKCE mismatch")
	}
	f.state, f.challenge = q.Get("state"), q.Get("code_challenge")
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Port() == "" {
		f.t.Fatal("callback not numeric IPv4 loopback")
	}
	return redirect
}
func (f *l1OAuthFixture) options() linearconn.ConnectOptions {
	return linearconn.ConnectOptions{Name: "synthetic connection", Access: f.access, NoBrowser: true, RedirectURL: "http://127.0.0.1:34127/synthetic-callback", FetchCode: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		f.acquisitions.Add(1)
		f.inspectAuthorization(args.URL)
		state, issuer := f.state, linearconn.Origin
		if f.mode == "wrong-state" {
			state += "wrong"
		}
		if f.mode == "wrong-callback-issuer" {
			issuer = "https://attacker.invalid"
		}
		if f.mode == "denial" {
			return nil, errors.New(l1SyntheticCode + " denied")
		}
		return &auth.AuthorizationResult{Code: l1SyntheticCode, State: state, Iss: issuer}, nil
	}}
}
func l1AssertNoFileCredentials(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range []string{l1SyntheticAccess, l1SyntheticRefresh, l1SyntheticClient, l1SyntheticCode, "synthetic-private-store-error"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("synthetic credential/diagnostic persisted in %s", filepath.Base(path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestL1NativeOAuthPublicClientPKCEAndCredentialRouting(t *testing.T) {
	states := map[string]bool{}
	for _, access := range []linearconn.Access{linearconn.Read, linearconn.Write} {
		t.Run(string(access), func(t *testing.T) {
			f := newL1OAuth(t, access, "")
			p, err := f.manager.Connect(context.Background(), f.options())
			if err != nil {
				t.Fatal(err)
			}
			if p.ID == "" || p.Access != access || p.WorkspaceID != l1Workspace || f.exchanges.Load() != 1 || states[f.state] {
				t.Fatal("profile/exchange/state isolation failed")
			}
			states[f.state] = true
			values := f.store.snapshot()
			if len(values) != 1 || values[linearconn.Service+"/"+p.ID] == "" {
				t.Fatal("credential not in isolated native namespace/profile")
			}
			l1AssertNoFileCredentials(t, f.dir)
			client, err := f.manager.HTTPClient(context.Background(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := f.requests.Load()
			for _, unsafe := range []string{"https://attacker.invalid/mcp", "http://mcp.linear.app/mcp", "https://mcp.linear.app/token", "https://mcp.linear.app:443/mcp", linearconn.Endpoint(access) + "?untrusted=1"} {
				if r, err := client.Get(unsafe); err == nil {
					r.Body.Close()
					t.Errorf("unsafe credential destination allowed: %s", unsafe)
				}
			}
			if f.requests.Load() != before {
				t.Fatal("rejected credential destination reached injected transport")
			}
			response, err := client.Get(linearconn.Endpoint(access))
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if err := f.manager.Logout(context.Background(), p.ID); err != nil {
				t.Fatal(err)
			}
			if r, err := client.Get(linearconn.Endpoint(access)); err == nil {
				r.Body.Close()
				t.Fatal("existing HTTP client retained logged-out credential")
			}
			if len(f.store.snapshot()) != 0 {
				t.Fatal("logout retained selected synthetic credential")
			}
		})
	}
}

func TestL1NativeOAuthFailsBeforeCredentialCommitAndRedacts(t *testing.T) {
	for _, mode := range []string{"foreign-issuer", "foreign-token", "foreign-registration", "confidential-dcr", "wrong-state", "wrong-callback-issuer", "denial", "token-error", "verification", "secure-store"} {
		t.Run(mode, func(t *testing.T) {
			f := newL1OAuth(t, linearconn.Read, mode)
			if mode == "secure-store" {
				f.store.failSet = true
			}
			_, err := f.manager.Connect(context.Background(), f.options())
			if err == nil {
				t.Fatal("invalid connection succeeded")
			}
			for _, secret := range []string{l1SyntheticAccess, l1SyntheticRefresh, l1SyntheticClient, l1SyntheticCode, "synthetic-private-store-error"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("connection diagnostic echoed synthetic secret")
				}
			}
			if len(f.store.snapshot()) != 0 {
				t.Fatal("failed connection retained credential")
			}
			profiles, err := f.manager.Profiles()
			if err != nil || len(profiles) != 0 {
				t.Fatalf("failed connection published profile: count=%d err=%v", len(profiles), err)
			}
			if strings.HasPrefix(mode, "foreign-") && f.acquisitions.Load() != 0 {
				t.Fatal("untrusted discovery reached acquisition")
			}
			if (mode == "wrong-state" || mode == "wrong-callback-issuer") && f.exchanges.Load() != 0 {
				t.Fatal("invalid callback reached token exchange")
			}
			l1AssertNoFileCredentials(t, f.dir)
		})
	}
}

func TestL1NativeOAuthLoopbackRejectsWrongRequestAndCloses(t *testing.T) {
	f := newL1OAuth(t, linearconn.Read, "")
	var callback string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := linearconn.ConnectOptions{Name: "loopback fixture", Access: linearconn.Read, NoBrowser: true, PresentURL: func(raw string) error {
		redirect := f.inspectAuthorization(raw)
		callback = redirect.String()
		client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
		defer client.CloseIdleConnections()
		for _, mode := range []string{"path", "method", "state", "duplicate-state", "valid"} {
			u := *redirect
			q := u.Query()
			q.Set("state", f.state)
			q.Set("code", l1SyntheticCode)
			q.Set("iss", linearconn.Origin)
			method := http.MethodGet
			switch mode {
			case "path":
				u.Path += "-wrong"
			case "method":
				method = http.MethodPost
			case "state":
				q.Set("state", "wrong")
			case "duplicate-state":
				q.Add("state", f.state)
			}
			u.RawQuery = q.Encode()
			req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
			if err != nil {
				return err
			}
			r, err := client.Do(req)
			if err != nil {
				return err
			}
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			want := 400
			if mode == "valid" {
				want = 200
			}
			if r.StatusCode != want {
				t.Errorf("callback %s status=%d want%d", mode, r.StatusCode, want)
			}
			if strings.Contains(string(b), l1SyntheticCode) || strings.Contains(string(b), f.state) {
				t.Error("callback reflected code/state")
			}
		}
		return nil
	}}
	if _, err := f.manager.Connect(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if f.exchanges.Load() != 1 {
		t.Fatal("malformed callback consumed or duplicated exchange")
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	if r, err := client.Get(callback); err == nil {
		r.Body.Close()
		t.Fatal("callback listener retained after connection")
	}
	l1AssertNoFileCredentials(t, f.dir)
}

func TestL1NativeOAuthRefreshSerializedAndPersistenceFailClosed(t *testing.T) {
	for _, mode := range []string{"rotates-once", "save-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newL1OAuth(t, linearconn.Write, "")
			p, err := f.manager.Connect(context.Background(), f.options())
			if err != nil {
				t.Fatal(err)
			}
			client, err := f.manager.HTTPClient(context.Background(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := f.store.snapshot()
			f.now = f.now.Add(2 * time.Hour)
			if mode == "save-failure" {
				f.store.failSet = true
				calls := f.requests.Load()
				if r, err := client.Get(linearconn.Endpoint(linearconn.Write)); err == nil {
					r.Body.Close()
					t.Fatal("failed refresh persistence dispatched provider read")
				}
				if f.requests.Load() != calls+1 || !reflect.DeepEqual(before, f.store.snapshot()) {
					t.Fatal("failed secure save used new token or replaced previous credential")
				}
				return
			}
			var group sync.WaitGroup
			for n := 0; n < 6; n++ {
				group.Add(1)
				go func() {
					defer group.Done()
					r, err := client.Get(linearconn.Endpoint(linearconn.Write))
					if err != nil {
						t.Error(err)
						return
					}
					r.Body.Close()
				}()
			}
			group.Wait()
			if f.refreshes.Load() != 1 {
				t.Fatalf("refreshes=%d, want1", f.refreshes.Load())
			}
			v := f.store.snapshot()[linearconn.Service+"/"+p.ID]
			if !strings.Contains(v, l1SyntheticRefresh+"-rotated") {
				t.Fatal("rotated refresh credential not saved")
			}
			l1AssertNoFileCredentials(t, f.dir)
		})
	}
}
