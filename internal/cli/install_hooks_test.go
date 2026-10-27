package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"clawbridge/internal/ctxwatch"
)

// mcp install gives a Claude Code entry the hooks, in the local settings of
// the entry's project by default, reporting them on standard error so its
// JSON stays alone on standard output; --hooks none leaves them out, and a
// Codex entry never gets them.
func TestMCPInstallAddsHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hooks need a POSIX shell and are not installed on Windows")
	}
	dir := isolated(t)
	project := filepath.Join(dir, "project")
	install := func(extra ...string) (string, string, error) {
		args := append([]string{"--session", "agent", "--workspace", "w", "--project", "p", "mcp", "install", "--agent", "claude-code", "--config", filepath.Join(project, ".mcp.json")}, extra...)
		var out, errs bytes.Buffer
		err := Run(context.Background(), args, &out, &errs)
		return out.String(), errs.String(), err
	}
	settings := filepath.Join(project, ".claude", "settings.local.json")

	out, errs, err := install("--hooks", "none")
	if err != nil || strings.Contains(errs, "settings") {
		t.Fatal(err, errs)
	}
	if _, err = os.Stat(settings); err == nil {
		t.Fatal("--hooks none wrote hooks")
	}
	out, errs, err = install("--name", "second")
	if err != nil {
		t.Fatal(err, errs)
	}
	var result map[string]any
	if json.Unmarshal([]byte(out), &result) != nil || !strings.Contains(errs, settings) || !strings.Contains(errs, "written") {
		t.Fatalf("%q %q", out, errs)
	}
	data, _ := os.ReadFile(settings)
	if strings.Count(string(data), ctxwatch.HookMarker) != 3 || !strings.Contains(string(data), "hook stop --from-hook") {
		t.Fatalf("%s", data)
	}
	if _, errs, err = install("--name", "third", "--hooks", "sideways"); err == nil || !strings.Contains(err.Error(), "--hooks must be") {
		t.Fatal(err, errs)
	}
	codex := []string{"--session", "agent", "--workspace", "w", "--project", "p", "mcp", "install", "--agent", "codex", "--config", filepath.Join(dir, "codex.toml"), "--hooks", "local"}
	if err = Run(context.Background(), codex, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "claude-code") {
		t.Fatal(err)
	}
	codex = codex[:len(codex)-2]
	var errOut bytes.Buffer
	if err = Run(context.Background(), codex, &bytes.Buffer{}, &errOut); err != nil || strings.Contains(errOut.String(), "settings") {
		t.Fatal(err, errOut.String())
	}
}
