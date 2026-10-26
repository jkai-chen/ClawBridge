package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/server"
)

func TestOAuthDiscoveryStrictContract(t *testing.T) {
	valid := `{"browser_client_id":"browser-client.apps.googleusercontent.com","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device-client.apps.googleusercontent.com","device_client_secret":"synthetic-device-client-credential"}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"empty", http.StatusOK, ""},
		{"array", http.StatusOK, `[]`},
		{"null", http.StatusOK, `null`},
		{"missing-field", http.StatusOK, `{"browser_client_id":"browser-client.apps.googleusercontent.com"}`},
		{"missing-browser-secret", http.StatusOK, `{"browser_client_id":"browser-client.apps.googleusercontent.com","device_client_id":"device-client.apps.googleusercontent.com","device_client_secret":"synthetic-device-client-credential"}`},
		{"missing-device-secret", http.StatusOK, `{"browser_client_id":"browser-client.apps.googleusercontent.com","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device-client.apps.googleusercontent.com"}`},
		{"unknown-field", http.StatusOK, `{"browser_client_id":"browser-client.apps.googleusercontent.com","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device-client.apps.googleusercontent.com","device_client_secret":"synthetic-device-client-credential","audience":"audience"}`},
		{"duplicate-browser-id", http.StatusOK, `{"browser_client_id":"first.apps.googleusercontent.com","browser_client_id":"second.apps.googleusercontent.com","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device-client.apps.googleusercontent.com","device_client_secret":"synthetic-device-client-credential"}`},
		{"duplicate-browser-secret", http.StatusOK, `{"browser_client_id":"browser-client.apps.googleusercontent.com","browser_client_secret":"first","browser_client_secret":"second","device_client_id":"device-client.apps.googleusercontent.com","device_client_secret":"synthetic-device-client-credential"}`},
		{"duplicate-device-id", http.StatusOK, `{"browser_client_id":"browser-client.apps.googleusercontent.com","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"first.apps.googleusercontent.com","device_client_id":"second.apps.googleusercontent.com","device_client_secret":"synthetic-device-client-credential"}`},
		{"duplicate-device-secret", http.StatusOK, `{"browser_client_id":"browser-client.apps.googleusercontent.com","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device-client.apps.googleusercontent.com","device_client_secret":"first","device_client_secret":"second"}`},
		{"duplicate-unknown", http.StatusOK, `{"browser_client_id":"browser-client.apps.googleusercontent.com","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device-client.apps.googleusercontent.com","device_client_secret":"synthetic-device-client-credential","unexpected":"first","unexpected":"second"}`},
		{"non-string", http.StatusOK, `{"browser_client_id":1,"browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device-client.apps.googleusercontent.com","device_client_secret":"synthetic-device-client-credential"}`},
		{"trailing-object", http.StatusOK, valid + `{}`},
		{"non-success", http.StatusBadRequest, valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/config" {
					t.Errorf("unexpected discovery request: %s %s", r.Method, r.URL.RequestURI())
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			if _, err := DiscoverOAuthClient(context.Background(), server.URL, nil); err == nil {
				t.Fatalf("accepted invalid discovery: %s", tc.body)
			} else if strings.Contains(err.Error(), "synthetic-") {
				t.Fatalf("discovery error echoed installed-app credential material: %v", err)
			}
		})
	}
}

func TestOAuthDiscoveryRequiresStrictJSONContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(`{"browser_client_id":"browser","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device","device_client_secret":"synthetic-device-client-credential"}`))
	}))
	defer server.Close()
	if _, err := DiscoverOAuthClient(context.Background(), server.URL, nil); err == nil {
		t.Fatal("accepted non-JSON discovery response")
	}
}

func TestOAuthDiscoveryReturnsCompleteInstalledAppBundle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"browser_client_id":"browser-client.apps.googleusercontent.com","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device-client.apps.googleusercontent.com","device_client_secret":"synthetic-device-client-credential"}`))
	}))
	defer server.Close()
	got, err := DiscoverOAuthClient(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.BrowserClientID != "browser-client.apps.googleusercontent.com" ||
		got.BrowserClientSecret != "synthetic-browser-client-credential" ||
		got.DeviceClientID != "device-client.apps.googleusercontent.com" ||
		got.DeviceClientSecret != "synthetic-device-client-credential" {
		t.Fatalf("unexpected installed-app bundle: %#v", got)
	}
}

func TestOAuthDiscoveryRefusesRedirects(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		followed.Store(true)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/config", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if _, err := DiscoverOAuthClient(context.Background(), redirect.URL, nil); err == nil {
		t.Fatal("discovery followed or accepted a redirect")
	}
	if followed.Load() {
		t.Fatal("discovery requested the redirect target")
	}
}

func TestOAuthDiscoveryRejectsOversizedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"browser_client_id":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", maxOAuthDiscoveryResponse)))
		_, _ = w.Write([]byte(`","browser_client_secret":"synthetic","device_client_id":"device","device_client_secret":"synthetic"}`))
	}))
	defer server.Close()
	if _, err := DiscoverOAuthClient(context.Background(), server.URL, nil); err == nil || !strings.Contains(err.Error(), "safety limit") {
		t.Fatalf("oversized discovery response: %v", err)
	}
}

func TestOAuthDiscoveryHTTPSOnlyExceptNumericLoopback(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:8080",
		"http://example.test",
		"http://[::1]:8080/path",
		"ftp://127.0.0.1:8080",
		"https://user:pass@example.test",
		"https://example.test/v1",
		"https://example.test?target=other",
		"https://example.test#fragment",
	} {
		if _, err := validateDiscoveryServerURL(raw); err == nil {
			t.Fatalf("accepted unsafe discovery URL %q", raw)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "https://bridge.example.test"} {
		if _, err := validateDiscoveryServerURL(raw); err != nil {
			t.Fatalf("rejected valid discovery URL %q: %v", raw, err)
		}
	}
}

func TestOAuthDiscoveryDoesNotEchoUntrustedBody(t *testing.T) {
	const secret = "synthetic-client-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"oauth_client_secret":"` + secret + `"}`))
	}))
	defer server.Close()
	_, err := DiscoverOAuthClient(context.Background(), server.URL, nil)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("discovery error exposed response or URL: %v", err)
	}
}

func TestOAuthDiscoveryTimeoutIsBounded(t *testing.T) {
	client := discoveryHTTPClient(&http.Client{Timeout: 0})
	if client.Timeout <= 0 || client.Timeout > 10*time.Second {
		t.Fatalf("discovery timeout=%s", client.Timeout)
	}
	if client.CheckRedirect == nil || !errors.Is(client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("discovery redirect policy is not fail-closed")
	}
}

// The server's own /v1/config answer, with the sign-in fields it adds,
// passes the strict parser (a v-next server broke login when it didn't).
func TestOAuthDiscoveryReadsTheServersSignInFields(t *testing.T) {
	metadata := server.OAuthMetadata{
		BrowserClientID: "browser-client.apps.googleusercontent.com", BrowserClientSecret: "synthetic-browser-client-credential",
		DeviceClientID: "device-client.apps.googleusercontent.com", DeviceClientSecret: "synthetic-device-client-credential",
	}
	for _, c := range []struct {
		name   string
		auth   server.AuthOptions
		want   string
		origin string
	}{
		{"google only", server.AuthOptions{}, "[google]", ""},
		{"google and device", server.AuthOptions{Methods: []string{server.AuthGoogle, server.AuthDevice}, PublicOrigin: "https://cb.example.test"}, "[google device]", "https://cb.example.test"},
	} {
		t.Run(c.name, func(t *testing.T) {
			handler := server.NewHandlerWithOptions(nil, nil, server.HandlerOptions{OAuth: metadata, Auth: c.auth})
			ts := httptest.NewServer(handler)
			defer ts.Close()
			got, err := DiscoverOAuthClient(context.Background(), ts.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.BrowserClientID != metadata.BrowserClientID || got.DeviceClientSecret != metadata.DeviceClientSecret || fmt.Sprint(got.AuthMethods) != c.want || got.PublicOrigin != c.origin {
				t.Fatalf("%#v", got)
			}
		})
	}
	base := `{"browser_client_id":"b","browser_client_secret":"bs","device_client_id":"d","device_client_secret":"ds"`
	for name, body := range map[string]string{
		"methods not an array":   base + `,"auth_methods":"google"}`,
		"a method not a string":  base + `,"auth_methods":[1]}`,
		"an empty method":        base + `,"auth_methods":[""]}`,
		"origin not a string":    base + `,"public_origin":1}`,
		"methods twice":          base + `,"auth_methods":["google"],"auth_methods":["device"]}`,
		"an OAuth field missing": `{"browser_client_id":"b","browser_client_secret":"bs","device_client_id":"d","auth_methods":["google"],"public_origin":"x"}`,
	} {
		if _, err := parseOAuthDiscovery([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
