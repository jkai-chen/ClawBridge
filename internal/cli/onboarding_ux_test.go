package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/auth"
	"golang.org/x/oauth2"
)

func TestBrowserSignInAlwaysShowsTheURLBeforeLaunching(t *testing.T) {
	original := launchLoginBrowser
	t.Cleanup(func() { launchLoginBrowser = original })
	target := "https://accounts.google.com/o/oauth2/v2/auth?client_id=c&state=s"
	for name, launch := range map[string]struct {
		opened bool
		err    error
	}{"opened": {true, nil}, "unknown": {false, nil}, "failed": {false, errors.New("no opener")}} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			launched := 0
			launchLoginBrowser = func(_ context.Context, got string, wait time.Duration) (bool, error) {
				launched++
				// The URL must already be on screen when the browser starts.
				if got != target || !strings.Contains(out.String(), target) || wait != loginBrowserWait {
					t.Fatalf("launched %q (wait %s) before the URL was shown: %q", got, wait, out.String())
				}
				return launch.opened, launch.err
			}
			if err := showAndOpenBrowser(&out)(context.Background(), target); err != nil {
				t.Fatal("a launch outcome must not fail sign-in while the URL is shown", err)
			}
			if launched != 1 || !strings.Contains(out.String(), "another browser or Google account") || strings.Count(out.String(), target) != 1 {
				t.Fatalf("URL not shown exactly once with its purpose: %q", out.String())
			}
		})
	}
	var out bytes.Buffer
	launchLoginBrowser = func(context.Context, string, time.Duration) (bool, error) {
		t.Fatal("a non-HTTPS URL reached the browser")
		return false, nil
	}
	if err := showAndOpenBrowser(&out)(context.Background(), "http://accounts.google.com/x"); err == nil || out.Len() != 0 {
		t.Fatal("a non-HTTPS URL was shown or accepted", err, out.String())
	}
}

// The printed URL is the one the auth package builds: client ID, PKCE
// challenge and state, never the client secret or the PKCE verifier.
func TestBrowserSignInURLCarriesNoSecret(t *testing.T) {
	const secret, verifier = "installed-app-client-secret", "pkce-verifier-0123456789-0123456789-0123456789"
	config := &oauth2.Config{ClientID: "desktop.apps.googleusercontent.com", ClientSecret: secret, RedirectURL: "http://127.0.0.1:1234/callback",
		Endpoint: oauth2.Endpoint{AuthURL: auth.GoogleEndpoints.AuthorizationURL, TokenURL: auth.GoogleEndpoints.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
		Scopes:   []string{"openid", "email"}}
	target := config.AuthCodeURL("state-value", oauth2.AccessTypeOffline, oauth2.ApprovalForce, oauth2.S256ChallengeOption(verifier))
	original := launchLoginBrowser
	t.Cleanup(func() { launchLoginBrowser = original })
	launchLoginBrowser = func(context.Context, string, time.Duration) (bool, error) { return true, nil }
	var out bytes.Buffer
	if err := showAndOpenBrowser(&out)(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) || strings.Contains(out.String(), verifier) || !strings.Contains(out.String(), "code_challenge=") {
		t.Fatalf("printed sign-in URL leaks a secret or lacks the challenge: %q", out.String())
	}
}

func TestBrowserLoginUsesTheURLShowingOpener(t *testing.T) {
	writeCloudLoginConfig(t)
	store := &loginMemoryStore{}
	installLoginTestHooks(t, store, auth.Credential{
		ClientID:  "desktop-client.apps.googleusercontent.com",
		Principal: "alice@example.test", Subject: "new", RefreshToken: "browser-refresh",
	})
	if _, _, err := runLogin(t, "login"); err != nil {
		t.Fatal(err)
	}
	if len(store.logins) != 1 || store.logins[0].OpenBrowser == nil {
		t.Fatal("browser login did not install the opener that always shows the URL", store.logins)
	}
}

func TestCredentialStoreNoteIsPlain(t *testing.T) {
	mac := credentialStoreNote("darwin")
	if !strings.Contains(mac, "macOS Keychain") || !strings.Contains(mac, "Allow") {
		t.Fatal("macOS note must name the Keychain and the prompt", mac)
	}
	for _, goos := range []string{"linux", "windows"} {
		note := credentialStoreNote(goos)
		if strings.Contains(note, "macOS") || !strings.Contains(note, "credential store") {
			t.Fatal(goos, "note must be generic", note)
		}
	}
	for _, note := range []string{mac, credentialStoreNote("linux")} {
		if strings.Contains(note, "write readiness") || strings.Contains(note, "not guaranteed") {
			t.Fatal("jargon survived", note)
		}
	}
}

func TestLinearConnectAlwaysShowsTheURLBeforeLaunching(t *testing.T) {
	original := openLinearBrowser
	t.Cleanup(func() { openLinearBrowser = original })
	target := "https://mcp.linear.app/authorize?client_id=c&state=s"
	for name, launch := range map[string]struct {
		opened bool
		err    error
	}{"opened": {true, nil}, "unknown": {false, nil}, "failed": {false, errors.New("cannot open browser")}} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			launched := 0
			openLinearBrowser = func(_ context.Context, got string) (bool, error) {
				launched++
				if got != target || !strings.Contains(out.String(), target) {
					t.Fatalf("launched %q before the URL was shown: %q", got, out.String())
				}
				return launch.opened, launch.err
			}
			if err := presentLinearURL(context.Background(), &out, false)(target); err != nil {
				t.Fatal("a launch outcome must not end the connection while the URL is shown", err)
			}
			if launched != 1 || !strings.Contains(out.String(), "another browser or Linear account") || strings.Count(out.String(), target) != 1 {
				t.Fatalf("URL not shown exactly once with its purpose: %q", out.String())
			}
		})
	}
	openLinearBrowser = func(context.Context, string) (bool, error) {
		t.Fatal("the browser was launched")
		return false, nil
	}
	var out bytes.Buffer
	if err := presentLinearURL(context.Background(), &out, true)(target); err != nil || out.String() != target+"\n" {
		t.Fatalf("--no-browser must print only the URL: %q %v", out.String(), err)
	}
	out.Reset()
	if err := presentLinearURL(context.Background(), &out, false)("http://mcp.linear.app/authorize"); err == nil || out.Len() != 0 {
		t.Fatal("a non-HTTPS URL was shown or accepted", err, out.String())
	}
}

// Linear's client is registered as public (no secret), and the URL is built as
// linearconn builds it: the PKCE challenge and state, never the verifier.
func TestLinearSignInURLCarriesNoVerifier(t *testing.T) {
	const verifier = "linear-pkce-verifier-0123456789-0123456789-0123456789"
	config := &oauth2.Config{ClientID: "registered-public-client", RedirectURL: "http://127.0.0.1:4321/callback", Scopes: []string{"read", "write"},
		Endpoint: oauth2.Endpoint{AuthURL: "https://mcp.linear.app/authorize", TokenURL: "https://mcp.linear.app/token", AuthStyle: oauth2.AuthStyleInParams}}
	target := config.AuthCodeURL("state-value", oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("resource", "https://mcp.linear.app/mcp"))
	original := openLinearBrowser
	t.Cleanup(func() { openLinearBrowser = original })
	openLinearBrowser = func(context.Context, string) (bool, error) { return true, nil }
	var out bytes.Buffer
	if err := presentLinearURL(context.Background(), &out, false)(target); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), verifier) || strings.Contains(out.String(), "client_secret") || !strings.Contains(out.String(), "code_challenge=") {
		t.Fatalf("printed Linear URL leaks the verifier or a secret, or lacks the challenge: %q", out.String())
	}
}
