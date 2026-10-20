package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/server"
)

func TestServeValidatesAudienceSetBeforePostgres(t *testing.T) {
	const (
		browserSecret = "synthetic-browser-public-app-credential-must-not-log"
		deviceSecret  = "synthetic-device-public-app-credential-must-not-log"
	)
	p := newProcessFixture(t)
	baseEnv := append([]string{}, p.env...)
	baseEnv = append(baseEnv,
		"CLAWBRIDGE_DATABASE_URL=postgres://review:not-a-real-password@127.0.0.1:1/review?sslmode=disable",
		"CLAWBRIDGE_OAUTH_CLIENT_SECRET="+browserSecret,
		"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET="+deviceSecret,
	)
	run := func(audience string, extra ...string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		args := []string{
			"serve",
			"--audience", audience,
			"--bootstrap-owner", "owner@example.test",
			"--listen", "127.0.0.1:0",
		}
		args = append(args, extra...)
		cmd := exec.CommandContext(ctx, p.binary, args...)
		cmd.Dir = p.dir
		cmd.Env = baseEnv
		return cmd.CombinedOutput()
	}

	invalid := []struct {
		name, audience string
	}{
		{name: "empty", audience: ""},
		{name: "empty-entry", audience: "desktop.apps.googleusercontent.com,,device.apps.googleusercontent.com"},
		{name: "duplicate-after-trim", audience: "desktop.apps.googleusercontent.com, desktop.apps.googleusercontent.com"},
		{name: "interior-whitespace", audience: "desktop client.apps.googleusercontent.com"},
		{name: "control-character", audience: "desktop.apps.googleusercontent.com\nclient"},
		{name: "too-long", audience: strings.Repeat("a", 2049)},
		{name: "too-many", audience: strings.Repeat("a,", 16) + "a"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			output, err := run(tc.audience)
			if err == nil {
				t.Fatalf("invalid audience set unexpectedly started: %s", output)
			}
			diagnostic := string(output)
			if !strings.Contains(diagnostic, "serve requires --audience and --bootstrap-owner") {
				t.Fatalf("invalid set did not fail at audience validation: %s", diagnostic)
			}
			for _, databaseDiagnostic := range []string{"Postgres", "postgres://", "not-a-real-password"} {
				if strings.Contains(diagnostic, databaseDiagnostic) {
					t.Fatalf("invalid set reached database setup (%q): %s", databaseDiagnostic, diagnostic)
				}
			}
			if strings.Contains(diagnostic, tc.audience) && tc.audience != "" {
				t.Fatalf("invalid audience value was reflected: %s", diagnostic)
			}
		})
	}

	validOutput, validErr := run(
		"desktop.apps.googleusercontent.com,device.apps.googleusercontent.com",
		"--oauth-client-id", "desktop.apps.googleusercontent.com",
		"--oauth-device-client-id", "device.apps.googleusercontent.com",
	)
	if validErr == nil {
		t.Fatalf("unreachable test database unexpectedly started server: %s", validOutput)
	}
	if !strings.Contains(string(validOutput), "cannot initialize Postgres") {
		t.Fatalf("valid set did not reach the Postgres boundary: %s", validOutput)
	}
	for _, secret := range []string{"postgres://", "not-a-real-password"} {
		if bytes.Contains(validOutput, []byte(secret)) {
			t.Fatalf("database credential was reflected in serve diagnostics: %s", validOutput)
		}
	}

	configPath := filepath.Join(p.dir, "must-not-be-read.toml")
	if err := os.WriteFile(configPath, []byte("not = [valid"), 0600); err != nil {
		t.Fatal(err)
	}
	baseEnv = append(baseEnv, "CLAWBRIDGE_CONFIG="+configPath)
	output, err := run(
		"desktop.apps.googleusercontent.com,device.apps.googleusercontent.com",
		"--oauth-client-id", "desktop.apps.googleusercontent.com",
		"--oauth-device-client-id", "device.apps.googleusercontent.com",
	)
	if err == nil || !strings.Contains(string(output), "cannot initialize Postgres") {
		t.Fatalf("serve read local configuration before its PostgreSQL boundary: %s", output)
	}

	for _, secret := range []string{browserSecret, deviceSecret} {
		if strings.Contains(string(validOutput)+string(output), secret) {
			t.Fatalf("serve diagnostics leaked installed-app credential %q", secret)
		}
	}
	argvSecret := "must-not-enter-process-argv"
	output, err = run(
		"desktop.apps.googleusercontent.com,device.apps.googleusercontent.com",
		"--oauth-client-id", "desktop.apps.googleusercontent.com",
		"--oauth-device-client-id", "device.apps.googleusercontent.com",
		"--oauth-client-secret", argvSecret,
	)
	if err == nil {
		t.Fatalf("serve accepted an OAuth credential in process argv: %s", output)
	}
	if strings.Contains(string(output), argvSecret) {
		t.Fatalf("serve echoed rejected argv credential: %s", output)
	}
}

func TestPublicOAuthConfigContract(t *testing.T) {
	const (
		browserClient       = "browser-client.apps.googleusercontent.com"
		browserClientSecret = "synthetic-browser-public-app-credential"
		deviceClient        = "device-client.apps.googleusercontent.com"
		deviceClientSecret  = "synthetic-device-public-app-credential"
	)

	type publicConfig struct {
		BrowserClientID     string `json:"browser_client_id"`
		BrowserClientSecret string `json:"browser_client_secret"`
		DeviceClientID      string `json:"device_client_id"`
		DeviceClientSecret  string `json:"device_client_secret"`
	}
	var verifierCalls, executorCalls int
	configHandler := server.NewHandlerWithOAuthMetadata(executorFunc(func(context.Context, string, bus.Request) (bus.Result, error) {
		executorCalls++
		return bus.Result{}, errors.New("public config must not execute an operation")
	}), server.VerifierFunc(func(context.Context, string) (server.Identity, error) {
		verifierCalls++
		return server.Identity{}, nil
	}), server.OAuthMetadata{
		BrowserClientID:     browserClient,
		BrowserClientSecret: browserClientSecret,
		DeviceClientID:      deviceClient,
		DeviceClientSecret:  deviceClientSecret,
	})
	configServer := httptest.NewServer(configHandler)
	t.Cleanup(configServer.Close)

	response, err := http.Get(configServer.URL + "/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public config status=%d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("public config content type=%q", contentType)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("public config cache control=%q", response.Header.Get("Cache-Control"))
	}
	var got publicConfig
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("public config is not a strict four-field object: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatal("public config has trailing JSON")
	}
	if got.BrowserClientID != browserClient || got.BrowserClientSecret != browserClientSecret ||
		got.DeviceClientID != deviceClient || got.DeviceClientSecret != deviceClientSecret {
		t.Fatal("public config credential fields were missing, reordered or cross-paired")
	}
	if len(body) > 1024 {
		t.Fatalf("public config unexpectedly large: %d bytes", len(body))
	}
	if verifierCalls != 0 || executorCalls != 0 {
		t.Fatalf("public config invoked verifier=%d executor=%d", verifierCalls, executorCalls)
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		request, err := http.NewRequest(method, configServer.URL+"/v1/config", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s public config status=%d, want 405", method, response.StatusCode)
		}
	}
	if verifierCalls != 0 || executorCalls != 0 {
		t.Fatalf("rejected public config methods invoked verifier=%d executor=%d", verifierCalls, executorCalls)
	}
}

type executorFunc func(context.Context, string, bus.Request) (bus.Result, error)

func (f executorFunc) ExecuteAs(ctx context.Context, principal string, request bus.Request) (bus.Result, error) {
	return f(ctx, principal, request)
}
