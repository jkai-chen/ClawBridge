package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/mirror"
	"golang.org/x/oauth2"
)

func TestScopedSyncOutageAndDefinitiveDenials(t *testing.T) {
	ctx := context.Background()
	var status atomic.Int64
	var foreign atomic.Bool
	principal := "alice@example.test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("missing credential")
		}
		if code := status.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		var request bus.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		data := map[string]any{}
		switch request.Operation {
		case "resolve_project":
			data = map[string]any{"workspace_id": "ws_1", "project_id": "prj_1"}
		case "snapshot":
			owner := principal
			if foreign.Load() {
				owner = "other@example.test"
			}
			data = map[string]any{"workspace": map[string]any{"id": "ws_1"}, "project": map[string]any{"id": "prj_1"}, "session": map[string]any{"id": "ses_1", "name": "worker", "instance": "instance-1"}, "principal": owner, "verified": true, "snapshot_at": time.Now().UTC().Format(time.RFC3339Nano), "sessions": []any{map[string]any{"sequence": 1, "id": "ses_1", "name": "worker", "status": "active"}}, "messages": []any{map[string]any{"sequence": 1, "id": "msg_1", "body": "cached private message", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "delivered_at": nil, "read_at": nil}}, "sent_messages": []any{}, "events": []any{}}
		default:
			t.Errorf("unexpected online operation: %s", request.Operation)
		}
		_ = json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: data})
	}))
	defer server.Close()
	config := Config{URL: server.URL, Audience: "audience", Principal: principal, CachePath: filepath.Join(t.TempDir(), "cache.db")}
	tokens := TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil })
	client, err := NewWithTokenSource(config, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Sync(ctx, "team", "app", "worker", "instance-1"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	foreign.Store(true)
	if _, err = client.Sync(ctx, "team", "app", "worker", "instance-1"); err == nil {
		t.Fatal("accepted foreign-principal snapshot")
	}
	after, _ := os.ReadFile(config.CachePath)
	if !bytes.Equal(before, after) {
		t.Fatal("failed sync altered mirror")
	}
	status.Store(503)
	wid, pid, err := client.ResolveProject(ctx, "team", "app", "")
	if err != nil || wid != "ws_1" || pid != "prj_1" {
		t.Fatalf("cached resolution: %q %q %v", wid, pid, err)
	}
	request := bus.Request{Operation: "inbox", Workspace: wid, Project: pid, Session: "worker", Args: map[string]any{"instance": "instance-1"}}
	result, err := client.Execute(ctx, request)
	if err != nil || result.Source != "offline-mirror" {
		t.Fatalf("offline read: %+v %v", result, err)
	}
	encoded, _ := json.Marshal(result)
	if !bytes.Contains(encoded, []byte("cached private message")) || !bytes.Contains(encoded, []byte(`"delivered_at":null`)) {
		t.Fatal("cached inbox changed content or stamped delivery")
	}
	for _, op := range []string{"send", "reply", "mark_read", "register_session", "retire_session", "project_role_set"} {
		request.Operation = op
		if _, err = client.Execute(ctx, request); err == nil || !strings.Contains(err.Error(), "no write queued") {
			t.Fatalf("offline %s was queued/succeeded: %v", op, err)
		}
	}
	// The project overview is read from the authority only: offline it says
	// so, and never reads as a failed write.
	overview := bus.Request{Operation: "project_overview", Workspace: wid, Project: pid, Args: map[string]any{}}
	if _, err = client.Execute(ctx, overview); !errors.Is(err, ErrOverviewOffline) {
		t.Fatalf("offline overview: %v", err)
	}
	// The project's threads are read from the authority only: offline it
	// says so, and never reads as a failed write.
	for _, op := range []string{"project_threads", "project_thread"} {
		threads := bus.Request{Operation: op, Workspace: wid, Project: pid, Args: map[string]any{}}
		if _, err = client.Execute(ctx, threads); !errors.Is(err, ErrThreadsOffline) {
			t.Fatalf("offline %s: %v", op, err)
		}
	}
	request.Operation = "inbox"
	for _, denial := range []int64{400, 401, 403, 404, 409, 429} {
		status.Store(denial)
		if _, err = client.Execute(ctx, request); err == nil {
			t.Fatalf("HTTP %d fell back to mirror", denial)
		}
	}
	status.Store(503)
	request.Args["instance"] = "different-instance"
	if _, err = client.Execute(ctx, request); err == nil {
		t.Fatal("mirror crossed instance")
	}
	request.Args["instance"] = "instance-1"
	request.Project = "different-project"
	if _, err = client.Execute(ctx, request); err == nil {
		t.Fatal("mirror crossed project")
	}
	after, _ = os.ReadFile(config.CachePath)
	if !bytes.Equal(before, after) {
		t.Fatal("offline reads/writes changed mirror bytes")
	}
}

func TestURLAndRedirectCredentialBoundary(t *testing.T) {
	tokens := TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil })
	for _, u := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com?token=value", "https://example.com#fragment", "file:///tmp/cache"} {
		if _, err := NewWithTokenSource(Config{URL: u, Audience: "aud"}, tokens); err == nil {
			t.Fatalf("accepted %s", u)
		}
	}
	var received atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	client, err := NewWithTokenSource(Config{URL: redirect.URL, Audience: "aud"}, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Execute(context.Background(), bus.Request{Operation: "whoami"}); err == nil {
		t.Fatal("redirect reported success")
	}
	if received.Load() {
		t.Fatal("authenticated redirect was followed")
	}
}

func TestCredentialSourceSelectionAndConstraints(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	credentialFile := filepath.Join(dir, "application_default_credentials.json")
	if err := os.WriteFile(credentialFile, []byte(`{
		"type": "authorized_user",
		"client_id": "test.apps.googleusercontent.com",
		"client_secret": "not-a-real-secret",
		"refresh_token": "not-a-real-refresh-token"
	}`), 0600); err != nil {
		t.Fatal(err)
	}
	base := Config{
		URL:                       "http://127.0.0.1:1",
		Audience:                  "https://bus.example.test",
		Principal:                 "service@example.test",
		CachePath:                 filepath.Join(dir, "cache.db"),
		Account:                   "human@example.test",
		ImpersonateServiceAccount: "agent@example.iam.gserviceaccount.com",
		CredentialsFile:           credentialFile,
	}

	for _, source := range []string{"", CredentialSourceGcloud} {
		config := base
		config.CredentialSource = source
		config.CredentialsFile = ""
		tokens, err := credentialTokenSource(ctx, config)
		if err != nil {
			t.Fatalf("source %q: %v", source, err)
		}
		if _, ok := tokens.(*GcloudTokenSource); !ok {
			t.Fatalf("source %q selected %T, want gcloud", source, tokens)
		}
	}

	config := base
	config.CredentialSource = CredentialSourceADC
	tokens, err := credentialTokenSource(ctx, config)
	if err != nil {
		t.Fatalf("ADC authorized-user construction: %v", err)
	}
	if _, ok := tokens.(*ADCTokenSource); !ok {
		t.Fatalf("ADC selected %T", tokens)
	}

	invalid := base
	invalid.CredentialSource = "metadata"
	if _, err = credentialTokenSource(ctx, invalid); err == nil || !strings.Contains(err.Error(), "oauth, gcloud, adc or device") {
		t.Fatalf("unknown source accepted: %v", err)
	}
	invalid = base
	invalid.CredentialSource = CredentialSourceGcloud
	if _, err = credentialTokenSource(ctx, invalid); err == nil || !strings.Contains(err.Error(), "cannot be combined with ADC or OAuth") {
		t.Fatalf("gcloud accepted credentials file: %v", err)
	}
	invalid = base
	invalid.CredentialSource = CredentialSourceGcloud
	invalid.CredentialsFile = ""
	invalid.OAuthDeviceClientSecret = "synthetic-device-client-credential"
	if _, err = credentialTokenSource(ctx, invalid); err == nil || !strings.Contains(err.Error(), "cannot be combined with ADC or OAuth") {
		t.Fatalf("gcloud accepted an inactive OAuth device credential: %v", err)
	}
}

func TestOAuthRouteRequiresAudienceToEqualActiveClient(t *testing.T) {
	base := Config{
		URL: "http://127.0.0.1:1", Principal: "alice@example.test",
		CachePath: filepath.Join(t.TempDir(), "cache.db"), CredentialSource: CredentialSourceOAuth,
		OAuthClientID: "active-client.apps.googleusercontent.com",
		Audience:      "other-client.apps.googleusercontent.com",
	}
	if _, err := ValidateRoute(base); err == nil || !strings.Contains(err.Error(), "audience must equal") {
		t.Fatalf("mismatched OAuth audience/client accepted: %v", err)
	}
	base.Audience = base.OAuthClientID
	if _, err := ValidateRoute(base); err != nil {
		t.Fatalf("matching OAuth audience/client rejected: %v", err)
	}
	base.OAuthClientSecret = "synthetic-browser-client-credential"
	base.OAuthDeviceClientSecret = "synthetic-device-client-credential"
	public := base.PublicRoute()
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"synthetic-browser-client-credential", "synthetic-device-client-credential"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("public route exposed an installed-app credential: %s", encoded)
		}
	}
}

func TestADCRejectsUnsafePathsAndCredentialTypes(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	directory := filepath.Join(dir, "credentials-dir")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	wrongType := filepath.Join(dir, "service-account.json")
	if err := os.WriteFile(wrongType, []byte(`{
		"type": "service_account",
		"client_email": "unused@example.test",
		"private_key": "not-a-real-key"
	}`), 0600); err != nil {
		t.Fatal(err)
	}

	base := Config{
		URL:                       "http://127.0.0.1:1",
		Audience:                  "https://bus.example.test",
		Account:                   "human@example.test",
		ImpersonateServiceAccount: "agent@example.iam.gserviceaccount.com",
		CredentialSource:          CredentialSourceADC,
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"relative", func(c *Config) { c.CredentialsFile = "credentials.json" }},
		{"missing", func(c *Config) { c.CredentialsFile = missing }},
		{"directory", func(c *Config) { c.CredentialsFile = directory }},
		{"missing-audience", func(c *Config) { c.CredentialsFile = wrongType; c.Audience = "" }},
		{"missing-account", func(c *Config) { c.CredentialsFile = wrongType; c.Account = "" }},
		{"missing-target", func(c *Config) { c.CredentialsFile = wrongType; c.ImpersonateServiceAccount = "" }},
		{"self-impersonation", func(c *Config) { c.CredentialsFile = wrongType; c.Account = c.ImpersonateServiceAccount }},
		{"wrong-type", func(c *Config) { c.CredentialsFile = wrongType }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := base
			tc.mutate(&config)
			_, err := New(config)
			if err == nil {
				t.Fatal("unsafe ADC configuration was accepted")
			}
			var credential *credentialError
			if !errors.As(err, &credential) {
				t.Fatalf("ADC setup failure lost credential classification: %v", err)
			}
			for _, secret := range []string{"not-a-real-key", "not-a-real-refresh-token", "not-a-real-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("ADC setup error exposed credential material")
				}
			}
		})
	}
}

type failingTokenSource struct{ err error }

func (f failingTokenSource) Token() (*oauth2.Token, error) { return nil, f.err }

func TestADCCredentialFailureDoesNotFallBackToMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	client, err := NewWithTokenSource(Config{
		URL:                       "http://127.0.0.1:1",
		Audience:                  "audience",
		Account:                   "human@example.test",
		ImpersonateServiceAccount: "agent@example.iam.gserviceaccount.com",
		CredentialsFile:           filepath.Join(t.TempDir(), "authorized-user.json"),
		Principal:                 "alice@example.test",
		CachePath:                 path,
		CredentialSource:          CredentialSourceADC,
	}, &ADCTokenSource{source: failingTokenSource{err: errors.New("synthetic ADC refresh failure")}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Execute(context.Background(), bus.Request{
		Operation: "inbox", Workspace: "w", Project: "p", Session: "worker",
		Args: map[string]any{"instance": "instance-1"},
	})
	if err == nil {
		t.Fatal("synthetic ADC failure unexpectedly succeeded")
	}
	var credential *credentialError
	if !errors.As(err, &credential) {
		t.Fatalf("ADC refresh failure was not classified as a credential error: %v", err)
	}
	if strings.Contains(err.Error(), "mirror") {
		t.Fatalf("ADC credential failure consulted the mirror: %v", err)
	}
	if strings.Contains(err.Error(), "synthetic ADC refresh failure") {
		t.Fatalf("ADC library error was exposed: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("ADC credential failure created or touched mirror: %v", statErr)
	}
}

func TestGcloudUsesExplicitAccountAudienceAndImpersonation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX test executable")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CLAWBRIDGE_TEST_CAPTURE\"\nprintf '%s' 'synthetic-token'\n"
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAWBRIDGE_TEST_CAPTURE", capture)
	source := &GcloudTokenSource{Account: "human@example.test", Audience: "https://bus.example.test", ImpersonateServiceAccount: "agent@example.iam.gserviceaccount.com"}
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(capture)
	for _, want := range []string{"--account\nhuman@example.test", "--audiences\nhttps://bus.example.test", "--impersonate-service-account\nagent@example.iam.gserviceaccount.com", "--include-email"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing explicit routing %s", want)
		}
	}
	if _, err := (&GcloudTokenSource{Account: "human@example.test", Audience: "aud"}).Token(context.Background()); err == nil {
		t.Fatal("generic development token path accepted")
	}
}

func TestCredentialFailureDoesNotFallBackToMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	tokens := TokenSourceFunc(func(context.Context) (string, error) {
		return "", &credentialError{errors.New("local credential unavailable")}
	})
	client, err := NewWithTokenSource(Config{
		URL:       "http://127.0.0.1:1",
		Audience:  "audience",
		Principal: "alice@example.test",
		CachePath: path,
	}, tokens)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Execute(context.Background(), bus.Request{Operation: "inbox", Workspace: "w", Project: "p", Session: "worker", Args: map[string]any{"instance": "instance-1"}})
	if err == nil || strings.Contains(err.Error(), "mirror") {
		t.Fatalf("credential failure attempted mirror fallback: %v", err)
	}
	var credential *credentialError
	if !errors.As(err, &credential) {
		t.Fatalf("credential error classification lost: %v", err)
	}
}

func TestPlainTokenSourceFailureCannotReadMatchingMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	config := Config{
		URL:       "http://127.0.0.1:1",
		Audience:  "audience",
		Principal: "alice@example.test",
		CachePath: path,
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err := mirror.Save(context.Background(), path, mirror.Snapshot{
		Binding: mirror.Binding{
			Server: config.URL, Audience: config.Audience, Principal: config.Principal,
			Workspace: "w", Project: "p", Session: "worker", Instance: "instance-1",
		},
		UpdatedAt: now,
		Data: map[string]any{
			"workspace": map[string]any{"id": "w"}, "project": map[string]any{"id": "p"},
			"session":   map[string]any{"id": "worker", "name": "worker", "instance": "instance-1"},
			"principal": config.Principal, "verified": true, "storage_mode": "cloud",
			"identity_provenance": "google-id-token", "snapshot_at": now,
			"sessions":      []any{map[string]any{"sequence": 1, "id": "worker", "name": "worker", "status": "active"}},
			"messages":      []any{map[string]any{"sequence": 1, "id": "msg_1", "body": "matching-cached-body", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "delivered_at": nil, "read_at": nil}},
			"sent_messages": []any{}, "events": []any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewWithTokenSource(config, TokenSourceFunc(func(context.Context) (string, error) {
		return "", errors.New("synthetic-plain-source-secret")
	}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Execute(context.Background(), bus.Request{
		Operation: "inbox", Workspace: "w", Project: "p", Session: "worker",
		Args: map[string]any{"instance": "instance-1"},
	})
	if err == nil {
		t.Fatalf("plain token-source failure returned %q from mirror", result.Source)
	}
	if strings.Contains(strings.ToLower(err.Error()), "mirror") || strings.Contains(err.Error(), "synthetic-plain-source-secret") {
		t.Fatalf("credential failure consulted or exposed mirror/source details: %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatal("plain token-source failure mutated the mirror")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	client, err = NewWithTokenSource(config, TokenSourceFunc(func(context.Context) (string, error) {
		return "", errors.New("must not replace the cancellation")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Execute(canceled, bus.Request{Operation: "inbox", Workspace: "w", Project: "p", Session: "worker", Args: map[string]any{"instance": "instance-1"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not preserved: %v", err)
	}
}

// R85-L1: a bearer route behind a path prefix keeps it, as before device
// signing changed how the request target is built.
func TestBearerRouteKeepsTheServerPath(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RequestURI
		_ = json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: map[string]any{}})
	}))
	defer server.Close()
	client, err := NewWithTokenSource(Config{URL: server.URL + "/bus", Audience: "audience", Principal: "alice@example.test"}, TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Execute(context.Background(), bus.Request{Operation: "list_projects", Workspace: "w"}); err != nil || got != "/bus/v1/execute" {
		t.Fatalf("sent %q: %v", got, err)
	}
}
