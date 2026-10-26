package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"clawbridge/internal/server"
)

const (
	deploymentBrowserClient = "deployment-browser.apps.googleusercontent.com"
	deploymentBrowserSecret = "deployment-browser-public-app-secret"
	deploymentDeviceClient  = "deployment-device.apps.googleusercontent.com"
	deploymentDeviceSecret  = "deployment-device-public-app-secret"
)

func TestDeploymentPublicConfigAndUnauthenticatedAPIContract(t *testing.T) {
	var verifierCalls, executorCalls atomic.Int32
	handler := server.NewHandlerWithOAuthMetadata(
		deploymentExecutorFunc(func(context.Context, string, bus.Request) (bus.Result, error) {
			executorCalls.Add(1)
			return bus.Result{}, errors.New("unauthenticated deployment request reached the executor")
		}),
		server.VerifierFunc(func(context.Context, string) (server.Identity, error) {
			verifierCalls.Add(1)
			return server.Identity{}, errors.New("missing bearer token reached the verifier")
		}),
		server.OAuthMetadata{
			BrowserClientID:     deploymentBrowserClient,
			BrowserClientSecret: deploymentBrowserSecret,
			DeviceClientID:      deploymentDeviceClient,
			DeviceClientSecret:  deploymentDeviceSecret,
		},
	)
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	assertDeploymentHTTPContract(t, httpServer.URL, httpServer.Client())
	if verifierCalls.Load() != 0 {
		t.Fatalf("public config or unauthenticated API invoked verifier %d times", verifierCalls.Load())
	}
	if executorCalls.Load() != 0 {
		t.Fatalf("public config or unauthenticated API invoked executor %d times", executorCalls.Load())
	}
}

func TestDeploymentLiveHTTPSContract(t *testing.T) {
	rawBaseURL := strings.TrimSpace(os.Getenv("CLAWBRIDGE_TEST_PUBLIC_URL"))
	if rawBaseURL == "" {
		t.Skip("set CLAWBRIDGE_TEST_PUBLIC_URL to probe the deployed HTTPS endpoint")
	}
	baseURL, err := url.Parse(rawBaseURL)
	if err != nil || baseURL.Scheme != "https" || baseURL.Host == "" ||
		baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" ||
		(baseURL.Path != "" && baseURL.Path != "/") {
		t.Fatal("CLAWBRIDGE_TEST_PUBLIC_URL must be an HTTPS origin without credentials, path, query or fragment")
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("deployed endpoint redirect is not accepted")
		},
	}
	assertDeploymentLiveHTTPContract(t, strings.TrimRight(baseURL.String(), "/"), client)
}

func assertDeploymentHTTPContract(t *testing.T, baseURL string, client *http.Client) {
	t.Helper()
	assertDeploymentHTTPContractWithExpected(t, baseURL, client, map[string]string{
		"browser_client_id":     deploymentBrowserClient,
		"browser_client_secret": deploymentBrowserSecret,
		"device_client_id":      deploymentDeviceClient,
		"device_client_secret":  deploymentDeviceSecret,
	})
}

func assertDeploymentLiveHTTPContract(t *testing.T, baseURL string, client *http.Client) {
	t.Helper()
	assertDeploymentHTTPContractWithExpected(t, baseURL, client, nil)
}

func assertDeploymentHTTPContractWithExpected(
	t *testing.T,
	baseURL string,
	client *http.Client,
	want map[string]string,
) {
	t.Helper()
	configResponse, err := client.Get(baseURL + "/v1/config")
	if err != nil {
		t.Fatalf("GET /v1/config: %v", err)
	}
	configBody, err := io.ReadAll(io.LimitReader(configResponse.Body, 64<<10))
	configResponse.Body.Close()
	if err != nil {
		t.Fatalf("read /v1/config: %v", err)
	}
	if configResponse.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/config status=%d", configResponse.StatusCode)
	}
	if contentType := configResponse.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("GET /v1/config content type=%q", contentType)
	}
	if cacheControl := configResponse.Header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("GET /v1/config cache control=%q", cacheControl)
	}
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(configBody))
	if err := decoder.Decode(&fields); err != nil {
		t.Fatalf("GET /v1/config is not JSON: %v", err)
	}
	if trailing, err := decoder.Token(); err != io.EOF || trailing != nil {
		t.Fatalf("GET /v1/config has trailing content")
	}
	// Exactly the four installed-app fields: a Google-only server names no
	// sign-in methods, so a v0.3.3 client's parser accepts it.
	if len(fields) != 4 {
		t.Fatal("GET /v1/config field count is not exactly four")
	}
	values := make(map[string]string, len(fields))
	for key := range map[string]struct{}{
		"browser_client_id":     {},
		"browser_client_secret": {},
		"device_client_id":      {},
		"device_client_secret":  {},
	} {
		raw, ok := fields[key]
		if !ok {
			t.Fatalf("GET /v1/config omitted %s", key)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || value == "" {
			t.Fatalf("GET /v1/config %s is not a non-empty string", key)
		}
		values[key] = value
	}
	if want != nil {
		for key, wantValue := range want {
			if values[key] != wantValue {
				t.Fatalf("GET /v1/config %s is invalid or cross-paired", key)
			}
		}
	} else if !strings.HasSuffix(values["browser_client_id"], ".apps.googleusercontent.com") ||
		!strings.HasSuffix(values["device_client_id"], ".apps.googleusercontent.com") {
		t.Fatal("GET /v1/config contains a non-Google client ID")
	}
	if bytes.Contains(bytes.ToLower(configBody), []byte("oauth_client_secret")) ||
		bytes.Contains(bytes.ToLower(configBody), []byte("token")) ||
		bytes.Contains(bytes.ToLower(configBody), []byte("refresh")) {
		t.Fatal("GET /v1/config exposed a non-contract credential field")
	}

	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/execute", strings.NewReader(`{"operation":"whoami"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("unauthenticated POST /v1/execute: %v", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	if readErr != nil {
		t.Fatalf("read unauthenticated /v1/execute: %v", readErr)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /v1/execute status=%d, want 401", response.StatusCode)
	}
	var errorResponse struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errorResponse); err != nil {
		t.Fatalf("parse unauthenticated /v1/execute response: %v", err)
	}
	if errorResponse.Error.Code != "unauthenticated" {
		t.Fatal("unauthenticated /v1/execute response did not use error.code=unauthenticated")
	}
}

type deploymentExecutorFunc func(context.Context, string, bus.Request) (bus.Result, error)

func (f deploymentExecutorFunc) ExecuteAs(ctx context.Context, principal string, request bus.Request) (bus.Result, error) {
	return f(ctx, principal, request)
}

func TestDeploymentBackupRunnerFailsClosedWithoutConfiguration(t *testing.T) {
	runner := deploymentBackupRunner(t)
	fakeBin := t.TempDir()
	commandLog := filepath.Join(t.TempDir(), "unexpected-command.log")
	for _, name := range []string{
		"docker", "podman", "pg_dump", "pg_dumpall", "psql",
		"gcloud", "gsutil", "rsync", "rclone", "aws", "curl", "wget",
		"sha256sum", "shasum",
	} {
		writeFakeCommand(t, filepath.Join(fakeBin, name), commandLog)
	}
	runDir := t.TempDir()
	env := []string{
		"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + runDir,
		"XDG_CONFIG_HOME=" + filepath.Join(runDir, ".config"),
		"TMPDIR=" + runDir,
		"LANG=C",
		"LC_ALL=C",
		"CLAWBRIDGE_TEST_FAKE_COMMAND_LOG=" + commandLog,
	}
	cmd := backupRunnerCommand(t, runner)
	cmd.Dir = runDir
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("backup runner succeeded without required configuration: %s", output)
	}
	if raw, readErr := os.ReadFile(commandLog); readErr == nil && len(raw) != 0 {
		t.Fatalf("backup runner invoked external backup commands before configuration was valid: %s", raw)
	} else if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("inspect fake command log: %v", readErr)
	}
}

func deploymentBackupRunner(t *testing.T) string {
	t.Helper()
	if explicit := strings.TrimSpace(os.Getenv("CLAWBRIDGE_TEST_BACKUP_RUNNER")); explicit != "" {
		if info, err := os.Stat(explicit); err != nil || info.IsDir() {
			t.Fatalf("CLAWBRIDGE_TEST_BACKUP_RUNNER is not a file: %v", err)
		}
		return explicit
	}
	root := repositoryRoot(t)
	for _, relative := range []string{
		"deploy/backup.sh",
		"deploy/backup-postgres.sh",
		"deploy/backup-runner.sh",
		"deploy/backup",
	} {
		path := filepath.Join(root, relative)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	t.Skip("deployment backup runner is not published; set CLAWBRIDGE_TEST_BACKUP_RUNNER when available")
	return ""
}

func backupRunnerCommand(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	if strings.HasSuffix(path, ".sh") {
		return exec.Command("sh", path)
	}
	return exec.Command(path)
}

func writeFakeCommand(t *testing.T, path, logPath string) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$0\" >> %q\nexit 91\n", logPath)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}
