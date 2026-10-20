package install

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func options(t *testing.T, agent string) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{Agent: agent, Config: filepath.Join(dir, "config"), Command: filepath.Join(dir, "clawbridge"), DB: filepath.Join(dir, "bus.db"), Workspace: "team", Project: "app", Session: "reviewer"}
}

func TestPreserveExistingJSONAndIdempotency(t *testing.T) {
	o := options(t, "claude-code")
	before := []byte(`{"counter":9007199254740993,"mcpServers":{"other":{"command":"other","args":["x"]}},"settings":{"enabled":true}}`)
	if err := os.WriteFile(o.Config, before, 0640); err != nil {
		t.Fatal(err)
	}
	r, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed || r.Backup == "" {
		t.Fatalf("missing change/backup: %+v", r)
	}
	backup, err := os.ReadFile(r.Backup)
	if err != nil || !bytes.Equal(before, backup) {
		t.Fatal("backup did not preserve exact original bytes")
	}
	after, err := os.ReadFile(o.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte("9007199254740993")) {
		t.Fatal("unrelated number changed precision")
	}
	var root map[string]any
	if err = json.Unmarshal(after, &root); err != nil {
		t.Fatal(err)
	}
	servers := root["mcpServers"].(map[string]any)
	if servers["other"].(map[string]any)["command"] != "other" {
		t.Fatal("unrelated server overwritten")
	}
	info, _ := os.Stat(o.Config)
	if info.Mode().Perm() != 0640 {
		t.Fatal("existing file mode lost")
	}
	repeat, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Changed || repeat.Backup != "" {
		t.Fatalf("repeat rewrote configuration: %+v", repeat)
	}
	repeated, _ := os.ReadFile(o.Config)
	if !bytes.Equal(after, repeated) {
		t.Fatal("repeat changed bytes")
	}
}

func TestTOMLAppendPreservesOriginalBytes(t *testing.T) {
	o := options(t, "codex")
	before := []byte("# Keep my comments\nmodel = 'example'\n[mcp_servers.other]\ncommand = 'other'\nargs = ['x']\n")
	if err := os.WriteFile(o.Config, before, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(o.Config)
	if !bytes.HasPrefix(after, before) {
		t.Fatal("original TOML was rewritten")
	}
	r, err := Install(o)
	if err != nil || r.Changed {
		t.Fatalf("repeat: %+v %v", r, err)
	}
}

func TestInvalidConfigAndConflictsLeaveOriginalUntouched(t *testing.T) {
	for _, tc := range []struct{ agent, body string }{
		{"codex", "[broken"}, {"claude-code", "{"}, {"claude-code", "null"},
		{"claude-code", "{} {}"}, {"claude-code", `{"mcpServers":[]}`},
		{"codex", `mcp_servers = 42`},
		{"claude-code", `{"mcpServers":{"clawbridge":{"command":"different"}}}`},
		{"codex", "[mcp_servers.clawbridge]\ncommand = 'different'\n"},
	} {
		t.Run(tc.agent+tc.body, func(t *testing.T) {
			o := options(t, tc.agent)
			before := []byte(tc.body)
			if err := os.WriteFile(o.Config, before, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Install(o); err == nil {
				t.Fatal("expected rejection")
			}
			after, _ := os.ReadFile(o.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected install modified original")
			}
			backups, _ := filepath.Glob(o.Config + ".clawbridge-backup-*")
			if len(backups) != 0 {
				t.Fatal("rejected install created backup")
			}
		})
	}
}

func TestDryRunDoesNotCreateDirectories(t *testing.T) {
	o := options(t, "codex")
	o.Config = filepath.Join(filepath.Dir(o.Config), "missing", "config.toml")
	o.DryRun = true
	r, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed || !r.DryRun {
		t.Fatalf("unexpected preview: %+v", r)
	}
	if _, err = os.Stat(filepath.Dir(o.Config)); !os.IsNotExist(err) {
		t.Fatal("dry-run created directories")
	}
	args := r.Entry["args"].([]string)
	joined := strings.Join(args, " ")
	for _, wanted := range []string{"--workspace team", "--project app", "--session reviewer", "--db"} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("entry missing pinned scope %s: %s", wanted, joined)
		}
	}
}

func TestLockPreventsConcurrentOverwrite(t *testing.T) {
	o := options(t, "codex")
	if err := os.WriteFile(o.Config+".clawbridge.lock", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err == nil {
		t.Fatal("lock ignored")
	}
	if _, err := os.Stat(o.Config); !os.IsNotExist(err) {
		t.Fatal("locked target created")
	}
}

func TestOAuthInstallRequiresSelectedAudienceAndSafeArgv(t *testing.T) {
	o := options(t, "claude-code")
	o.Mode = "cloud"
	o.Server = "https://server.example.test"
	o.Audience = "active-client.apps.googleusercontent.com"
	o.Principal = "alice@example.test"
	o.Cache = filepath.Join(filepath.Dir(o.Config), "cache.db")
	o.CredentialSource = "oauth"
	o.OAuthClientID = "active-client.apps.googleusercontent.com"
	o.OAuthDeviceClientID = "device-client.apps.googleusercontent.com"
	o.OAuthKeyringService = "review-keyring"
	result, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	args := result.Entry["args"].([]string)
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"--audience\x00active-client.apps.googleusercontent.com",
		"--oauth-client-id\x00active-client.apps.googleusercontent.com",
		"--oauth-device-client-id\x00device-client.apps.googleusercontent.com",
		"--oauth-keyring-service\x00review-keyring",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("OAuth installer argv omitted %q: %#v", want, args)
		}
	}
	for _, forbidden := range []string{
		"oauth-client-secret", "oauth-device-client-secret",
		"browser_client_secret", "device_client_secret",
		"refresh_token", "id_token", "access_token",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("OAuth installer argv exposed %q: %#v", forbidden, args)
		}
	}
}
