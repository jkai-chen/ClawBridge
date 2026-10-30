package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fieldEntry writes a Claude Code entry in the argument order v0.3.12's
// mcp install wrote for a cloud device route with a Linear profile, an
// instance and the profile file, and returns doctor's view of it: the same
// authority, resolved from another profile (its own cache, profile file,
// device and key store), without a Linear profile or instance of its own.
func fieldEntry(t *testing.T, args map[string]string) (MCPConfig, string) {
	t.Helper()
	dir := t.TempDir()
	command := filepath.Join(dir, "clawbridge")
	if err := os.WriteFile(command, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"--mode": "cloud", "--workspace": "acme", "--project": "p", "--session": "agent",
		"--server": "https://cb.example.test", "--credential-source": "device",
		"--device-id": "dev_abcdefghjkmnpqrstvwxyz0123", "--key-store": "keyring",
		"--principal": "ada@example.test", "--cache": filepath.Join(dir, "other.cache.db"),
		"--linear-profile": "lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--instance": "inst_1",
		"--client-config": filepath.Join(dir, "profiles", "other.toml"),
	}
	for flag, value := range args {
		values[flag] = value
	}
	argv := []any{"mcp"}
	for _, flag := range []string{"--mode", "--workspace", "--project", "--session", "--server", "--credential-source", "--device-id", "--key-store", "--principal", "--cache", "--linear-profile", "--instance", "--client-config"} {
		argv = append(argv, flag, values[flag])
	}
	raw, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"clawbridge": map[string]any{"type": "stdio", "command": command, "args": argv}}})
	path := filepath.Join(dir, "copy.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return MCPConfig{
		Agent: "claude-code", Name: "clawbridge", Path: path,
		Workspace: "acme", Project: "p", Session: "agent", Mode: "cloud",
		Server: "https://cb.example.test", Principal: "ada@example.test", CredentialSource: "device",
		Cache: filepath.Join(dir, "config.cache.db"), ClientConfig: filepath.Join(dir, "config.toml"),
		DeviceID: "dev_0123456789abcdefghjkmnpqrs", KeyStore: "file",
	}, dir
}

func mcpCheck(t *testing.T, m MCPConfig) Check {
	t.Helper()
	var got Check
	checkMCP(m, func(c Check) { got = c })
	return got
}

// The field case: an entry installed against one profile, checked by a
// doctor that resolved another with the same authority, passes, and the
// details taken from the entry are named.
func TestMCPEntryFieldShapePassesWithItsOwnRouteDetails(t *testing.T) {
	m, dir := fieldEntry(t, nil)
	c := mcpCheck(t, m)
	if c.Status != Pass {
		t.Fatalf("a working entry flagged: %+v", c)
	}
	for _, want := range []string{
		"It uses the Linear profile lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa it was installed with",
		"--instance inst_1", "--cache " + filepath.Join(dir, "other.cache.db"),
		"--client-config " + filepath.Join(dir, "profiles", "other.toml"),
		"--device-id dev_abcdefghjkmnpqrstvwxyz0123", "--key-store keyring",
	} {
		if !strings.Contains(c.Message, want) {
			t.Fatalf("adopted detail %q not named: %s", want, c.Message)
		}
	}
	if strings.Contains(c.Message, "ada@example.test") {
		t.Fatalf("principal echoed: %s", c.Message)
	}
	// Given the same instance, profile, server with a trailing slash, the
	// entry still passes.
	m.Instance, m.LinearProfile, m.Server = "inst_1", "lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "https://cb.example.test/"
	if c := mcpCheck(t, m); c.Status != Pass || strings.Contains(c.Message, "--instance") {
		t.Fatalf("the same given instance or a normalised server rejected: %+v", c)
	}
}

// The reported root cause on its own: the same profile at another path. The
// entry's --client-config isn't the path doctor resolved, and was the only
// difference.
func TestMCPEntryFromAProfileAtAnotherPathPasses(t *testing.T) {
	m, dir := fieldEntry(t, nil)
	m.Cache, m.DeviceID, m.KeyStore = filepath.Join(dir, "other.cache.db"), "dev_abcdefghjkmnpqrstvwxyz0123", "keyring"
	m.LinearProfile, m.Instance = "lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "inst_1"
	c := mcpCheck(t, m)
	if c.Status != Pass || !strings.HasSuffix(c.Message, " It keeps the --client-config "+filepath.Join(dir, "profiles", "other.toml")+" it was installed with.") {
		t.Fatalf("an entry from the same profile at another path: %+v", c)
	}
}

// The authority is never taken from the entry: each mismatch fails and
// names the flag, with values only where they aren't a principal.
func TestMCPEntryAuthorityMismatchFailsNamingTheFlag(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*MCPConfig)
		want   string
	}{
		"workspace": {func(m *MCPConfig) { m.Workspace = "other" }, "differs in --workspace (entry acme, expected other)"},
		"project":   {func(m *MCPConfig) { m.Project = "q" }, "differs in --project (entry p, expected q)"},
		"session":   {func(m *MCPConfig) { m.Session = "bob" }, "differs in --session (entry agent, expected bob)"},
		"instance":  {func(m *MCPConfig) { m.Instance = "inst_2" }, "differs in --instance (entry inst_1, expected inst_2)"},
		"server":    {func(m *MCPConfig) { m.Server = "https://other.example.test" }, "differs in --server (entry https://cb.example.test, expected https://other.example.test)"},
		"principal": {func(m *MCPConfig) { m.Principal = "bob@example.test" }, "differs in --principal"},
		"mode":      {func(m *MCPConfig) { m.Mode, m.DB = "local", "/state/local.db" }, "differs in --mode (entry cloud, expected local)"},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := fieldEntry(t, nil)
			tc.mutate(&m)
			c := mcpCheck(t, m)
			if c.Status != Fail || !strings.Contains(c.Message, tc.want) {
				t.Fatalf("want a failure naming %q: %+v", tc.want, c)
			}
			if strings.Contains(c.Message, "@example.test") {
				t.Fatalf("principal echoed: %s", c.Message)
			}
		})
	}
	// Device keys are the only sign-in since v1.0, so doctor always expects
	// device: an entry naming another credential source (an old Google one)
	// is a mismatch.
	m, _ := fieldEntry(t, map[string]string{"--credential-source": "oauth"})
	if c := mcpCheck(t, m); c.Status != Fail || !strings.Contains(c.Message, "differs in --credential-source (entry oauth, expected device)") {
		t.Fatalf("an entry with another credential source: %+v", c)
	}
}

// A detail that would be taken from the entry but is malformed fails,
// naming the flag and never echoing the value.
func TestMCPEntryMalformedLocalDetailFails(t *testing.T) {
	for flag, value := range map[string]string{
		"--cache":          "relative/cache.db",
		"--client-config":  "/state/../profiles/other.toml",
		"--device-id":      "dev_not-a-device",
		"--key-store":      "vault",
		"--linear-profile": "lin bad/value",
		"--instance":       "-bad instance",
	} {
		t.Run(flag, func(t *testing.T) {
			m, _ := fieldEntry(t, map[string]string{flag: value})
			c := mcpCheck(t, m)
			if c.Status != Fail || c.Message != "The installed MCP entry has a malformed "+flag+"." {
				t.Fatalf("malformed %s: %+v", flag, c)
			}
		})
	}
}
