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

// v1.0 removed Google sign-in: a server signs in with device keys only, and
// a start that still names Google is refused with the runbook's text before
// anything else, never falling back.
func TestServeRefusesGoogleSignIn(t *testing.T) {
	origin := []string{"--public-origin", "https://cb.example.test", "--bootstrap-owner", "owner@example.test"}
	for _, auth := range []string{"google", "google,device", "device,google", "Google"} {
		_, err := ParseServe(append([]string{"--auth", auth}, origin...), func(string) string { return "" })
		var refused *StartRefused
		if !errors.As(err, &refused) || err.Error() != "clawbridge serve: Google sign-in was removed in v1.0; set CLAWBRIDGE_AUTH=device after every member has an active device (docs/operations/google-sign-in-removal.md#before-you-switch)" {
			t.Errorf("--auth %s: %v", auth, err)
		}
	}
	for _, args := range [][]string{{"--audience", "desktop.example.test"}, {"--audience=desktop.example.test"}, {"-audience", "x"}, {"--audience"}} {
		_, err := ParseServe(append(append([]string{"--auth", "device"}, args...), origin...), func(string) string { return "" })
		var refused *StartRefused
		if !errors.As(err, &refused) || err.Error() != "clawbridge serve: --audience was for Google sign-in, removed in v1.0 (docs/operations/google-sign-in-removal.md#before-you-switch)" {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, name := range []string{"oauth-client-id", "oauth-device-client-id"} {
		_, err := ParseServe(append([]string{"--" + name, "synthetic-client"}, origin...), func(string) string { return "" })
		if err == nil || err.Error() != "clawbridge serve: --"+name+" was for Google sign-in, removed in v1.0 (docs/operations/google-sign-in-removal.md#before-you-switch)" || strings.Contains(err.Error(), "synthetic-client") {
			t.Errorf("--%s: %v", name, err)
		}
	}
	// Run refuses before it reads the database.
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "")
	if err := Run(context.Background(), append([]string{"--auth", "google"}, origin...)); err == nil || err.Error() != GoogleAuthRefused {
		t.Fatalf("Run with google: %v", err)
	}
}

// Every spelling of google in --auth or CLAWBRIDGE_AUTH (comma, space, any
// case) refuses the start with the same text; CLAWBRIDGE_AUTH counts only
// without --auth.
func TestServeRefusesEveryGoogleAuthForm(t *testing.T) {
	origin := []string{"--public-origin", "https://cb.example.test", "--bootstrap-owner", "owner@example.test"}
	for _, form := range []string{"google", "GOOGLE", "Google", "google,device", "device,google", "google device", "device google", " google ", "google, device", "Device,GOOGLE", "google  device", "device\tgoogle"} {
		_, err := ParseServe(append([]string{"--auth", form}, origin...), func(string) string { return "" })
		var refused *StartRefused
		if !errors.As(err, &refused) || err.Error() != "clawbridge serve: Google sign-in was removed in v1.0; set CLAWBRIDGE_AUTH=device after every member has an active device (docs/operations/google-sign-in-removal.md#before-you-switch)" {
			t.Errorf("--auth %q: %v", form, err)
		}
		_, err = ParseServe(origin, func(k string) string {
			if k == "CLAWBRIDGE_AUTH" {
				return form
			}
			return ""
		})
		if !errors.As(err, &refused) || err.Error() != GoogleAuthRefused {
			t.Errorf("CLAWBRIDGE_AUTH=%q: %v", form, err)
		}
	}
	// --auth device wins over the environment; the environment alone counts.
	env := func(k string) string {
		if k == "CLAWBRIDGE_AUTH" {
			return "google"
		}
		return ""
	}
	if s, err := ParseServe(append([]string{"--auth", "device"}, origin...), env); err != nil || fmt.Sprint(s.Auth.Methods) != "[device]" {
		t.Fatal(s.Auth, err)
	}
	if s, err := ParseServe(origin, func(k string) string {
		if k == "CLAWBRIDGE_AUTH" {
			return "device"
		}
		return ""
	}); err != nil || fmt.Sprint(s.Auth.Methods) != "[device]" {
		t.Fatal(s.Auth, err)
	}
}

// A compose file from before v1.0 stays on the VM with the published image
// and always passes Google sign-in's flags, empty when unset: they are
// treated as absent, with no error and no warning. A value still refuses.
func TestServeAcceptsOldComposeEmptyGoogleFlags(t *testing.T) {
	// v0.3.13's deploy/compose.yaml, rendered with the settings unset and
	// CLAWBRIDGE_AUTH=device.
	old := []string{"--listen", "0.0.0.0:8080", "--auth", "device", "--public-origin", "https://clawbridge.example.test",
		"--audience", "", "--bootstrap-owner", "owner@example.test", "--oauth-client-id", "", "--oauth-device-client-id", ""}
	settings, err := ParseServe(old, func(string) string { return "" })
	if err != nil || fmt.Sprint(settings.Auth.Methods) != "[device]" || settings.Listen != "0.0.0.0:8080" || settings.BootstrapOwner != "owner@example.test" || settings.LeftoverGoogleSettings {
		t.Fatalf("the old compose command line did not start: %+v %v", settings, err)
	}
	for _, form := range [][]string{{"--audience="}, {"--audience", ""}, {"-oauth-client-id="}, {"--oauth-device-client-id", ""}} {
		if _, err := ParseServe(append(append([]string{}, form...), "--public-origin", "https://cb.example.test", "--bootstrap-owner", "owner@example.test"), func(string) string { return "" }); err != nil {
			t.Errorf("%q: %v", form, err)
		}
	}
	// With a value, each refuses with its exact line.
	for name, at := range map[string]int{"audience": 7, "oauth-client-id": 11, "oauth-device-client-id": 13} {
		args := append([]string{}, old...)
		args[at] = "desktop.example.test"
		_, err := ParseServe(args, func(string) string { return "" })
		var refused *StartRefused
		if !errors.As(err, &refused) || err.Error() != "clawbridge serve: --"+name+" was for Google sign-in, removed in v1.0 (docs/operations/google-sign-in-removal.md#before-you-switch)" {
			t.Errorf("%s with a value: %v (%q)", name, err, args)
		}
	}
	// Run gets past the flags to the database.
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "")
	if err := Run(context.Background(), old); err == nil || !strings.Contains(err.Error(), "Postgres connection") {
		t.Fatalf("Run with the old compose command line: %v", err)
	}
}

// device is the only method and the default; it needs the public origin.
func TestServeAuthIsDevice(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"default":        {[]string{"--public-origin", "https://cb.example.test"}, ""},
		"explicit":       {[]string{"--auth", "device", "--public-origin", "https://cb.example.test"}, ""},
		"no origin":      {[]string{"--auth", "device"}, "--public-origin"},
		"unknown method": {[]string{"--auth", "password", "--public-origin", "https://cb.example.test"}, "--auth must be device"},
		"twice":          {[]string{"--auth", "device,device", "--public-origin", "https://cb.example.test"}, "--auth must be device"},
	} {
		settings, err := ParseServe(append(c.args, "--bootstrap-owner", "owner@example.test"), func(string) string { return "" })
		if c.want == "" {
			if err != nil || fmt.Sprint(settings.Auth.Methods) != "[device]" || settings.Auth.PublicOrigin != "https://cb.example.test" {
				t.Errorf("%s: %v %v", name, settings.Auth, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error naming %q, got %v", name, c.want, err)
		}
	}
}

// Google sign-in's five old settings left in the environment are ignored:
// the server starts and warns once, never printing a value.
func TestServeWarnsOnceAboutLeftoverGoogleSettings(t *testing.T) {
	args := []string{"--public-origin", "https://cb.example.test", "--bootstrap-owner", "owner@example.test"}
	settings, err := ParseServe(args, func(string) string { return "" })
	if err != nil || settings.LeftoverGoogleSettings {
		t.Fatalf("clean env: %v %v", settings.LeftoverGoogleSettings, err)
	}
	for _, k := range []string{"CLAWBRIDGE_AUDIENCE", "CLAWBRIDGE_OAUTH_CLIENT_ID", "CLAWBRIDGE_OAUTH_CLIENT_SECRET", "CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID", "CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET"} {
		settings, err := ParseServe(args, func(name string) string {
			if name == k {
				return "synthetic-leftover-value"
			}
			return ""
		})
		if err != nil || !settings.LeftoverGoogleSettings {
			t.Errorf("%s: %v %v", k, settings.LeftoverGoogleSettings, err)
		}
	}
	if LeftoverGoogleSettingsWarning != "Google sign-in settings are set but unused since v1.0 (CLAWBRIDGE_AUDIENCE, CLAWBRIDGE_OAUTH_*); remove them from the env file" {
		t.Fatalf("warning text changed: %q", LeftoverGoogleSettingsWarning)
	}
	// Run logs the one line, then goes on to the database.
	for _, k := range googleSettings {
		t.Setenv(k, "synthetic-leftover-value")
	}
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "")
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	err = Run(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "Postgres connection") {
		t.Fatalf("leftover settings stopped the start: %v", err)
	}
	if strings.Count(logged.String(), LeftoverGoogleSettingsWarning) != 1 || strings.Count(logged.String(), "\n") != 1 || strings.Contains(logged.String(), "synthetic-leftover-value") {
		t.Fatalf("log = %q", logged.String())
	}
}

type publicConfigExecutor struct{}

func (publicConfigExecutor) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New("unauthenticated discovery reached executor")
}

// /v1/config carries no OAuth client: the four keys a v0.3 client's strict
// parser needs are always empty.
func TestPublicConfigHasNoOAuthClient(t *testing.T) {
	handler := NewHandler(publicConfigExecutor{}, VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{}, errors.New("unexpected verification")
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `{"browser_client_id":"","browser_client_secret":"","device_client_id":"","device_client_secret":""}` {
		t.Fatalf("config response status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("public config is cacheable")
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/config", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/config status=%d", recorder.Code)
	}
	device := NewHandlerWithOptions(publicConfigExecutor{}, nil, HandlerOptions{Auth: AuthOptions{Methods: []string{AuthDevice}, PublicOrigin: "https://cb.example.test", Devices: &fakeDevices{}}})
	recorder = httptest.NewRecorder()
	device.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/config", nil))
	if strings.TrimSpace(recorder.Body.String()) != `{"browser_client_id":"","browser_client_secret":"","device_client_id":"","device_client_secret":"","auth_methods":["device"],"public_origin":"https://cb.example.test"}` {
		t.Fatalf("device config = %s", recorder.Body.String())
	}
}

// The onboarding check reads the release from /healthz without signing in.
func TestHealthzReportsVersion(t *testing.T) {
	verifier := VerifierFunc(func(context.Context, string) (Identity, error) {
		return Identity{}, errors.New("healthz called verifier")
	})
	recorder := httptest.NewRecorder()
	NewHandler(publicConfigExecutor{}, verifier).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
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
	handler := NewHandler(publicConfigExecutor{}, verifier)
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
