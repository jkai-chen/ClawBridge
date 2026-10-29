package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/buildinfo"
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
	t.Setenv("CLAWBRIDGE_OAUTH_CLIENT_SECRET", "synthetic-browser")
	t.Setenv("CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET", "synthetic-device")
	err := Run(context.Background(), []string{
		"--audience", "desktop.apps.googleusercontent.com,device.apps.googleusercontent.com",
		"--oauth-client-id", "desktop.apps.googleusercontent.com",
		"--oauth-device-client-id", "device.apps.googleusercontent.com",
		"--bootstrap-owner", "owner@example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "Postgres connection") {
		t.Fatalf("valid audience set was not accepted before database setup: %v", err)
	}
}

// Wherever Google sign-in is on, explicitly or by its audience, all four
// OAuth settings are required: a Google deployment missing one fails at
// startup rather than serving devices only.
func TestServeGoogleNeedsEveryOAuthSetting(t *testing.T) {
	env := map[string]string{"CLAWBRIDGE_OAUTH_CLIENT_SECRET": "synthetic-browser", "CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET": "synthetic-device"}
	full := []string{"--audience", "desktop.apps.googleusercontent.com,device.apps.googleusercontent.com", "--oauth-client-id", "desktop.apps.googleusercontent.com", "--oauth-device-client-id", "device.apps.googleusercontent.com", "--bootstrap-owner", "owner@example.test"}
	origin := []string{"--public-origin", "https://cb.example.com"}
	for name, c := range map[string]struct {
		args    []string
		drop    string
		want    string
		methods string
	}{
		"inferred google":         {full, "", "", "[google]"},
		"audience and origin":     {append(append([]string{}, full...), origin...), "", "", "[google device]"},
		"explicit device":         {append([]string{"--auth", "device", "--bootstrap-owner", "owner@example.test"}, origin...), "", "", "[device]"},
		"device without origin":   {[]string{"--auth", "device", "--bootstrap-owner", "owner@example.test"}, "", "--public-origin", ""},
		"no browser client":       {full, "--oauth-client-id", "browser client credential has a secret but no client ID", ""},
		"no device client":        {full, "--oauth-device-client-id", "device client credential has a secret but no client ID", ""},
		"no browser secret":       {full, "CLAWBRIDGE_OAUTH_CLIENT_SECRET", "CLAWBRIDGE_OAUTH_CLIENT_SECRET", ""},
		"no device secret":        {full, "CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET", "CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET", ""},
		"explicit google, no aud": {[]string{"--auth", "google", "--bootstrap-owner", "owner@example.test"}, "", "--audience", ""},
	} {
		args := []string{}
		for i := 0; i < len(c.args); i++ {
			if c.args[i] == c.drop {
				i++
				continue
			}
			args = append(args, c.args[i])
		}
		settings, err := ParseServe(args, func(k string) string {
			if k == c.drop {
				return ""
			}
			return env[k]
		})
		if c.want == "" {
			if err != nil || fmt.Sprint(settings.Auth.Methods) != c.methods {
				t.Errorf("%s: %v %v", name, settings.Auth.Methods, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error naming %s, got %v", name, c.want, err)
		}
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
	// Exactly the bundle: a Google-only server names no sign-in methods, so
	// a v0.3.3 client's parser (exactly these four) accepts it.
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

// The onboarding check reads the release from /healthz without signing in.
func TestHealthzReportsVersion(t *testing.T) {
	verifier := VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{}, errors.New("healthz called verifier")
	})
	recorder := httptest.NewRecorder()
	NewHandlerWithOAuthMetadata(publicConfigExecutor{}, verifier, OAuthMetadata{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !reflect.DeepEqual(body, map[string]string{"status": "ok", "version": buildinfo.Version}) {
		t.Fatalf("healthz = %v", body)
	}
}

// The Linear app's redirect page is static: whatever the query carries, the
// page is the same, it never echoes or logs it, and it needs no sign-in.
func TestLinearCallbackIsStatic(t *testing.T) {
	verifier := VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{}, errors.New("callback called verifier")
	})
	handler := NewHandlerWithOAuthMetadata(publicConfigExecutor{}, verifier, OAuthMetadata{})
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	var first string
	for _, target := range []string{"/linear/callback", "/linear/callback?code=synthetic-code-sentinel&state=synthetic-state-sentinel", "/linear/callback?error=access_denied&error_description=%3Cscript%3E"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		body := recorder.Body.String()
		if recorder.Code != http.StatusOK || !strings.Contains(body, "ClawBridge app installed") || recorder.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("%s: %d %q", target, recorder.Code, body)
		}
		for header, want := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY"} {
			if recorder.Header().Get(header) != want {
				t.Fatalf("%s: %s = %q", target, header, recorder.Header().Get(header))
			}
		}
		if !strings.HasPrefix(recorder.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatal("no content security policy")
		}
		if first == "" {
			first = body
		} else if body != first {
			t.Fatalf("%s: the page depends on the query", target)
		}
	}
	if strings.Contains(first, "sentinel") || strings.Contains(first, "access_denied") || strings.Contains(first, "<script") || logged.Len() != 0 {
		t.Fatalf("query reflected or logged: %q %q", first, logged.String())
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "/linear/callback?code=x", nil))
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %q", recorder.Code, recorder.Body.String())
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, "/linear/callback?code=x", strings.NewReader("code=x")))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: %d", method, recorder.Code)
		}
	}
}

// A4 L5: project flow is proven by the flag or the environment, off by
// default.
func TestServeProjectFlowProven(t *testing.T) {
	base := []string{"--bootstrap-owner", "owner@example.test", "--auth", "device", "--public-origin", "https://cb.example.test"}
	for name, c := range map[string]struct {
		flag bool
		env  string
		want bool
		err  bool
	}{
		"default": {}, "flag": {flag: true, want: true}, "env": {env: "1", want: true}, "env off": {env: "0"}, "env bad": {env: "yes", err: true},
	} {
		args := append([]string{}, base...)
		if c.flag {
			args = append(args, "--project-flow-proven")
		}
		settings, err := ParseServe(args, func(k string) string {
			if k == "CLAWBRIDGE_PROJECT_FLOW_PROVEN" {
				return c.env
			}
			return ""
		})
		if (err != nil) != c.err || err == nil && settings.ProjectFlowProven != c.want {
			t.Errorf("%s: %v %v", name, settings.ProjectFlowProven, err)
		}
	}
}

// A domain coordinator's project status set is proven by the flag or the
// environment, off by default.
func TestServeProjectStatusDomainProven(t *testing.T) {
	base := []string{"--bootstrap-owner", "owner@example.test", "--auth", "device", "--public-origin", "https://cb.example.test"}
	for name, c := range map[string]struct {
		flag bool
		env  string
		want bool
		err  bool
	}{
		"default": {}, "flag": {flag: true, want: true}, "env": {env: "1", want: true}, "env true": {env: "true", want: true}, "env off": {env: "0"}, "env bad": {env: "yes", err: true},
	} {
		args := append([]string{}, base...)
		if c.flag {
			args = append(args, "--project-status-domain-proven")
		}
		settings, err := ParseServe(args, func(k string) string {
			if k == "CLAWBRIDGE_PROJECT_STATUS_DOMAIN_PROVEN" {
				return c.env
			}
			return ""
		})
		if (err != nil) != c.err || err == nil && (settings.ProjectStatusDomainProven != c.want || settings.ProjectTeamAddProven || settings.ProjectFlowProven) {
			t.Errorf("%s: %v %v", name, settings.ProjectStatusDomainProven, err)
		}
	}
}

// FR10: a team add is proven by the flag or the environment, off by
// default, and independent of project flow.
func TestServeProjectTeamAddProven(t *testing.T) {
	base := []string{"--bootstrap-owner", "owner@example.test", "--auth", "device", "--public-origin", "https://cb.example.test"}
	for name, c := range map[string]struct {
		flag bool
		env  string
		want bool
		err  bool
	}{
		"default": {}, "flag": {flag: true, want: true}, "env": {env: "1", want: true}, "env true": {env: "true", want: true}, "env off": {env: "0"}, "env bad": {env: "yes", err: true},
	} {
		args := append([]string{}, base...)
		if c.flag {
			args = append(args, "--project-team-add-proven")
		}
		settings, err := ParseServe(args, func(k string) string {
			if k == "CLAWBRIDGE_PROJECT_TEAM_ADD_PROVEN" {
				return c.env
			}
			return ""
		})
		if (err != nil) != c.err || err == nil && (settings.ProjectTeamAddProven != c.want || settings.ProjectFlowProven) {
			t.Errorf("%s: %v %v", name, settings.ProjectTeamAddProven, err)
		}
	}
}

// FR13: a project plan is proven by the flag or the environment, off by
// default, and independent of the team add.
func TestServeProjectPlanProven(t *testing.T) {
	base := []string{"--bootstrap-owner", "owner@example.test", "--auth", "device", "--public-origin", "https://cb.example.test"}
	for name, c := range map[string]struct {
		flag bool
		env  string
		want bool
		err  bool
	}{
		"default": {}, "flag": {flag: true, want: true}, "env": {env: "1", want: true}, "env true": {env: "true", want: true}, "env off": {env: "0"}, "env bad": {env: "yes", err: true},
	} {
		args := append([]string{}, base...)
		if c.flag {
			args = append(args, "--project-plan-proven")
		}
		settings, err := ParseServe(args, func(k string) string {
			if k == "CLAWBRIDGE_PROJECT_PLAN_PROVEN" {
				return c.env
			}
			return ""
		})
		if (err != nil) != c.err || err == nil && (settings.ProjectPlanProven != c.want || settings.ProjectTeamAddProven) {
			t.Errorf("%s: %v %v", name, settings.ProjectPlanProven, err)
		}
	}
}
