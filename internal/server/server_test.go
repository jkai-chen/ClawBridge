package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

func TestParseAudiences(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "single", raw: "desktop.apps.googleusercontent.com", want: []string{"desktop.apps.googleusercontent.com"}},
		{name: "ordered set with surrounding whitespace", raw: " desktop.apps.googleusercontent.com, device.apps.googleusercontent.com ", want: []string{"desktop.apps.googleusercontent.com", "device.apps.googleusercontent.com"}},
		{name: "custom URL audience", raw: "https://clawbridge.example.test/audience", want: []string{"https://clawbridge.example.test/audience"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAudiences(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParseAudiencesRejectsInvalidSets(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "   "},
		{name: "empty entry", raw: "desktop,,device"},
		{name: "whitespace entry", raw: "desktop,   ,device"},
		{name: "duplicate after trimming", raw: "desktop, desktop"},
		{name: "interior whitespace", raw: "desktop client"},
		{name: "control character", raw: "desktop\nclient"},
		{name: "too long", raw: strings.Repeat("a", maxAudienceEntryLength+1)},
		{name: "too many", raw: strings.Repeat("a,", maxAudienceEntries) + "a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := parseAudiences(tc.raw); err == nil {
				t.Fatalf("invalid audience set accepted: %#v", got)
			}
		})
	}
}

func TestNewGoogleVerifierForAudiencesCopiesTrustedSet(t *testing.T) {
	input := []string{"desktop.apps.googleusercontent.com", "device.apps.googleusercontent.com"}
	verifier, err := NewGoogleVerifierForAudiences(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	input[0] = "attacker.example.test"
	want := []string{"desktop.apps.googleusercontent.com", "device.apps.googleusercontent.com"}
	if !reflect.DeepEqual(verifier.audiences, want) {
		t.Fatalf("trusted audience set mutated: got %#v, want %#v", verifier.audiences, want)
	}
}

func TestNewGoogleVerifierKeepsCommaAsSingleAudience(t *testing.T) {
	verifier, err := NewGoogleVerifier(context.Background(), "desktop,device")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(verifier.audiences, []string{"desktop,device"}) {
		t.Fatalf("single-audience constructor parsed a set: %#v", verifier.audiences)
	}
}

func TestServeRejectsInvalidAudienceSetBeforeDatabase(t *testing.T) {
	for _, raw := range []string{"", "desktop,,device", "desktop,desktop", "desktop client"} {
		err := Run(context.Background(), []string{"--audience", raw, "--bootstrap-owner", "owner@example.test"})
		if err == nil {
			t.Fatalf("invalid audience %q accepted", raw)
		}
		if strings.Contains(err.Error(), "Postgres") {
			t.Fatalf("invalid audience reached database setup: %v", err)
		}
	}
}

func TestServeAcceptsOrderedAudienceSetBeforeDatabase(t *testing.T) {
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "")
	err := Run(context.Background(), []string{
		"--audience", "desktop.apps.googleusercontent.com,device.apps.googleusercontent.com",
		"--bootstrap-owner", "owner@example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "Postgres connection") {
		t.Fatalf("valid audience set was not accepted before database setup: %v", err)
	}
}

func TestOAuthMetadataValidation(t *testing.T) {
	audiences := []string{"desktop.apps.googleusercontent.com", "device.apps.googleusercontent.com"}
	valid := OAuthMetadata{
		BrowserClientID: audiences[0], BrowserClientSecret: "synthetic-browser-client-credential",
		DeviceClientID: audiences[1], DeviceClientSecret: "synthetic-device-client-credential",
	}
	got, err := validateOAuthMetadata(audiences, valid)
	if err != nil || !reflect.DeepEqual(got, valid) {
		t.Fatalf("valid metadata rejected: %+v %v", got, err)
	}
	if got, err := validateOAuthMetadata(audiences, OAuthMetadata{}); err != nil || got != (OAuthMetadata{}) {
		t.Fatalf("omitted metadata did not retain legacy behavior: %+v %v", got, err)
	}
	tests := []struct {
		name     string
		metadata OAuthMetadata
	}{
		{name: "browser missing from audiences", metadata: OAuthMetadata{BrowserClientID: "other.apps.googleusercontent.com", BrowserClientSecret: "synthetic-browser-client-credential"}},
		{name: "device missing from audiences", metadata: OAuthMetadata{DeviceClientID: "other.apps.googleusercontent.com", DeviceClientSecret: "synthetic-device-client-credential"}},
		{name: "duplicate roles", metadata: OAuthMetadata{BrowserClientID: audiences[0], BrowserClientSecret: "synthetic-browser-client-credential", DeviceClientID: audiences[0], DeviceClientSecret: "synthetic-device-client-credential"}},
		{name: "interior whitespace", metadata: OAuthMetadata{BrowserClientID: "desktop client.apps.googleusercontent.com", BrowserClientSecret: "synthetic-browser-client-credential"}},
		{name: "browser secret without id", metadata: OAuthMetadata{BrowserClientSecret: "synthetic-browser-client-credential"}},
		{name: "device secret without id", metadata: OAuthMetadata{DeviceClientSecret: "synthetic-device-client-credential"}},
		{name: "missing browser secret", metadata: OAuthMetadata{BrowserClientID: audiences[0]}},
		{name: "missing device secret", metadata: OAuthMetadata{DeviceClientID: audiences[1]}},
		{name: "secret too long", metadata: OAuthMetadata{BrowserClientID: audiences[0], BrowserClientSecret: strings.Repeat("s", 4097)}},
		{name: "secret control character", metadata: OAuthMetadata{BrowserClientID: audiences[0], BrowserClientSecret: "synthetic\ncredential"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateOAuthMetadata(audiences, tc.metadata); err == nil {
				t.Fatal("invalid OAuth metadata accepted")
			} else {
				for _, forbidden := range []string{
					tc.metadata.BrowserClientSecret, tc.metadata.DeviceClientSecret,
				} {
					if forbidden != "" && strings.Contains(err.Error(), forbidden) {
						t.Fatal("OAuth metadata validation echoed installed-app credential material")
					}
				}
			}
		})
	}
}

func TestServeRejectsInvalidOAuthMetadataBeforeDatabase(t *testing.T) {
	err := Run(context.Background(), []string{
		"--audience", "desktop.apps.googleusercontent.com,device.apps.googleusercontent.com",
		"--oauth-client-id", "unlisted.apps.googleusercontent.com",
		"--bootstrap-owner", "owner@example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "must be present in --audience") {
		t.Fatalf("invalid OAuth client metadata accepted: %v", err)
	}
	if strings.Contains(err.Error(), "Postgres") {
		t.Fatalf("invalid OAuth metadata reached database setup: %v", err)
	}
}

type publicConfigExecutor struct{}

func (publicConfigExecutor) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New("unauthenticated discovery reached executor")
}

func TestPublicOAuthConfigIsUnauthenticatedAndMinimal(t *testing.T) {
	metadata := OAuthMetadata{
		BrowserClientID: "desktop.apps.googleusercontent.com", BrowserClientSecret: "synthetic-browser-client-credential",
		DeviceClientID: "device.apps.googleusercontent.com", DeviceClientSecret: "synthetic-device-client-credential",
	}
	verifier := VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{}, errors.New("public config called verifier")
	})
	handler := NewHandlerWithOAuthMetadata(publicConfigExecutor{}, verifier, metadata)
	request := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	request.Header.Set("X-Principal", "attacker@example.test")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		BrowserClientID     string `json:"browser_client_id"`
		BrowserClientSecret string `json:"browser_client_secret"`
		DeviceClientID      string `json:"device_client_id"`
		DeviceClientSecret  string `json:"device_client_secret"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.BrowserClientID != metadata.BrowserClientID || response.BrowserClientSecret != metadata.BrowserClientSecret ||
		response.DeviceClientID != metadata.DeviceClientID || response.DeviceClientSecret != metadata.DeviceClientSecret {
		t.Fatalf("config=%+v", response)
	}
	for _, forbidden := range []string{"token", "principal", "workspace", "project", "database", "postgres", "status", "health"} {
		if strings.Contains(strings.ToLower(recorder.Body.String()), forbidden) {
			t.Errorf("public config exposed or mentioned %q: %s", forbidden, recorder.Body.String())
		}
	}
	var exact map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &exact); err != nil {
		t.Fatal(err)
	}
	if len(exact) != 4 {
		t.Fatalf("public config does not contain the exact installed-app bundle: %#v", exact)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("public config is cacheable")
	}
}

func TestPublicOAuthConfigOmissionAndMethod(t *testing.T) {
	handler := NewHandler(publicConfigExecutor{}, VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{}, errors.New("unexpected verification")
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `{"browser_client_id":"","browser_client_secret":"","device_client_id":"","device_client_secret":""}` {
		t.Fatalf("legacy config response status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/config", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/config status=%d", recorder.Code)
	}
}

func TestServeRejectsOAuthClientIDOverServerFlagLimit(t *testing.T) {
	clientID := strings.Repeat("a", maxAudienceEntryLength+1)
	if _, err := validateOAuthMetadata([]string{clientID}, OAuthMetadata{BrowserClientID: clientID}); err == nil || !strings.Contains(err.Error(), "client ID is too long") {
		t.Fatalf("oversized OAuth metadata accepted: %v", err)
	}
}

func TestServeRejectsOAuthMetadataBeforeDatabase(t *testing.T) {
	err := Run(context.Background(), []string{
		"--audience", "desktop.apps.googleusercontent.com",
		"--oauth-client-id", "unlisted.apps.googleusercontent.com",
		"--bootstrap-owner", "owner@example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "must be present in --audience") {
		t.Fatalf("unlisted OAuth metadata accepted: %v", err)
	}
}

func TestServeOAuthMetadataPrecedesDatabase(t *testing.T) {
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "")
	t.Setenv("CLAWBRIDGE_OAUTH_CLIENT_SECRET", "synthetic-browser-client-credential")
	t.Setenv("CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET", "synthetic-device-client-credential")
	err := Run(context.Background(), []string{
		"--audience", "desktop.apps.googleusercontent.com,device.apps.googleusercontent.com",
		"--oauth-client-id", "desktop.apps.googleusercontent.com",
		"--oauth-device-client-id", "device.apps.googleusercontent.com",
		"--bootstrap-owner", "owner@example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "Postgres connection") {
		t.Fatalf("valid OAuth metadata did not reach database boundary: %v", err)
	}
}

func TestServeRequiresInstalledAppCredentialsFromEnvironmentOnly(t *testing.T) {
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "")
	t.Setenv("CLAWBRIDGE_OAUTH_CLIENT_SECRET", "")
	t.Setenv("CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET", "")
	err := Run(context.Background(), []string{
		"--audience", "desktop.apps.googleusercontent.com",
		"--oauth-client-id", "desktop.apps.googleusercontent.com",
		"--bootstrap-owner", "owner@example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "CLAWBRIDGE_OAUTH_CLIENT_SECRET") {
		t.Fatalf("configured browser ID without an environment credential did not fail closed: %v", err)
	}
	if strings.Contains(err.Error(), "Postgres") {
		t.Fatalf("missing browser credential reached database setup: %v", err)
	}
	err = Run(context.Background(), []string{
		"--audience", "device.apps.googleusercontent.com",
		"--oauth-device-client-id", "device.apps.googleusercontent.com",
		"--bootstrap-owner", "owner@example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET") {
		t.Fatalf("configured device ID without an environment credential did not fail closed: %v", err)
	}

	err = Run(context.Background(), []string{
		"--audience", "device.apps.googleusercontent.com",
		"--oauth-client-secret", "synthetic-must-not-be-an-argv-credential",
		"--bootstrap-owner", "owner@example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("secret-valued serve flag was accepted: %v", err)
	}
	if strings.Contains(err.Error(), "synthetic-must-not-be-an-argv-credential") {
		t.Fatal("rejected secret-valued serve flag was echoed")
	}
}
