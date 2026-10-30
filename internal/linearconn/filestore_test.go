package linearconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// oauthProvider answers discovery, registration, the code exchange (an
// access token that expires at once) and its refresh.
func oauthProvider(t *testing.T) (http.RoundTripper, *int) {
	t.Helper()
	var mu sync.Mutex
	refreshes := 0
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource":
			return response(`{"resource":"https://mcp.linear.app/mcp","authorization_servers":["https://mcp.linear.app"]}`), nil
		case "/.well-known/oauth-authorization-server":
			return response(`{"issuer":"https://mcp.linear.app","authorization_endpoint":"https://mcp.linear.app/authorize","token_endpoint":"https://mcp.linear.app/token","registration_endpoint":"https://mcp.linear.app/register","code_challenge_methods_supported":["S256"],"token_endpoint_auth_methods_supported":["none"],"grant_types_supported":["authorization_code","refresh_token"]}`), nil
		case "/register":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			b, _ := json.Marshal(map[string]any{"client_id": "client-id", "redirect_uris": in["redirect_uris"], "token_endpoint_auth_method": "none"})
			return response(string(b)), nil
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "authorization_code" {
				return response(`{"access_token":"initial-secret","refresh_token":"refresh-old","token_type":"Bearer","expires_in":1}`), nil
			}
			mu.Lock()
			refreshes++
			mu.Unlock()
			return response(`{"access_token":"rotated-secret","refresh_token":"refresh-new","token_type":"Bearer","expires_in":3600}`), nil
		case "/mcp", "/mcp/readonly":
			return response(`{}`), nil
		}
		return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
	}), &refreshes
}

func fetchCode(ctx context.Context, a *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	u, e := url.Parse(a.URL)
	if e != nil {
		return nil, e
	}
	return &auth.AuthorizationResult{Code: "code", State: u.Query().Get("state"), Iss: Origin}, nil
}

func connectWith(m *Manager, store string) (Profile, error) {
	return m.Connect(context.Background(), ConnectOptions{Name: "work", Access: Write, KeyStore: store, RedirectURL: "http://127.0.0.1:44999/callback", FetchCode: fetchCode})
}

// noSecret fails when any credential value shows in s.
func noSecret(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{"initial-secret", "rotated-secret", "refresh-old", "refresh-new", "token-one"} {
		if strings.Contains(s, secret) {
			t.Fatalf("a secret leaked into output: %q", s)
		}
	}
}

func connectionFile(m *Manager, id string) string {
	return filepath.Join(m.config.Directory, "connections", id+".json")
}

func TestLinearFileStoreConnectRefreshAndLogout(t *testing.T) {
	rt, refreshes := oauthProvider(t)
	m := testManager(t, rt)
	keyring := m.config.Store.(*memoryStore)
	p, e := connectWith(m, StoreFile)
	if e != nil {
		t.Fatal(e)
	}
	if p.KeyStore != StoreFile || p.Store() != StoreFile {
		t.Fatalf("profile store = %q", p.KeyStore)
	}
	path := connectionFile(m, p.ID)
	st, e := os.Stat(path)
	if e != nil {
		t.Fatal("no connection file", e)
	}
	// POSIX modes; Windows' ACLs are checked by the platform privacy tests.
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("connection file mode %v", st.Mode().Perm())
	}
	if d, e := os.Stat(filepath.Dir(path)); e != nil || runtime.GOOS != "windows" && d.Mode().Perm()&0o077 != 0 {
		t.Fatal("connection directory not private", e)
	}
	if len(keyring.values) != 0 {
		t.Fatal("the keyring was written for a file connection")
	}
	b, _ := os.ReadFile(filepath.Join(m.config.Directory, "profiles.json"))
	noSecret(t, string(b))
	mainSchema(t, b)
	record := filepath.Join(m.config.Directory, "connections", p.ID+".store")
	if r, e := os.ReadFile(record); e != nil || strings.TrimSpace(string(r)) != StoreFile {
		t.Fatalf("the store is not recorded beside the file: %q %v", r, e)
	}
	if st, e := os.Stat(record); e != nil || runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		t.Fatal("the store record is not private", e)
	}
	// The access token expires at once: the first request refreshes it,
	// and the rotation goes back to the file.
	client, e := m.HTTPClient(context.Background(), p.ID)
	if e != nil {
		t.Fatal(e)
	}
	resp, e := client.Post(Endpoint(Write), "application/json", strings.NewReader(`{}`))
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if *refreshes != 1 {
		t.Fatalf("refreshes = %d", *refreshes)
	}
	b, e = os.ReadFile(path)
	if e != nil || !strings.Contains(string(b), "refresh-new") {
		t.Fatal("the refresh was not written to the file", e)
	}
	if st, e = os.Stat(path); e != nil || runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		t.Fatal("the refresh widened the file's mode", e)
	}
	if len(keyring.values) != 0 {
		t.Fatal("the refresh wrote the keyring")
	}
	if e = m.Logout(context.Background(), p.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("logout kept the connection file", e)
	}
	if _, e = os.Stat(record); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("logout kept the store record", e)
	}
	if ps, e := m.Profiles(); e != nil || len(ps) != 0 {
		t.Fatal("logout kept the profile and its store record", ps, e)
	}
}

func TestLinearKeyringIsTheDefaultAndOldProfilesKeepIt(t *testing.T) {
	rt, _ := oauthProvider(t)
	m := testManager(t, rt)
	p, e := connectWith(m, "")
	if e != nil {
		t.Fatal(e)
	}
	if p.Store() != StoreKeyring {
		t.Fatalf("default store = %q", p.Store())
	}
	if _, e = os.Stat(connectionFile(m, p.ID)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("a keyring connection wrote a file", e)
	}
	b, _ := os.ReadFile(filepath.Join(m.config.Directory, "profiles.json"))
	// Nothing new on disk for a keyring profile: one written before the
	// store was recorded reads the same.
	mainSchema(t, b)
	if _, e = os.Stat(filepath.Join(m.config.Directory, "connections")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("a keyring profile wrote a store record", e)
	}
	if _, e = m.Status(context.Background(), p.ID); e != nil {
		t.Fatal("a profile without a record lost its keyring credential", e)
	}
	// Reconnecting the profile without --key-store keeps its store.
	f, e := connectWith(m, StoreFile)
	if e != nil {
		t.Fatal(e)
	}
	again, e := m.Connect(context.Background(), ConnectOptions{Name: "work", ProfileID: f.ID, Access: Write, RedirectURL: "http://127.0.0.1:44999/callback", FetchCode: fetchCode})
	if e != nil || again.Store() != StoreFile {
		t.Fatal("reconnecting moved the profile off its file store", again.KeyStore, e)
	}
}

func TestLinearReconnectMovesTheCredentialBetweenStores(t *testing.T) {
	rt, _ := oauthProvider(t)
	m := testManager(t, rt)
	keyring := m.config.Store.(*memoryStore)
	p, e := connectWith(m, StoreKeyring)
	if e != nil {
		t.Fatal(e)
	}
	moved, e := m.Connect(context.Background(), ConnectOptions{Name: "work", ProfileID: p.ID, Access: Write, KeyStore: StoreFile, RedirectURL: "http://127.0.0.1:44999/callback", FetchCode: fetchCode})
	if e != nil || moved.Store() != StoreFile {
		t.Fatal(moved, e)
	}
	if len(keyring.values) != 0 {
		t.Fatal("the old keyring credential was kept")
	}
	if _, e = m.Status(context.Background(), p.ID); e != nil {
		t.Fatal(e)
	}
	// And back: the record goes with the file.
	back, e := m.Connect(context.Background(), ConnectOptions{Name: "work", ProfileID: p.ID, Access: Write, KeyStore: StoreKeyring, RedirectURL: "http://127.0.0.1:44999/callback", FetchCode: fetchCode})
	if e != nil || back.Store() != StoreKeyring || len(keyring.values) != 1 {
		t.Fatal(back, e)
	}
	for _, ext := range []string{".json", ".store"} {
		if _, e = os.Stat(filepath.Join(m.config.Directory, "connections", p.ID+ext)); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("moving to the keyring kept", ext, e)
		}
	}
	if _, e = m.Status(context.Background(), p.ID); e != nil {
		t.Fatal(e)
	}
}

// mainSchema decodes profiles.json into the profile index as main writes
// it: a rollback to an older binary must still read it.
func mainSchema(t *testing.T, b []byte) {
	t.Helper()
	var x struct {
		Version  int `json:"version"`
		Profiles []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			Access         string `json:"access"`
			AccountLabel   string `json:"account_label,omitempty"`
			WorkspaceID    string `json:"workspace_id,omitempty"`
			WorkspaceLabel string `json:"workspace_label,omitempty"`
		} `json:"profiles"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&x); e != nil {
		t.Fatalf("profiles.json is not main's schema: %v: %s", e, b)
	}
}

// A profile recorded as file whose file is gone is an error naming the
// path: the keyring is never read instead.
func TestLinearRecordedFileNeverFallsBackToTheKeyring(t *testing.T) {
	rt, _ := oauthProvider(t)
	m := testManager(t, rt)
	p, e := connectWith(m, StoreFile)
	if e != nil {
		t.Fatal(e)
	}
	m.config.Store.(*memoryStore).values[Service+p.ID] = `{"version":1,"token":{"access_token":"token-one","token_type":"Bearer"},"imported":true}`
	path := connectionFile(m, p.ID)
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	_, e = m.Status(context.Background(), p.ID)
	if e == nil || !strings.Contains(e.Error(), path) || !strings.Contains(e.Error(), "missing") || !strings.Contains(e.Error(), "clawbridge linear logout --profile "+p.ID+", then connect again") {
		t.Fatal("a missing file fell back or wasn't named", e)
	}
	noSecret(t, e.Error())
	if _, e = m.HTTPClient(context.Background(), p.ID); e == nil {
		t.Fatal("a client was made from the keyring")
	}
	// An unreadable record is an error too, never "no record".
	record := filepath.Join(m.config.Directory, "connections", p.ID+".store")
	if e = os.WriteFile(record, []byte("vault\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Status(context.Background(), p.ID); e == nil || !strings.Contains(e.Error(), record) {
		t.Fatal(e)
	}
	// Logout still needs a readable record; with one, it removes both.
	if e = os.WriteFile(record, []byte("file\n"), 0o600); e != nil {
		t.Fatal(e)
	}
	if e = m.Logout(context.Background(), p.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(record); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("logout kept the record", e)
	}
}

func TestLinearMissingKeyringNamesTheFileStore(t *testing.T) {
	rt, _ := oauthProvider(t)
	m := testManager(t, rt)
	m.config.KeyringCheck = func(context.Context) error { return errors.New("no Secret Service on the session bus") }
	for _, store := range []string{"", StoreKeyring} {
		_, e := connectWith(m, store)
		if e == nil || !strings.Contains(e.Error(), "--key-store file") {
			t.Fatalf("store %q: %v", store, e)
		}
		noSecret(t, e.Error())
	}
	if _, e := os.Stat(filepath.Join(m.config.Directory, "connections")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("a file was written without --key-store file", e)
	}
	// A keyring that fails at the save names the way out too.
	m.config.KeyringCheck = nil
	m.config.Store.(*memoryStore).fail = true
	_, e := connectWith(m, "")
	if e == nil || !strings.Contains(e.Error(), "--key-store file") {
		t.Fatal(e)
	}
	noSecret(t, e.Error())
	if _, e := os.Stat(filepath.Join(m.config.Directory, "connections")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("a failed keyring save fell back to a file", e)
	}
	// The file store doesn't need the keyring.
	m.config.KeyringCheck = func(context.Context) error { return errors.New("no Secret Service") }
	if _, e = connectWith(m, StoreFile); e != nil {
		t.Fatal(e)
	}
}

func TestLinearImportToFileStore(t *testing.T) {
	m := testManager(t, roundTripFunc(func(*http.Request) (*http.Response, error) { return response(`{}`), nil }))
	p, e := m.ImportTo(context.Background(), "work", Read, StoreFile, strings.NewReader("token-one\n"))
	if e != nil {
		t.Fatal(e)
	}
	if p.Store() != StoreFile || len(m.config.Store.(*memoryStore).values) != 0 {
		t.Fatal("import ignored the file store")
	}
	if _, e = m.ImportTo(context.Background(), "work", Read, "vault", strings.NewReader("token-one\n")); e == nil {
		t.Fatal("an unknown store was accepted")
	}
}

func TestLinearFileStoreRefusesWindowsDrives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("WSL's drives are a Linux path")
	}
	rt, _ := oauthProvider(t)
	m := testManager(t, rt)
	old := windowsDrives
	t.Cleanup(func() { windowsDrives = old })
	resolved, e := filepath.EvalSymlinks(filepath.Dir(m.config.Directory))
	if e != nil {
		t.Fatal(e)
	}
	windowsDrives = resolved + string(filepath.Separator)
	_, e = connectWith(m, StoreFile)
	if e == nil || !strings.Contains(e.Error(), "under "+windowsDrives) || !strings.Contains(e.Error(), "can't hold private files") || !strings.Contains(e.Error(), "Linux home") {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(m.config.Directory, "connections")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("a connection directory was made on a Windows drive", e)
	}
	if !onWindowsDrive(filepath.Join(resolved, "x")) || onWindowsDrive("/home/example/.clawbridge/linear") {
		t.Fatal("onWindowsDrive misjudged a path")
	}
}

func TestLinearKeyringWordingByOS(t *testing.T) {
	old := goos
	t.Cleanup(func() { goos = old })
	for os, want := range map[string]bool{"linux": true, "darwin": false, "windows": false} {
		goos = os
		got := strings.Contains(keyringDetail(nil), "Secret Service")
		if got != want || !strings.Contains(keyringDetail(nil), "--key-store file") {
			t.Fatalf("%s: %s", os, keyringDetail(nil))
		}
	}
}
