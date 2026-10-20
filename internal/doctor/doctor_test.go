package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"clawbridge/internal/bus"
)

type routeExecutor struct{}

func (routeExecutor) Execute(context.Context, bus.Request) (bus.Result, error) {
	return bus.Result{Source: "server", Data: map[string]any{"projects": []map[string]any{{"slug": "p"}}}}, nil
}

func (routeExecutor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "w", "p", nil
}

func TestMCPScopeAndCredentialRouteAreComparedExactly(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "clawbridge")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache.db")
	entry := map[string]any{
		"command": command,
		"args": []any{
			"mcp", "--mode", "cloud", "--workspace", "w", "--project", "p",
			"--session", "alice", "--server", "https://server.example.test",
			"--audience", "desktop-client", "--credential-source", "oauth",
			"--oauth-client-id", "desktop-client", "--oauth-device-client-id", "device-client",
			"--oauth-keyring-service", "review-keyring", "--principal", "alice@example.test",
			"--cache", cache, "--instance", "instance-1",
		},
	}
	raw, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"clawbridge": entry}})
	configPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	config := MCPConfig{
		Agent: "claude-code", Name: "clawbridge", Path: configPath,
		Workspace: "w", Project: "p", Session: "alice", Instance: "instance-1",
		Mode: "cloud", Server: "https://server.example.test",
		Audience: "desktop-client", Principal: "alice@example.test",
		Cache: cache, CredentialSource: "oauth", OAuthClientID: "desktop-client",
		OAuthDeviceClientID: "device-client", OAuthKeyringService: "review-keyring",
	}
	checkMCP(config, func(check Check) {
		if check.Status != Pass {
			t.Fatalf("matching OAuth route rejected: %+v", check)
		}
	})
	for name, mutate := range map[string]func(*MCPConfig){
		"gcloud-backend": func(c *MCPConfig) {
			c.CredentialSource = "gcloud"
			c.Account, c.ImpersonateServiceAccount = "human@example.test", "agent@example.test"
			c.OAuthClientID, c.OAuthDeviceClientID, c.OAuthKeyringService = "", "", ""
		},
		"wrong-client":    func(c *MCPConfig) { c.OAuthClientID = "other-client" },
		"wrong-audience":  func(c *MCPConfig) { c.Audience = "other-client" },
		"wrong-keyring":   func(c *MCPConfig) { c.OAuthKeyringService = "other-keyring" },
		"wrong-principal": func(c *MCPConfig) { c.Principal = "bob@example.test" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := config
			mutate(&changed)
			checkMCP(changed, func(check Check) {
				if check.Status != Fail {
					t.Fatalf("route conflict was not rejected: %+v", check)
				}
			})
		})
	}
}

func TestCloudRouteValidationRejectsCrossBackendFields(t *testing.T) {
	dir := t.TempDir()
	config := Config{
		Path: filepath.Join(dir, "config.toml"), Present: true, Valid: true,
		Mode: "cloud", Server: "https://server.example.test", Audience: "audience",
		Principal: "alice@example.test", Cache: filepath.Join(dir, "cache.db"),
		CredentialSource: "oauth", OAuthClientID: "client",
		Account: "human@example.test",
	}
	report := Run(context.Background(), Input{Config: config}, routeExecutor{})
	encoded, _ := json.Marshal(report)
	for _, forbidden := range []string{
		"synthetic-browser-client-credential", "synthetic-device-client-credential",
		"oauth_client_secret", "oauth_device_client_secret", "browser_client_secret", "device_client_secret",
	} {
		if bytes.Contains(bytes.ToLower(encoded), []byte(forbidden)) {
			t.Fatalf("doctor report contains credential material %q", forbidden)
		}
	}
	for _, check := range report.Checks {
		if check.ID == "config.valid" && check.Status != Fail {
			t.Fatalf("cross-backend route was not rejected: %+v", check)
		}
	}
}

func TestInstalledEnvironmentCannotCarryCredentialsOrOverrideRoute(t *testing.T) {
	expected := map[string]any{"args": []any{"mcp", "--mode", "cloud"}}
	for _, name := range []string{"CLAWBRIDGE_OAUTH_CLIENT_SECRET", "CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET", "CLAWBRIDGE_SERVER", "CLAWBRIDGE_MODE", "CLAWBRIDGE_CONFIG", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_AUTH_ACCESS_TOKEN", "ACCESS_TOKEN", "REFRESH_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			actual := map[string]any{"args": expected["args"], "env": map[string]any{name: "synthetic-not-for-output"}}
			if entryMatches(actual, expected) {
				t.Fatal("credential or route environment accepted")
			}
			delete(actual, "env")
			actual["env_vars"] = []any{name}
			if entryMatches(actual, expected) {
				t.Fatal("credential or route environment forwarding accepted")
			}
		})
	}
	actual := map[string]any{"args": expected["args"], "env": map[string]any{"LANG": "en_US.UTF-8", "UNRELATED_SETTING": "yes"}, "env_vars": []any{"TERM"}}
	if !entryMatches(actual, expected) {
		t.Fatal("safe unrelated environment rejected")
	}
	actual["args"] = []any{"mcp", "--mode", "cloud", "--oauth-client-secret", "synthetic-not-for-output"}
	if entryMatches(actual, expected) {
		t.Fatal("credential argv accepted")
	}
}
