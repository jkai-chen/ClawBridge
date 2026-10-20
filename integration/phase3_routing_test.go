package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
)

func oauthInstallOptions(t *testing.T) install.Options {
	t.Helper()
	dir := t.TempDir()
	command := filepath.Join(dir, "clawbridge")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return install.Options{
		Agent: "claude-code", Config: filepath.Join(dir, "mcp.json"),
		Command: command, DB: filepath.Join(dir, "local.db"),
		Mode: "cloud", Workspace: "workspace-id", Project: "project-id",
		Session: "reviewer", Instance: "reviewer-instance",
		Server: "https://clawbridge.example.test", Audience: "browser-client.apps.googleusercontent.com",
		Principal: "reviewer@example.test", Cache: filepath.Join(dir, "mirror.db"),
		CredentialSource:    "oauth",
		OAuthClientID:       "browser-client.apps.googleusercontent.com",
		OAuthDeviceClientID: "device-client.apps.googleusercontent.com",
		OAuthKeyringService: "clawbridge-review-keychain",
	}
}

func TestOAuthInstallerUsesSafeRouteAndDoctorAcceptsIt(t *testing.T) {
	o := oauthInstallOptions(t)
	result, err := install.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	args, ok := result.Entry["args"].([]string)
	if !ok {
		t.Fatalf("entry args type=%T", result.Entry["args"])
	}
	want := []string{
		"mcp", "--mode", "cloud",
		"--workspace", "workspace-id", "--project", "project-id",
		"--session", "reviewer",
		"--server", "https://clawbridge.example.test",
		"--audience", "browser-client.apps.googleusercontent.com",
		"--credential-source", "oauth",
		"--oauth-client-id", "browser-client.apps.googleusercontent.com",
		"--oauth-device-client-id", "device-client.apps.googleusercontent.com",
		"--oauth-keyring-service", "clawbridge-review-keychain",
		"--principal", "reviewer@example.test",
		"--cache", o.Cache,
		"--instance", "reviewer-instance",
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("installed OAuth argv:\n got %q\nwant %q", args, want)
	}
	for _, forbidden := range []string{
		"--oauth-client-secret", "synthetic-client-secret",
		"--account", "--impersonate-service-account", "--credentials-file", "--db",
	} {
		if strings.Contains(strings.Join(args, "\x00"), forbidden) {
			t.Fatalf("OAuth argv contains %q: %v", forbidden, args)
		}
	}

	report := doctor.Run(context.Background(), doctor.Input{
		Config: doctor.Config{
			Path:    filepath.Join(t.TempDir(), "trusted.toml"),
			Present: true, Valid: true, Mode: "cloud",
			Workspace: "workspace-id", Project: "project-id",
			Session: "reviewer", Instance: "reviewer-instance",
			Server:    "https://clawbridge.example.test",
			Audience:  "browser-client.apps.googleusercontent.com",
			Principal: "reviewer@example.test", Cache: o.Cache,
			CredentialSource:    "oauth",
			OAuthClientID:       "browser-client.apps.googleusercontent.com",
			OAuthDeviceClientID: "device-client.apps.googleusercontent.com",
			OAuthKeyringService: "clawbridge-review-keychain",
		},
		Now: testDoctorTime(),
		MCPConfigs: []doctor.MCPConfig{{
			Agent: "claude-code", Name: "clawbridge", Path: o.Config,
			Workspace: "workspace-id", Project: "project-id",
			Session: "reviewer", Instance: "reviewer-instance",
			Mode: "cloud", Server: "https://clawbridge.example.test",
			Audience:  "browser-client.apps.googleusercontent.com",
			Principal: "reviewer@example.test", Cache: o.Cache,
			CredentialSource:    "oauth",
			OAuthClientID:       "browser-client.apps.googleusercontent.com",
			OAuthDeviceClientID: "device-client.apps.googleusercontent.com",
			OAuthKeyringService: "clawbridge-review-keychain",
		}},
	}, routingExecutor{})
	for _, check := range report.Checks {
		if check.ID != "mcp.claude-code.clawbridge" {
			continue
		}
		if check.Status != doctor.Pass {
			t.Fatalf("doctor rejected installer output: status=%s message=%s", check.Status, check.Message)
		}
		return
	}
	t.Fatalf("doctor did not inspect installed entry: %+v", report)
}

type routingExecutor struct{}

func (routingExecutor) Execute(context.Context, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New("authority check is outside this focused MCP test")
}

func (routingExecutor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "", "", errors.New("resolution is outside this focused MCP test")
}

func testDoctorTime() time.Time {
	return time.Date(2026, 10, 20, 20, 0, 0, 0, time.UTC)
}

func TestOAuthInstallerRejectsSecretAndCrossBackendFields(t *testing.T) {
	for name, mutate := range map[string]func(*install.Options){
		"gcloud-account": func(o *install.Options) {
			o.Account = "human@example.test"
		},
		"credentials-file": func(o *install.Options) {
			o.CredentialsFile = filepath.Join(t.TempDir(), "credentials.json")
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := oauthInstallOptions(t)
			mutate(&o)
			_, err := install.Install(o)
			if err == nil {
				t.Fatal("cross-backend field was accepted")
			}
		})
	}
}

func TestInstalledOAuthEntryHasOnlyExplicitEnvironment(t *testing.T) {
	o := oauthInstallOptions(t)
	result, err := install.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	entry := result.Entry
	env, hasEnv := entry["env"]
	if hasEnv {
		encoded, _ := json.Marshal(env)
		for _, forbidden := range []string{"refresh", "access", "id_token", "client_secret", "authorization_code", "code_verifier"} {
			if bytes.Contains(bytes.ToLower(encoded), []byte(forbidden)) {
				t.Fatalf("installed environment contains forbidden material %q: %s", forbidden, encoded)
			}
		}
	}
}

func TestPhase3CommandOutputsAndTempArtifactsDoNotLeakSecrets(t *testing.T) {
	const (
		clientSecret       = "synthetic-review-client-secret-must-not-escape"
		deviceClientSecret = "synthetic-review-device-client-secret-must-not-escape"
		configSecret       = "synthetic-review-unrelated-config-secret"
		adcSecret          = "synthetic-review-adc-private-refresh"
	)
	p := newProcessFixture(t)
	gitAt(t, p.dir, "init", "--quiet")
	home := filepath.Join(p.dir, "secret-scan-state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.toml")
	badCloud := "mode = 'cloud'\n" +
		"server = 'https://clawbridge.example.test'\n" +
		"audience = 'browser-client.apps.googleusercontent.com'\n" +
		"principal = 'reviewer@example.test'\n" +
		"cache = 'mirror.db'\n" +
		"credential_source = 'oauth'\n" +
		"oauth_client_id = 'browser-client.apps.googleusercontent.com'\n" +
		"oauth_client_secret = " + fmt.Sprintf("%q", clientSecret) + "\n" +
		"oauth_device_client_id = 'device-client.apps.googleusercontent.com'\n" +
		"oauth_device_client_secret = " + fmt.Sprintf("%q", deviceClientSecret) + "\n" +
		"account = 'must-fail-cross-backend@example.test'\n" +
		"unrelated_secret = " + fmt.Sprintf("%q", configSecret) + "\n"
	if err := os.WriteFile(configPath, []byte(badCloud), 0600); err != nil {
		t.Fatal(err)
	}
	env := append(append([]string{}, p.env...), "CLAWBRIDGE_HOME="+home)
	run := func(args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(p.binary, args...)
		cmd.Dir = p.dir
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatalf("%v unexpectedly succeeded: stdout=%s stderr=%s", args, stdout.String(), stderr.String())
		}
		return stdout.String(), stderr.String()
	}
	for _, args := range [][]string{
		{"whoami"},
		{"doctor"},
		{"mcp", "--mode", "cloud"},
	} {
		stdout, stderr := run(args...)
		for _, secret := range []string{clientSecret, deviceClientSecret, configSecret, adcSecret} {
			if strings.Contains(stdout+stderr, secret) {
				t.Fatalf("%v echoed secret sentinel %q: stdout=%s stderr=%s", args, secret, stdout, stderr)
			}
		}
	}

	// Installer JSON and argv must carry the safe OAuth route without either the
	// trusted client secret or unrelated repository/home content.
	o := oauthInstallOptions(t)
	o.Config = filepath.Join(p.dir, "scan-mcp.json")
	installResult, err := install.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	encodedEntry, _ := json.Marshal(installResult)
	for _, secret := range []string{clientSecret, deviceClientSecret, configSecret, adcSecret} {
		if bytes.Contains(encodedEntry, []byte(secret)) {
			t.Fatalf("installer result leaked %q: %s", secret, encodedEntry)
		}
	}
	argvJSON, _ := json.Marshal(installResult.Entry["args"])
	if strings.Contains(string(argvJSON), "oauth-client-secret") ||
		bytes.Contains(argvJSON, []byte(clientSecret)) ||
		bytes.Contains(argvJSON, []byte(deviceClientSecret)) {
		t.Fatalf("installed argv contains OAuth client secret material: %s", argvJSON)
	}
	if _, ok := installResult.Entry["env"]; ok {
		t.Fatalf("installed OAuth entry unexpectedly contains environment: %v", installResult.Entry["env"])
	}

	// A malformed ADC route is rejected without exposing its file contents.
	adcPath := filepath.Join(home, "adc.json")
	if err := os.WriteFile(adcPath, []byte("{\"type\":\"service_account\",\"private_key\":\""+adcSecret+"\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	badADC := "mode = 'cloud'\nserver = 'https://clawbridge.example.test'\naudience = 'https://audience.example.test'\nprincipal = 'reviewer@example.test'\ncache = 'mirror.db'\ncredential_source = 'adc'\ncredentials_file = " + fmt.Sprintf("%q", adcPath) + "\naccount = 'reviewer@example.test'\nimpersonate_service_account = 'agent@project.iam.gserviceaccount.com'\n"
	if err := os.WriteFile(configPath, []byte(badADC), 0600); err != nil {
		t.Fatal(err)
	}
	beforeFiles := snapshotRegularFiles(t, home)
	stdout, stderr := run("whoami")
	if strings.Contains(stdout+stderr, adcSecret) ||
		strings.Contains(stdout+stderr, clientSecret) ||
		strings.Contains(stdout+stderr, deviceClientSecret) {
		t.Fatalf("ADC failure echoed credential material: stdout=%s stderr=%s", stdout, stderr)
	}
	afterFiles := snapshotRegularFiles(t, home)
	if !reflect.DeepEqual(beforeFiles, afterFiles) {
		t.Fatalf("failed ADC credential selection changed temporary or trusted files:\nbefore=%v\nafter=%v", beforeFiles, afterFiles)
	}
}

func snapshotRegularFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(path, root)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
