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

func TestDeploymentPublicConfigAndUnauthenticatedAPIContract(t *testing.T) {
	var verifierCalls, executorCalls atomic.Int32
	var handler http.Handler
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	defer httpServer.Close()
	// The handler serve builds: device keys at the public origin, no
	// verifier.
	handler = server.NewHandlerWithOptions(
		deploymentExecutorFunc(func(context.Context, string, bus.Request) (bus.Result, error) {
			executorCalls.Add(1)
			return bus.Result{}, errors.New("unauthenticated deployment request reached the executor")
		}),
		nil,
		server.HandlerOptions{Auth: server.AuthOptions{Methods: []string{server.AuthDevice}, PublicOrigin: httpServer.URL, Devices: noDeploymentDevices{}}},
	)

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
	assertDeploymentHTTPContractWithExpected(t, baseURL, client, baseURL)
}

func assertDeploymentLiveHTTPContract(t *testing.T, baseURL string, client *http.Client) {
	t.Helper()
	assertDeploymentHTTPContractWithExpected(t, baseURL, client, baseURL)
}

// assertDeploymentHTTPContractWithExpected checks a v1.0 server's public
// answers: /v1/config names device sign-in at origin and carries no OAuth
// client (its four keys stay, empty, for v0.3 clients), and /v1/execute
// refuses a missing credential and a Bearer (Google ID) token alike.
func assertDeploymentHTTPContractWithExpected(
	t *testing.T,
	baseURL string,
	client *http.Client,
	origin string,
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
	var config struct {
		BrowserClientID     *string  `json:"browser_client_id"`
		BrowserClientSecret *string  `json:"browser_client_secret"`
		DeviceClientID      *string  `json:"device_client_id"`
		DeviceClientSecret  *string  `json:"device_client_secret"`
		AuthMethods         []string `json:"auth_methods"`
		PublicOrigin        string   `json:"public_origin"`
	}
	decoder := json.NewDecoder(bytes.NewReader(configBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		t.Fatalf("GET /v1/config is not the v1.0 object: %v", err)
	}
	for name, v := range map[string]*string{"browser_client_id": config.BrowserClientID, "browser_client_secret": config.BrowserClientSecret, "device_client_id": config.DeviceClientID, "device_client_secret": config.DeviceClientSecret} {
		if v == nil || *v != "" {
			t.Fatalf("GET /v1/config %s is missing or carries an OAuth client", name)
		}
	}
	if strings.Join(config.AuthMethods, ",") != "device" || config.PublicOrigin != origin {
		t.Fatalf("GET /v1/config sign-in: %v %q", config.AuthMethods, config.PublicOrigin)
	}
	for _, header := range []string{"", "Bearer eyJhbGciOiJSUzI1NiJ9.e30.c2ln"} {
		request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/execute", strings.NewReader(`{"operation":"whoami"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("POST /v1/execute: %v", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		var refused struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if response.StatusCode != http.StatusUnauthorized || json.Unmarshal(body, &refused) != nil || refused.Error.Code != "unauthenticated" {
			t.Fatalf("POST /v1/execute with %q: %d %s", header, response.StatusCode, body)
		}
		if header != "" && refused.Error.Message != server.BearerRefused {
			t.Fatalf("a Bearer token was refused without the removal notice: %s", body)
		}
	}
}

// noDeploymentDevices is a device store that holds none.
type noDeploymentDevices struct{}

func (noDeploymentDevices) DeviceByID(context.Context, string) (bus.Device, error) {
	return bus.Device{}, errors.New("none")
}
func (noDeploymentDevices) UseNonce(context.Context, string, string, string) error { return nil }
func (noDeploymentDevices) Join(context.Context, bus.JoinRequest) (map[string]any, error) {
	return nil, bus.ErrInvalidInvite
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
