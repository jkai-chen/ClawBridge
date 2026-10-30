package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
			"--credential-source", "device", "--device-id", "dev_abcdefghjkmnpqrstvwxyz0123",
			"--key-store", "keyring", "--principal", "alice@example.test",
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
		Principal: "alice@example.test",
		Cache:     cache, CredentialSource: "device", DeviceID: "dev_abcdefghjkmnpqrstvwxyz0123", KeyStore: "keyring",
	}
	checkMCP(config, func(check Check) {
		if check.Status != Pass {
			t.Fatalf("matching device route rejected: %+v", check)
		}
	})
	for name, mutate := range map[string]func(*MCPConfig){
		"google-backend":  func(c *MCPConfig) { c.CredentialSource, c.DeviceID, c.KeyStore = "oauth", "", "" },
		"wrong-device":    func(c *MCPConfig) { c.DeviceID = "dev_zzzzzzzzzzzzzzzzzzzzzzzzzz" },
		"wrong-key-store": func(c *MCPConfig) { c.KeyStore = "file" },
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

// A profile that still signs in with Google (removed in v1.0) fails the
// config check and the sign-in check, which says how to join instead.
func TestCloudRouteValidationRejectsGoogleRoutes(t *testing.T) {
	dir := t.TempDir()
	config := Config{
		Path: filepath.Join(dir, "config.toml"), Present: true, Valid: true,
		Mode: "cloud", Server: "https://server.example.test",
		Principal: "alice@example.test", Cache: filepath.Join(dir, "cache.db"),
		CredentialSource: "oauth",
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
	signIn := false
	for _, check := range report.Checks {
		if check.ID == "config.valid" && check.Status != Fail {
			t.Fatalf("a Google route was not rejected: %+v", check)
		}
		if check.ID == "auth.token-selection" {
			signIn = check.Status == Fail && strings.Contains(check.Action, "clawbridge join")
		}
	}
	if !signIn {
		t.Fatalf("no sign-in check says how to join: %+v", report.Checks)
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

func TestPinnedClientConfigIsComparedExactly(t *testing.T) {
	m := MCPConfig{Agent: "claude-code", Mode: "cloud", Workspace: "w", Project: "p", Session: "agent", Server: "https://service.example.test", Principal: "human@example.test", Cache: filepath.Join(t.TempDir(), "cache.db"), CredentialSource: "device", DeviceID: "dev_abcdefghjkmnpqrstvwxyz0123", KeyStore: "keyring", ClientConfig: filepath.Join(t.TempDir(), "trusted.toml")}
	entry, err := expectedMCPEntry(m)
	if err != nil {
		t.Fatal(err)
	}
	if !entryMatches(entry, entry) {
		t.Fatal("same pinned route rejected")
	}
	other := m
	other.ClientConfig = filepath.Join(t.TempDir(), "other.toml")
	wrong, err := expectedMCPEntry(other)
	if err != nil {
		t.Fatal(err)
	}
	if entryMatches(entry, wrong) {
		t.Fatal("different trusted client config accepted")
	}
}

// A machine with no profile at all is pointed at join or guided setup; a
// half-configured local profile keeps the local-setup message.
func TestConfigCheckNoProfileYetPointsAtJoin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	find := func(config Config) Check {
		for _, c := range Run(context.Background(), Input{Config: config}, routeExecutor{}).Checks {
			if c.ID == "config.trusted" {
				return c
			}
		}
		t.Fatal("no config.trusted check")
		return Check{}
	}
	none := find(Config{Path: path, Present: true, Valid: true, Mode: "local"})
	if none.Status != Fail || !strings.Contains(none.Message, "No ClawBridge profile yet: run `clawbridge join` (with an invite) or `clawbridge` (guided setup)") || strings.Contains(none.Message+none.Action, "local setup") {
		t.Fatalf("%+v", none)
	}
	half := find(Config{Path: path, Present: true, Valid: true, Mode: "local", Workspace: "w"})
	if !strings.Contains(half.Message, "Local routing is incomplete") || !strings.Contains(half.Action, "Run local setup") {
		t.Fatalf("%+v", half)
	}
}
