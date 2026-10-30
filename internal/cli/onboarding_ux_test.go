package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

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
