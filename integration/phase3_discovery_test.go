package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
	"clawbridge/internal/server"
)

const (
	discoveryBrowserClient = "review-browser.apps.googleusercontent.com"
	discoveryBrowserSecret = "review-browser-public-app-secret-must-not-log"
	discoveryDeviceClient  = "review-device.apps.googleusercontent.com"
	discoveryDeviceSecret  = "review-device-public-app-secret-must-not-log"
)

func discoveryConfigJSON(browserID, browserSecret, deviceID, deviceSecret string) string {
	body, _ := json.Marshal(struct {
		BrowserClientID     string `json:"browser_client_id"`
		BrowserClientSecret string `json:"browser_client_secret"`
		DeviceClientID      string `json:"device_client_id"`
		DeviceClientSecret  string `json:"device_client_secret"`
	}{
		BrowserClientID:     browserID,
		BrowserClientSecret: browserSecret,
		DeviceClientID:      deviceID,
		DeviceClientSecret:  deviceSecret,
	})
	return string(body)
}

func TestPhase3OAuthDiscoveryTransportContract(t *testing.T) {
	t.Run("exact-request-and-response", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if r.Method != http.MethodGet {
				t.Errorf("discovery method=%s, want GET", r.Method)
			}
			if r.URL.RequestURI() != "/v1/config" {
				t.Errorf("discovery request URI=%q, want /v1/config", r.URL.RequestURI())
			}
			if got := r.Header.Get("Accept"); got != "application/json" {
				t.Errorf("discovery Accept=%q, want application/json", got)
			}
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("discovery sent Authorization=%q", got)
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = io.WriteString(w, discoveryConfigJSON(
				discoveryBrowserClient,
				discoveryBrowserSecret,
				discoveryDeviceClient,
				discoveryDeviceSecret,
			))
		}))
		defer server.Close()

		got, err := remote.DiscoverOAuthClient(context.Background(), server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.BrowserClientID != discoveryBrowserClient ||
			got.BrowserClientSecret != discoveryBrowserSecret ||
			got.DeviceClientID != discoveryDeviceClient ||
			got.DeviceClientSecret != discoveryDeviceSecret {
			t.Fatal("discovered credential fields were missing, reordered or cross-paired")
		}
		if requests.Load() != 1 {
			t.Fatalf("discovery requests=%d, want exactly one", requests.Load())
		}
	})

	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{name: "empty", body: ""},
		{name: "array", body: `[]`},
		{name: "missing-browser-id", body: `{"browser_client_secret":"secret","device_client_id":"` + discoveryDeviceClient + `","device_client_secret":"secret"}`},
		{name: "missing-browser-secret", body: `{"browser_client_id":"` + discoveryBrowserClient + `","device_client_id":"` + discoveryDeviceClient + `","device_client_secret":"secret"}`},
		{name: "missing-device-id", body: `{"browser_client_id":"` + discoveryBrowserClient + `","browser_client_secret":"secret","device_client_secret":"secret"}`},
		{name: "missing-device-secret", body: `{"browser_client_id":"` + discoveryBrowserClient + `","browser_client_secret":"secret","device_client_id":"` + discoveryDeviceClient + `"}`},
		{name: "unknown-field", body: `{"browser_client_id":"` + discoveryBrowserClient + `","browser_client_secret":"secret","device_client_id":"` + discoveryDeviceClient + `","device_client_secret":"secret","oauth_client_secret":"must-not-be-used"}`},
		{name: "non-string-browser-id", body: `{"browser_client_id":1,"browser_client_secret":"secret","device_client_id":"` + discoveryDeviceClient + `","device_client_secret":"secret"}`},
		{name: "non-string-browser-secret", body: `{"browser_client_id":"` + discoveryBrowserClient + `","browser_client_secret":1,"device_client_id":"` + discoveryDeviceClient + `","device_client_secret":"secret"}`},
		{name: "non-string-device-id", body: `{"browser_client_id":"` + discoveryBrowserClient + `","browser_client_secret":"secret","device_client_id":1,"device_client_secret":"secret"}`},
		{name: "non-string-device-secret", body: `{"browser_client_id":"` + discoveryBrowserClient + `","browser_client_secret":"secret","device_client_id":"` + discoveryDeviceClient + `","device_client_secret":1}`},
		{name: "duplicate-browser-id", body: `{"browser_client_id":"first","browser_client_id":"` + discoveryBrowserClient + `","browser_client_secret":"secret","device_client_id":"` + discoveryDeviceClient + `","device_client_secret":"secret"}`},
		{name: "duplicate-browser-secret", body: `{"browser_client_id":"` + discoveryBrowserClient + `","browser_client_secret":"first","browser_client_secret":"second","device_client_id":"` + discoveryDeviceClient + `","device_client_secret":"secret"}`},
		{name: "duplicate-device-id", body: `{"browser_client_id":"` + discoveryBrowserClient + `","browser_client_secret":"secret","device_client_id":"first","device_client_id":"` + discoveryDeviceClient + `","device_client_secret":"secret"}`},
		{name: "duplicate-device-secret", body: `{"browser_client_id":"` + discoveryBrowserClient + `","browser_client_secret":"secret","device_client_id":"` + discoveryDeviceClient + `","device_client_secret":"first","device_client_secret":"second"}`},
		{name: "trailing-content", body: discoveryConfigJSON(discoveryBrowserClient, discoveryBrowserSecret, discoveryDeviceClient, discoveryDeviceSecret) + `{}`},
		{name: "non-success", status: http.StatusBadGateway, body: discoveryConfigJSON(discoveryBrowserClient, discoveryBrowserSecret, discoveryDeviceClient, discoveryDeviceSecret)},
		{name: "wrong-content-type", contentType: "text/plain", body: discoveryConfigJSON(discoveryBrowserClient, discoveryBrowserSecret, discoveryDeviceClient, discoveryDeviceSecret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			contentType := tc.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			if _, err := remote.DiscoverOAuthClient(context.Background(), server.URL, nil); err == nil {
				t.Fatal("accepted an invalid discovery response")
			}
		})
	}

	for _, tc := range []struct {
		name                               string
		browserID, browserSecret, deviceID string
		deviceSecret                       string
	}{
		{
			name:      "omitted-browser-pair",
			browserID: "", browserSecret: "",
			deviceID: discoveryDeviceClient, deviceSecret: discoveryDeviceSecret,
		},
		{
			name:      "omitted-device-pair",
			browserID: discoveryBrowserClient, browserSecret: discoveryBrowserSecret,
			deviceID: "", deviceSecret: "",
		},
		{
			name:      "all-pairs-omitted",
			browserID: "", browserSecret: "",
			deviceID: "", deviceSecret: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, discoveryConfigJSON(tc.browserID, tc.browserSecret, tc.deviceID, tc.deviceSecret))
			}))
			defer server.Close()
			got, err := remote.DiscoverOAuthClient(context.Background(), server.URL, nil)
			if err != nil {
				t.Fatalf("valid four-field response with omitted unrelated pair was rejected: %v", err)
			}
			if got.BrowserClientID != tc.browserID || got.BrowserClientSecret != tc.browserSecret ||
				got.DeviceClientID != tc.deviceID || got.DeviceClientSecret != tc.deviceSecret {
				t.Fatal("omitted four-field values were not preserved exactly")
			}
		})
	}

	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(fmt.Sprintf("redirect-%d-refused", status), func(t *testing.T) {
			var followed atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				followed.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, discoveryConfigJSON("redirected", "redirected", "redirected", "redirected"))
			}))
			defer target.Close()
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/v1/config", status)
			}))
			defer redirect.Close()
			if _, err := remote.DiscoverOAuthClient(context.Background(), redirect.URL, nil); err == nil {
				t.Fatalf("accepted discovery redirect status %d", status)
			}
			if followed.Load() != 0 {
				t.Fatalf("redirect status %d target requests=%d, want zero", status, followed.Load())
			}
		})
	}

	t.Run("oversized-response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"browser_client_id":"`)
			_, _ = io.WriteString(w, strings.Repeat("x", 17<<10))
			_, _ = io.WriteString(w, `","browser_client_secret":"`+discoveryBrowserSecret+
				`","device_client_id":"`+discoveryDeviceClient+
				`","device_client_secret":"`+discoveryDeviceSecret+`"}`)
		}))
		defer server.Close()
		if _, err := remote.DiscoverOAuthClient(context.Background(), server.URL, nil); err == nil || !strings.Contains(err.Error(), "safety limit") {
			t.Fatalf("oversized response error=%v", err)
		}
	})

	t.Run("bounded-timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer server.Close()
		client := &http.Client{Timeout: 75 * time.Millisecond}
		started := time.Now()
		if _, err := remote.DiscoverOAuthClient(context.Background(), server.URL, client); err == nil {
			t.Fatal("accepted a discovery response that never completed")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("discovery timeout took %s, want <= 1s for injected client bound", elapsed)
		}
	})

	for _, tc := range []struct {
		name string
		url  string
	}{
		{name: "localhost-http", url: "http://localhost:43210"},
		{name: "remote-http", url: "http://example.invalid"},
		{name: "path", url: "https://bridge.example.test/v1"},
		{name: "query", url: "https://bridge.example.test?redirect=attacker"},
		{name: "fragment", url: "https://bridge.example.test#fragment"},
		{name: "credentials", url: "https://user:secret@bridge.example.test"},
	} {
		t.Run("url-policy-"+tc.name, func(t *testing.T) {
			_, err := remote.DiscoverOAuthClient(context.Background(), tc.url, nil)
			if err == nil {
				t.Fatalf("accepted unsafe discovery URL %q", tc.url)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "attacker") {
				t.Fatalf("unsafe URL error leaked untrusted content: %v", err)
			}
		})
	}

	t.Run("redacted-non-success-body", func(t *testing.T) {
		const secret = "synthetic-discovery-secret-must-not-echo"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"oauth_client_secret":"`+secret+`","body":"full-response"}`)
		}))
		defer server.Close()
		_, err := remote.DiscoverOAuthClient(context.Background(), server.URL, nil)
		if err == nil {
			t.Fatal("accepted non-success discovery response")
		}
		for _, forbidden := range []string{secret, "full-response", server.URL} {
			if strings.Contains(err.Error(), forbidden) {
				t.Fatalf("discovery error leaked %q: %v", forbidden, err)
			}
		}
	})
}

func TestPhase3OAuthDiscoveryCLIFailClosed(t *testing.T) {
	p := newProcessFixture(t)
	binDir := filepath.Join(p.dir, "discovery-bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	openLog := filepath.Join(p.dir, "browser-open.log")
	// A provider-denial callback terminates browser acquisition without any
	// token exchange, Google request or keychain write. The log proves whether
	// discovery incorrectly advanced to acquisition.
	openScript := `#!/bin/sh
printf '%s\n' "$1" > "$CLAWBRIDGE_TEST_BROWSER_LOG"
python3 - "$1" <<'PY'
import sys
import urllib.parse
import urllib.request

authorize = urllib.parse.urlparse(sys.argv[1])
query = urllib.parse.parse_qs(authorize.query)
redirect = urllib.parse.urlparse(query["redirect_uri"][0])
callback = redirect._replace(query=urllib.parse.urlencode({
    "error": "access_denied",
    "state": query["state"][0],
})).geturl()
try:
    urllib.request.urlopen(callback, timeout=2).read()
except Exception:
    pass
PY
`
	for _, name := range []string{"open", "xdg-open"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(openScript), 0700); err != nil {
			t.Fatal(err)
		}
	}

	oversizedResponse := discoveryConfigJSON(
		strings.Repeat("x", 17<<10),
		discoveryBrowserSecret,
		discoveryDeviceClient,
		discoveryDeviceSecret,
	)
	for _, tc := range []struct {
		name           string
		device         bool
		explicitID     string
		explicitSecret string
		response       string
		status         int
		content        string
		redirect       bool
		wantOpen       bool
		afterDevice    bool
	}{
		{
			name:     "browser-empty-applicable-id",
			response: discoveryConfigJSON("", discoveryBrowserSecret, discoveryDeviceClient, discoveryDeviceSecret),
		},
		{
			name:     "device-empty-applicable-id",
			device:   true,
			response: discoveryConfigJSON(discoveryBrowserClient, discoveryBrowserSecret, "", discoveryDeviceSecret),
		},
		{
			name:     "browser-empty-applicable-secret",
			response: discoveryConfigJSON(discoveryBrowserClient, "", discoveryDeviceClient, discoveryDeviceSecret),
		},
		{
			name:     "device-empty-applicable-secret",
			device:   true,
			response: discoveryConfigJSON(discoveryBrowserClient, discoveryBrowserSecret, discoveryDeviceClient, ""),
		},
		{
			name:     "malformed-json",
			response: `{"browser_client_id":`,
		},
		{
			name: "duplicate-field",
			response: `{"browser_client_id":"first","browser_client_id":"` + discoveryBrowserClient +
				`","browser_client_secret":"first","device_client_id":"` + discoveryDeviceClient +
				`","device_client_secret":"first"}`,
		},
		{
			name: "unknown-field",
			response: `{"browser_client_id":"` + discoveryBrowserClient +
				`","browser_client_secret":"` + discoveryBrowserSecret +
				`","device_client_id":"` + discoveryDeviceClient +
				`","device_client_secret":"` + discoveryDeviceSecret +
				`","oauth_client_secret":"must-not-be-used"}`,
		},
		{
			name: "non-string-field",
			response: `{"browser_client_id":1,"browser_client_secret":"` + discoveryBrowserSecret +
				`","device_client_id":"` + discoveryDeviceClient +
				`","device_client_secret":"` + discoveryDeviceSecret + `"}`,
		},
		{
			name:     "trailing-content",
			response: discoveryConfigJSON(discoveryBrowserClient, discoveryBrowserSecret, discoveryDeviceClient, discoveryDeviceSecret) + `{}`,
		},
		{
			name:     "oversized-response",
			response: oversizedResponse,
		},
		{
			name:     "non-success-secret-body",
			status:   http.StatusBadGateway,
			response: `{"oauth_client_secret":"synthetic-body-secret"}`,
		},
		{
			name:     "redirect",
			redirect: true,
		},
		{
			name:           "explicit-browser-id-bypasses-discovery",
			explicitID:     discoveryBrowserClient,
			explicitSecret: discoveryBrowserSecret,
			wantOpen:       true,
		},
		{
			name: "browser-after-device-rediscovers-desktop", afterDevice: true,
			explicitID: discoveryDeviceClient, explicitSecret: discoveryDeviceSecret,
			response: discoveryConfigJSON(discoveryBrowserClient, discoveryBrowserSecret, discoveryDeviceClient, discoveryDeviceSecret),
			wantOpen: true,
		},
		{
			name: "browser-after-device-discovery-fails-closed", afterDevice: true,
			explicitID: discoveryDeviceClient, explicitSecret: discoveryDeviceSecret,
			status: http.StatusBadGateway, response: `{}`,
		},
		{
			name: "browser-after-device-rejects-device-as-desktop", afterDevice: true,
			explicitID: discoveryDeviceClient, explicitSecret: discoveryDeviceSecret,
			response: discoveryConfigJSON(discoveryDeviceClient, discoveryDeviceSecret, discoveryDeviceClient, discoveryDeviceSecret),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "state")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			var configCalls, redirectCalls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirectCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, discoveryConfigJSON("redirected", "redirected", "redirected", "redirected"))
			}))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				configCalls.Add(1)
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/config" {
					t.Errorf("discovery request=%s %s", r.Method, r.URL.RequestURI())
				}
				if tc.redirect {
					http.Redirect(w, r, target.URL+"/v1/config", http.StatusTemporaryRedirect)
					return
				}
				status := tc.status
				if status == 0 {
					status = http.StatusOK
				}
				content := tc.content
				if content == "" {
					content = "application/json"
				}
				w.Header().Set("Content-Type", content)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()

			config := "mode = 'cloud'\n" +
				"server = " + quoteTOML(server.URL) + "\n" +
				"credential_source = 'oauth'\n" +
				"oauth_keyring_service = 'review-no-keychain-write'\n"
			if tc.explicitID != "" {
				config += "oauth_client_id = " + quoteTOML(tc.explicitID) + "\n"
			}
			if tc.explicitSecret != "" {
				config += "oauth_client_secret = " + quoteTOML(tc.explicitSecret) + "\n"
			}
			if tc.afterDevice {
				config += "oauth_device_client_id = " + quoteTOML(discoveryDeviceClient) + "\n" +
					"oauth_device_client_secret = " + quoteTOML(discoveryDeviceSecret) + "\n" +
					"audience = " + quoteTOML(discoveryDeviceClient) + "\n"
			}
			configPath := filepath.Join(home, "config.toml")
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(openLog); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}

			args := []string{"login"}
			if tc.device {
				args = append(args, "--device")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, p.binary, args...)
			cmd.Dir = p.dir
			cmd.Env = append(discoveryProcessEnvWithProxy(p.env, "http://127.0.0.1:1"),
				"CLAWBRIDGE_HOME="+home,
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CLAWBRIDGE_TEST_BROWSER_LOG="+openLog,
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err = cmd.Run()
			if ctx.Err() != nil {
				t.Fatalf("login did not fail closed before acquisition: %v stdout=%s stderr=%s", ctx.Err(), stdout.String(), stderr.String())
			}
			if tc.wantOpen {
				if err == nil {
					t.Fatalf("provider-denied login unexpectedly succeeded: stdout=%s stderr=%s", stdout.String(), stderr.String())
				}
				if _, statErr := os.Stat(openLog); statErr != nil {
					t.Fatalf("explicit client ID did not reach browser acquisition: %v stderr=%s", statErr, stderr.String())
				}
				wantCalls := int32(0)
				if tc.afterDevice {
					wantCalls = 1
				}
				if configCalls.Load() != wantCalls {
					t.Fatalf("discovery calls=%d, want %d", configCalls.Load(), wantCalls)
				}
				opened, readErr := os.ReadFile(openLog)
				if readErr != nil {
					t.Fatal(readErr)
				}
				authorization, parseErr := url.Parse(strings.TrimSpace(string(opened)))
				if parseErr != nil || authorization.Query().Get("client_id") != discoveryBrowserClient {
					t.Fatal("browser acquisition did not use the Desktop client")
				}
			} else {
				if err == nil {
					t.Fatalf("invalid discovery unexpectedly succeeded: stdout=%s", stdout.String())
				}
				if _, statErr := os.Stat(openLog); !os.IsNotExist(statErr) {
					t.Fatalf("invalid discovery reached browser/device acquisition; browser log stat=%v stderr=%s", statErr, stderr.String())
				}
				if configCalls.Load() != 1 {
					t.Fatalf("discovery requests=%d, want one", configCalls.Load())
				}
			}
			if redirectCalls.Load() != 0 {
				t.Fatalf("discovery followed redirect %d times", redirectCalls.Load())
			}
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("login failure changed trusted config: before=%s after=%s", before, after)
			}
			if _, err := os.Stat(filepath.Join(home, "local.db")); !os.IsNotExist(err) {
				t.Fatalf("login failure opened or created a local database: %v", err)
			}
			combined := stdout.String() + stderr.String()
			for _, forbidden := range []string{
				"synthetic-body-secret",
				discoveryBrowserSecret,
				discoveryDeviceSecret,
				server.URL,
				"full-response",
			} {
				if strings.Contains(combined, forbidden) {
					t.Fatalf("login error leaked %q: %s", forbidden, combined)
				}
			}
		})
	}
}

func TestPhase3OAuthDeviceExplicitIDBypassesDiscovery(t *testing.T) {
	p := newProcessFixture(t)
	var discoveryCalls, proxyCalls atomic.Int32
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discoveryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, discoveryConfigJSON(
			discoveryBrowserClient,
			discoveryBrowserSecret,
			discoveryDeviceClient,
			discoveryDeviceSecret,
		))
	}))
	defer discovery.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			proxyCalls.Add(1)
		}
		http.Error(w, "synthetic proxy denial", http.StatusBadGateway)
	}))
	defer proxy.Close()

	home := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	config := "mode = 'cloud'\n" +
		"server = " + quoteTOML(discovery.URL) + "\n" +
		"credential_source = 'oauth'\n" +
		"oauth_device_client_id = " + quoteTOML(discoveryDeviceClient) + "\n" +
		"oauth_device_client_secret = " + quoteTOML(discoveryDeviceSecret) + "\n" +
		"oauth_keyring_service = 'review-no-keychain-write'\n"
	configPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.binary, "login", "--device")
	cmd.Dir = p.dir
	cmd.Env = append(discoveryProcessEnvWithProxy(p.env, proxy.URL),
		"CLAWBRIDGE_HOME="+home,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("explicit device login did not fail after an isolated acquisition denial: %v", ctx.Err())
	}
	if err == nil {
		t.Fatalf("synthetic proxy-denied device login unexpectedly succeeded: stdout=%s", stdout.String())
	}
	if discoveryCalls.Load() != 0 {
		t.Fatalf("explicit device client still called discovery %d times", discoveryCalls.Load())
	}
	if proxyCalls.Load() == 0 {
		t.Fatalf("explicit device client did not reach device acquisition; stderr=%s", stderr.String())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("failed explicit device acquisition changed trusted config: before=%s after=%s", before, after)
	}
}

func discoveryProcessEnvWithProxy(base []string, proxy string) []string {
	blocked := map[string]bool{
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true,
	}
	env := make([]string, 0, len(base)+8)
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if ok && blocked[name] {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HTTP_PROXY="+proxy,
		"HTTPS_PROXY="+proxy,
		"ALL_PROXY="+proxy,
		"NO_PROXY=127.0.0.1,::1,localhost",
		"http_proxy="+proxy,
		"https_proxy="+proxy,
		"all_proxy="+proxy,
		"no_proxy=127.0.0.1,::1,localhost",
	)
}

func TestPhase3OAuthDiscoveryRejectsInsecureNonLoopbackBeforeRequest(t *testing.T) {
	p := newProcessFixture(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, discoveryConfigJSON(
			discoveryBrowserClient,
			discoveryBrowserSecret,
			discoveryDeviceClient,
			discoveryDeviceSecret,
		))
	}))
	defer server.Close()
	insecureURL := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)

	home := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	config := "mode = 'cloud'\n" +
		"server = " + quoteTOML(insecureURL) + "\n" +
		"credential_source = 'oauth'\n"
	configPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p.binary, "login")
	cmd.Dir = p.dir
	cmd.Env = append(append([]string{}, p.env...), "CLAWBRIDGE_HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("insecure non-numeric-loopback discovery succeeded: stdout=%s", stdout.String())
	}
	if requests.Load() != 0 {
		t.Fatalf("insecure discovery made %d network requests", requests.Load())
	}
	if strings.Contains(stdout.String()+stderr.String(), insecureURL) {
		t.Fatalf("insecure URL was echoed: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("insecure discovery failure changed trusted config")
	}
}

func TestPhase3LoginHelpSubprocess(t *testing.T) {
	p := newProcessFixture(t)
	cmd := exec.Command(p.binary, "login", "--help")
	cmd.Dir = p.dir
	cmd.Env = p.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("login --help failed: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("login --help wrote unexpected stdout: %s", stdout.String())
	}
	for _, want := range []string{"Usage of login", "-device", "-use-gcloud"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("login --help omitted %q: %s", want, stderr.String())
		}
	}
}

func TestPhase3DiscoveryPublicConfigContainsExactlyFourCredentials(t *testing.T) {
	handler := server.NewHandlerWithOAuthMetadata(discoveryExecutor{}, server.VerifierFunc(func(context.Context, string) (server.Identity, error) {
		return server.Identity{}, fmt.Errorf("public config must not call verifier")
	}), server.OAuthMetadata{
		BrowserClientID:     discoveryBrowserClient,
		BrowserClientSecret: discoveryBrowserSecret,
		DeviceClientID:      discoveryDeviceClient,
		DeviceClientSecret:  discoveryDeviceSecret,
	})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	response, err := http.Get(httpServer.URL + "/v1/config")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public config status=%d", response.StatusCode)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("public config is invalid JSON: %v", err)
	}
	required := []string{
		"browser_client_id",
		"browser_client_secret",
		"device_client_id",
		"device_client_secret",
	}
	// The four credentials, and the sign-in methods.
	if len(fields) != len(required)+1 || fmt.Sprint(fields["auth_methods"]) != "[google]" {
		t.Fatal("public config field count is not exactly four")
	}
	want := map[string]string{
		"browser_client_id":     discoveryBrowserClient,
		"browser_client_secret": discoveryBrowserSecret,
		"device_client_id":      discoveryDeviceClient,
		"device_client_secret":  discoveryDeviceSecret,
	}
	for key, wantValue := range want {
		value, ok := fields[key].(string)
		if !ok || value != wantValue {
			t.Fatalf("public config %s has an invalid or cross-paired value", key)
		}
	}
	if strings.Contains(strings.ToLower(string(body)), "oauth_client_secret") ||
		strings.Contains(strings.ToLower(string(body)), "refresh") ||
		strings.Contains(strings.ToLower(string(body)), "token") {
		t.Fatal("public config exposed a non-contract credential field")
	}
}

type discoveryExecutor struct{}

func (discoveryExecutor) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	return bus.Result{}, fmt.Errorf("public config must not execute an operation")
}
