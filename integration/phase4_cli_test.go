package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/install"
)

func TestPhase4CLIInstallerPinsTrustedOAuthWithoutSecrets(t *testing.T) {
	p := newProcessFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "trusted.toml")
	target := filepath.Join(dir, "agent.json")
	const browserSecret = "synthetic-P4-browser-secret"
	const deviceSecret = "synthetic-P4-device-secret"
	raw := []byte("mode='cloud'\nserver='https://bus.example.test'\naudience='browser.apps.googleusercontent.com'\ncredential_source='oauth'\noauth_client_id='browser.apps.googleusercontent.com'\noauth_client_secret='" + browserSecret + "'\noauth_device_client_id='device.apps.googleusercontent.com'\noauth_device_client_secret='" + deviceSecret + "'\nprincipal='member@example.test'\nworkspace='ws_test'\nproject='prj_test'\nsession='operator'\ninstance='operator-instance'\ncache=" + quoteTOML(filepath.Join(dir, "cache.db")) + "\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Explicit selection must bypass both an invalid env-selected config and repository.
	bad := filepath.Join(dir, "bad.toml")
	for _, name := range []string{bad, filepath.Join(dir, ".clawbridge.toml")} {
		if err := os.WriteFile(name, []byte("broken=["), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, explicitSession := range []bool{false, true} {
		args := []string{"mcp", "install", "--client-config", path, "--agent", "claude-code", "--config", target, "--command", p.binary, "--dry-run"}
		if explicitSession {
			args = append(args, "--session", "distinct-agent")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, p.binary, args...)
		cmd.Dir, cmd.Env = dir, []string{"HOME=" + dir, "PATH=" + dir, "CLAWBRIDGE_HOME=" + filepath.Join(dir, "state"), "CLAWBRIDGE_CONFIG=" + bad}
		// Standard output is the result; mcp install reports the hooks it
		// adds on standard error. Neither shows a secret.
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		cancel()
		all := string(out) + stderr.String()
		if strings.Contains(all, browserSecret) || strings.Contains(all, deviceSecret) {
			t.Fatal("installer exposed app credentials (values suppressed)")
		}
		if !explicitSession {
			if err == nil || !strings.Contains(all, "explicit") {
				t.Fatal("installer inherited operator actor")
			}
			continue
		}
		if err != nil {
			t.Fatalf("install inherited OAuth route: %v: %s", err, all)
		}
		var result install.Result
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result.Entry)
		for _, want := range []string{path, "distinct-agent", "ws_test", "prj_test", "cloud", "oauth"} {
			if !strings.Contains(string(encoded), want) {
				t.Errorf("installed route omitted %q", want)
			}
		}
		for _, forbidden := range []string{"operator-instance", "--oauth-client-secret", "--oauth-device-client-secret", "--db"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Errorf("installed route contains forbidden %q", forbidden)
			}
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Error("installation changed client route")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("dry-run wrote agent configuration")
	}
}

func TestPhase4CLIRejectsBeforeAuthorityOrStorage(t *testing.T) {
	p := newProcessFixture(t)
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "synthetic service must not be contacted", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name string
		args []string
		want string
		help bool
	}{
		{"relative-config", []string{"--client-config", "relative.toml", "whoami"}, "absolute", false},
		{"empty-config", []string{"--client-config=", "whoami"}, "nonempty", false},
		{"duplicate-config", []string{"--client-config", "/first.toml", "whoami", "--client-config", "/second.toml"}, "once", false},
		{"missing-explicit-server", []string{"setup", "join", "--workspace", "w", "--project", "p"}, "explicit", false},
		{"missing-explicit-workspace", []string{"setup", "join", "--server", srv.URL, "--project", "p"}, "explicit", false},
		{"missing-explicit-project", []string{"setup", "join", "--server", srv.URL, "--workspace", "w"}, "explicit", false},
		{"conflicting-acquisition", []string{"setup", "join", "--login", "--device", "--server", srv.URL, "--workspace", "w", "--project", "p"}, "either", false},
		{"actor-without-instance", []string{"setup", "join", "--server", srv.URL, "--workspace", "w", "--project", "p", "--session", "actor"}, "together", false},
		{"join-positional", []string{"setup", "join", "unrequested", "--server", srv.URL, "--workspace", "w", "--project", "p"}, "flags only", false},
		{"join-help", []string{"setup", "join", "--help"}, "", true},
		{"tui-help", []string{"tui", "--help"}, "", true},
		{"tui-nonterminal", []string{"tui"}, "terminal", false},
		{"doctor-partial-selection", []string{"doctor", "--mcp-agent", "codex"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			// Inherited scope and route must not satisfy join's explicit-input gate.
			env := []string{"HOME=" + dir, "CLAWBRIDGE_HOME=" + state, "PATH=" + dir, "TERM=dumb",
				"CLAWBRIDGE_SERVER=" + srv.URL, "CLAWBRIDGE_WORKSPACE=inherited", "CLAWBRIDGE_PROJECT=inherited"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, p.binary, tc.args...)
			cmd.Dir, cmd.Env = dir, env
			before := requests.Load()
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("preflight/help did not return promptly")
			}
			if tc.help && err != nil || !tc.help && err == nil {
				t.Fatalf("unexpected exit: %v: %s", err, out)
			}
			if tc.want != "" && !strings.Contains(strings.ToLower(string(out)), tc.want) {
				t.Fatalf("missing actionable %q diagnostic: %s", tc.want, out)
			}
			if requests.Load() != before {
				t.Error("validation/help contacted authority")
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Error("validation/help created state directory")
			}
		})
	}
}
