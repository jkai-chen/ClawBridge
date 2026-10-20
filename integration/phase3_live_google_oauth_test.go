package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveGoogleDiscoveredClientsExchangeWithMatchingSecrets is an opt-in
// compatibility probe. It uses only the public discovery document and dummy
// grant values; it never requests consent or handles a user credential. Each
// public installed-app secret must be paired with its matching client ID.
func TestLiveGoogleDiscoveredClientsExchangeWithMatchingSecrets(t *testing.T) {
	baseURL := liveGoogleBaseURL(t)
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("public config redirect is not accepted")
		},
	}
	credentials := livePublicOAuthCredentials(t, client, baseURL)

	t.Run("device-code-poll", func(t *testing.T) {
		assertGoogleMatchingCredentialCompatibility(t, client, url.Values{
			"client_id":     {credentials.DeviceClientID},
			"client_secret": {credentials.DeviceClientSecret},
			"device_code":   {"clawbridge-review-dummy-device-code"},
			"grant_type":    {"urn:ietf:params:oauth:grant-type:device_code"},
		})
	})
	t.Run("desktop-authorization-code-exchange", func(t *testing.T) {
		assertGoogleMatchingCredentialCompatibility(t, client, url.Values{
			"client_id":     {credentials.BrowserClientID},
			"client_secret": {credentials.BrowserClientSecret},
			"code":          {"clawbridge-review-dummy-authorization-code"},
			"code_verifier": {strings.Repeat("a", 64)},
			"grant_type":    {"authorization_code"},
			"redirect_uri":  {"http://127.0.0.1:53682/callback"},
		})
	})
}

// TestLiveDeviceLoginDiscoveryReachesPollingNoGcloud exercises the built CLI
// with an isolated home/config and a PATH that cannot resolve gcloud. It stops
// the unconsented device flow after the prompt and a polling interval, so it
// proves acquisition without completing a human login or saving a credential.
func TestLiveDeviceLoginDiscoveryReachesPollingNoGcloud(t *testing.T) {
	baseURL := liveGoogleBaseURL(t)
	client := &http.Client{Timeout: 15 * time.Second}
	credentials := livePublicOAuthCredentials(t, client, baseURL)

	p := newProcessFixture(t)
	home := filepath.Join(p.dir, "live-home")
	configPath := filepath.Join(p.dir, "live-config.toml")
	cachePath := filepath.Join(p.dir, "live-cache.db")
	keyringService := "clawbridge-review-live-" + filepath.Base(p.dir)
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("mode = %q\nserver = %q\ncache = %q\ncredential_source = %q\noauth_keyring_service = %q\n",
		"cloud", baseURL, cachePath, "oauth", keyringService)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	originalConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	env := sanitizedLiveCLIEnv(home, configPath)
	gcloudProbe := exec.Command("/bin/sh", "-c", "command -v gcloud")
	gcloudProbe.Env = env
	if output, err := gcloudProbe.CombinedOutput(); err == nil {
		t.Fatalf("sanitized live PATH unexpectedly resolves gcloud: %s", output)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.binary, "login", "--device")
	cmd.Dir = p.dir
	cmd.Env = env
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start live device login: %v", err)
	}
	lines := make(chan string, 32)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()

	promptSeen := false
	var observedStderr strings.Builder
	waitStart := time.Now()
	for !promptSeen {
		select {
		case line, ok := <-lines:
			if !ok {
				if waitErr := <-wait; waitErr == nil {
					t.Fatal("live device login exited successfully without human consent")
				}
				t.Fatal("live device login exited before reaching Google's device prompt")
			}
			observedStderr.WriteString(line)
			observedStderr.WriteByte('\n')
			if strings.Contains(line, "Open https://www.google.com/device") &&
				strings.Contains(line, "enter code") {
				promptSeen = true
			}
		case waitErr := <-wait:
			t.Fatalf("live device login exited before reaching Google's device prompt (err=%v)", waitErr)
		case <-time.After(20 * time.Second):
			cancel()
			t.Fatal("live device login did not reach Google's device prompt within 20 seconds")
		}
	}
	if elapsed := time.Since(waitStart); elapsed > 20*time.Second {
		cancel()
		t.Fatal("live device login reached the prompt too late")
	}

	// A missing or rejected client secret terminates immediately after the
	// prompt. Surviving longer than Google's normal polling interval proves the
	// process entered pending/slow-down polling instead.
	select {
	case waitErr := <-wait:
		t.Fatalf("live device login exited immediately after the prompt (err=%v)", waitErr)
	case <-time.After(6 * time.Second):
	}
	cancel()
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		t.Fatal("live device login did not stop after reviewer cancellation")
	}
	for line := range lines {
		observedStderr.WriteString(line)
		observedStderr.WriteByte('\n')
	}

	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(originalConfig, configAfter) {
		t.Fatal("unconsented live device probe changed trusted configuration")
	}
	for _, credential := range []string{
		credentials.BrowserClientID,
		credentials.BrowserClientSecret,
		credentials.DeviceClientID,
		credentials.DeviceClientSecret,
	} {
		if strings.Contains(stdout.String(), credential) ||
			strings.Contains(observedStderr.String(), credential) {
			t.Fatal("live device output exposed a discovered installed-app credential")
		}
	}
}

func liveGoogleBaseURL(t *testing.T) string {
	t.Helper()
	if os.Getenv("CLAWBRIDGE_TEST_LIVE_GOOGLE_OAUTH") != "1" {
		t.Skip("set CLAWBRIDGE_TEST_LIVE_GOOGLE_OAUTH=1 to probe Google's public token endpoint")
	}
	rawBaseURL := strings.TrimSpace(os.Getenv("CLAWBRIDGE_TEST_PUBLIC_URL"))
	if rawBaseURL == "" {
		t.Skip("set CLAWBRIDGE_TEST_PUBLIC_URL to the deployed HTTPS origin")
	}
	baseURL, err := url.Parse(rawBaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" ||
		baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" ||
		(baseURL.Path != "" && baseURL.Path != "/") {
		t.Fatal("CLAWBRIDGE_TEST_PUBLIC_URL must be an HTTPS origin without credentials, path, query or fragment")
	}
	return strings.TrimRight(baseURL.String(), "/")
}

func sanitizedLiveCLIEnv(home, configPath string) []string {
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + home,
		"CLAWBRIDGE_HOME=" + home,
		"CLAWBRIDGE_CONFIG=" + configPath,
	}
	// Preserve only process mechanics needed by macOS subprocesses. Every
	// authority/credential variable comes from the isolated trusted config.
	for _, key := range []string{"TMPDIR", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

type liveOAuthCredentials struct {
	BrowserClientID     string
	BrowserClientSecret string
	DeviceClientID      string
	DeviceClientSecret  string
}

func livePublicOAuthCredentials(t *testing.T, client *http.Client, baseURL string) liveOAuthCredentials {
	t.Helper()
	response, err := client.Get(baseURL + "/v1/config")
	if err != nil {
		t.Fatalf("GET public config: %v", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	response.Body.Close()
	if readErr != nil {
		t.Fatalf("read public config: %v", readErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public config status=%d", response.StatusCode)
	}
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&fields); err != nil {
		t.Fatalf("public config is not JSON: %v", err)
	}
	if trailing, err := decoder.Token(); err != io.EOF || trailing != nil {
		t.Fatal("public config has trailing content")
	}
	if len(fields) != 4 {
		t.Fatalf("public config has %d fields, want exactly four public OAuth app credential fields", len(fields))
	}
	readString := func(name string) string {
		raw, ok := fields[name]
		if !ok {
			t.Fatalf("public config omitted %s", name)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || value == "" {
			t.Fatalf("public config %s is not a non-empty string", name)
		}
		return value
	}
	credentials := liveOAuthCredentials{
		BrowserClientID:     readString("browser_client_id"),
		BrowserClientSecret: readString("browser_client_secret"),
		DeviceClientID:      readString("device_client_id"),
		DeviceClientSecret:  readString("device_client_secret"),
	}
	for _, clientID := range []string{credentials.BrowserClientID, credentials.DeviceClientID} {
		if !strings.HasSuffix(clientID, ".apps.googleusercontent.com") {
			t.Fatal("public config client ID is not a Google OAuth client ID")
		}
	}
	if credentials.BrowserClientSecret == credentials.DeviceClientSecret {
		t.Fatal("public browser and device client secrets are not distinct")
	}
	return credentials
}

func assertGoogleMatchingCredentialCompatibility(t *testing.T, client *http.Client, form url.Values) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Google token endpoint: %v", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<10))
	response.Body.Close()
	if readErr != nil {
		t.Fatalf("read Google token response: %v", readErr)
	}
	var result struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &result)
	if strings.Contains(strings.ToLower(result.Error), "secret") ||
		strings.Contains(strings.ToLower(result.ErrorDescription), "client_secret") ||
		strings.Contains(strings.ToLower(result.ErrorDescription), "client secret") {
		t.Fatal("Google rejects the matching discovered client credential")
	}
	if response.StatusCode >= 500 {
		t.Fatalf("Google token endpoint unavailable: status=%d", response.StatusCode)
	}
	if result.Error != "" && result.Error != "invalid_grant" {
		t.Fatalf("Google rejected the matching-credential dummy grant as %q", result.Error)
	}
}
