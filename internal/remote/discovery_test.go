package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/server"
)

func TestSignInDiscoveryStrictContract(t *testing.T) {
	valid := `{"browser_client_id":"browser-client.apps.googleusercontent.com","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device-client.apps.googleusercontent.com","device_client_secret":"synthetic-device-client-credential"}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"empty", http.StatusOK, ""},
		{"array", http.StatusOK, `[]`},
		{"null", http.StatusOK, `null`},
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
			if _, err := DiscoverSignIn(context.Background(), server.URL, nil); err == nil {
				t.Fatalf("accepted invalid discovery: %s", tc.body)
			} else if strings.Contains(err.Error(), "synthetic-") {
				t.Fatalf("discovery error echoed installed-app credential material: %v", err)
			}
		})
	}
}

func TestSignInDiscoveryRequiresStrictJSONContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(`{"browser_client_id":"browser","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device","device_client_secret":"synthetic-device-client-credential"}`))
	}))
	defer server.Close()
	if _, err := DiscoverSignIn(context.Background(), server.URL, nil); err == nil {
		t.Fatal("accepted non-JSON discovery response")
	}
}

// A server older than v1.0 still sends Google sign-in's OAuth client; it is
// read past and never kept, and a v1.0 server's empty keys are the same.
func TestSignInDiscoveryIgnoresTheOldOAuthClient(t *testing.T) {
	for _, body := range []string{
		`{"browser_client_id":"browser-client.example.test","browser_client_secret":"synthetic-browser-client-credential","device_client_id":"device-client.example.test","device_client_secret":"synthetic-device-client-credential","auth_methods":["google","device"],"public_origin":"https://cb.example.test"}`,
		`{"browser_client_id":"","browser_client_secret":"","device_client_id":"","device_client_secret":"","auth_methods":["device"],"public_origin":"https://cb.example.test"}`,
		`{"auth_methods":["device"],"public_origin":"https://cb.example.test"}`,
	} {
		got, err := parseSignInDiscovery([]byte(body))
		if err != nil || !strings.Contains(fmt.Sprint(got.AuthMethods), "device") || got.PublicOrigin != "https://cb.example.test" || strings.Contains(fmt.Sprintf("%#v", got), "synthetic") {
			t.Fatalf("%s: %#v %v", body, got, err)
		}
	}
}

func TestSignInDiscoveryRefusesRedirects(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		followed.Store(true)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/config", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if _, err := DiscoverSignIn(context.Background(), redirect.URL, nil); err == nil {
		t.Fatal("discovery followed or accepted a redirect")
	}
	if followed.Load() {
		t.Fatal("discovery requested the redirect target")
	}
}

func TestSignInDiscoveryRejectsOversizedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"browser_client_id":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", maxSignInDiscoveryResponse)))
		_, _ = w.Write([]byte(`","browser_client_secret":"synthetic","device_client_id":"device","device_client_secret":"synthetic"}`))
	}))
	defer server.Close()
	if _, err := DiscoverSignIn(context.Background(), server.URL, nil); err == nil || !strings.Contains(err.Error(), "safety limit") {
		t.Fatalf("oversized discovery response: %v", err)
	}
}

func TestSignInDiscoveryHTTPSOnlyExceptNumericLoopback(t *testing.T) {
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

func TestSignInDiscoveryDoesNotEchoUntrustedBody(t *testing.T) {
	const secret = "synthetic-client-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"oauth_client_secret":"` + secret + `"}`))
	}))
	defer server.Close()
	_, err := DiscoverSignIn(context.Background(), server.URL, nil)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("discovery error exposed response or URL: %v", err)
	}
}

func TestSignInDiscoveryTimeoutIsBounded(t *testing.T) {
	client := discoveryHTTPClient(&http.Client{Timeout: 0})
	if client.Timeout <= 0 || client.Timeout > 10*time.Second {
		t.Fatalf("discovery timeout=%s", client.Timeout)
	}
	if client.CheckRedirect == nil || !errors.Is(client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("discovery redirect policy is not fail-closed")
	}
}

// The server's own /v1/config answer passes this client's parser, and the
// parser of v0.3.4 to v0.3.13 clients, which needs the four (now empty)
// OAuth keys before it reads the sign-in fields.
func TestSignInDiscoveryReadsTheServersSignInFields(t *testing.T) {
	handler := server.NewHandlerWithOptions(nil, nil, server.HandlerOptions{Auth: server.AuthOptions{Methods: []string{server.AuthDevice}, PublicOrigin: "https://cb.example.test", Devices: noDevices{}}})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	got, err := DiscoverSignIn(context.Background(), ts.URL, nil)
	if err != nil || fmt.Sprint(got.AuthMethods) != "[device]" || got.PublicOrigin != "https://cb.example.test" {
		t.Fatalf("%#v %v", got, err)
	}
	raw, _ := http.Get(ts.URL + "/v1/config")
	var fields map[string]any
	_ = json.NewDecoder(raw.Body).Decode(&fields)
	raw.Body.Close()
	for _, k := range []string{"browser_client_id", "browser_client_secret", "device_client_id", "device_client_secret"} {
		if v, ok := fields[k].(string); !ok || v != "" {
			t.Fatalf("%s = %#v: a v0.3 client can't parse the answer, or it carries an OAuth client", k, fields[k])
		}
	}
	base := `{"browser_client_id":"b","browser_client_secret":"bs","device_client_id":"d","device_client_secret":"ds"`
	for name, body := range map[string]string{
		"methods not an array":  base + `,"auth_methods":"device"}`,
		"a method not a string": base + `,"auth_methods":[1]}`,
		"an empty method":       base + `,"auth_methods":[""]}`,
		"origin not a string":   base + `,"public_origin":1}`,
		"methods twice":         base + `,"auth_methods":["device"],"auth_methods":["device"]}`,
		"an OAuth key a number": `{"browser_client_id":1,"auth_methods":["device"]}`,
	} {
		if _, err := parseSignInDiscovery([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// noDevices is a device store that holds none.
type noDevices struct{}

func (noDevices) DeviceByID(context.Context, string) (bus.Device, error) {
	return bus.Device{}, errors.New("none")
}
func (noDevices) UseNonce(context.Context, string, string, string) error { return nil }
func (noDevices) Join(context.Context, bus.JoinRequest) (map[string]any, error) {
	return nil, bus.ErrInvalidInvite
}
