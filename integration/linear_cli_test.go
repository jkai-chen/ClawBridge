package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestL1CLIHelpAndInvalidOptionsBeforeStateOrNativeAccess(t *testing.T) {
	p := newProcessFixture(t)
	state := filepath.Join(p.dir, "state")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	poison := []byte("not-valid-toml = [\nsynthetic-secret-MUST-NOT-ECHO\n")
	config := filepath.Join(state, "config.toml")
	if err := os.WriteFile(config, poison, 0600); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(t.TempDir(), "empty-path")
	if err := os.Mkdir(emptyPath, 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for _, v := range p.env {
		if strings.HasPrefix(v, "HOME=") || strings.HasPrefix(v, "PATH=") || strings.HasPrefix(v, "XDG_") || strings.HasPrefix(v, "DBUS_") {
			continue
		}
		env = append(env, v)
	}
	env = append(env, "HOME="+filepath.Join(p.dir, "unused-home"), "XDG_CONFIG_HOME="+filepath.Join(p.dir, "unused-xdg"), "PATH="+emptyPath, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(p.dir, "absent-bus"))
	commands := []string{"connect", "login", "import", "profiles", "status", "logout", "teams", "projects", "binding", "link", "unlink", "context", "issues", "issue", "states", "create", "update", "comment"}
	for _, command := range commands {
		t.Run("help/"+command, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, p.binary, "linear", command, "--help")
			cmd.Dir = p.dir
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil || len(out) == 0 {
				t.Fatalf("native command help failed before poison config: %s err=%v", command, err)
			}
			if strings.Contains(string(out), "synthetic-secret-MUST-NOT-ECHO") {
				t.Fatal("help reflected config contents")
			}
		})
	}
	invalid := [][]string{
		{"linear", "import", "--name", "synthetic", "--token", "synthetic-argv-token"},
		{"linear", "import", "--name", "synthetic"},
		{"linear", "connect", "--access", "admin", "--name", "synthetic"},
		{"linear", "issues", "--profile", l1ProfileID, "--limit", "0"},
		{"linear", "create", "--profile", l1ProfileID, "--title", "synthetic"},
		{"linear", "update", l1IssueID, "--profile", l1ProfileID, "--operation-id", "no-fields"},
		{"linear", "link", "--profile", l1ProfileID, "--team", l1TeamA, "--default-team", l1TeamA, "--team-board"},
	}
	for n, args := range invalid {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		cmd := exec.CommandContext(ctx, p.binary, args...)
		cmd.Dir = p.dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil || len(out) == 0 {
			t.Fatalf("invalid options case%d accepted or no diagnostic", n)
		}
		if strings.Contains(string(out), "synthetic-secret-MUST-NOT-ECHO") || strings.Contains(string(out), "synthetic-argv-token") {
			t.Fatal("invalid option handling reflected secret/config body")
		}
	}
	b, err := os.ReadFile(config)
	if err != nil || string(b) != string(poison) {
		t.Fatal("help/invalid options changed config sentinel")
	}
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.toml" {
		t.Fatal("help/invalid options created profile/ledger/state files")
	}
	for _, path := range []string{p.db, filepath.Join(p.dir, "unused-home"), filepath.Join(p.dir, "unused-xdg")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("help/invalid options initialized authority or alternate config storage")
		}
	}
}
