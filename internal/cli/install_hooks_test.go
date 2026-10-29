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
	// The POSIX text on unix, the exec form on Windows.
	marker, stop := ctxwatch.HookMarker, "hook stop --from-hook"
	if runtime.GOOS == "windows" {
		marker, stop = ctxwatch.HookTag, `"--hook-tag"`
	}
	if strings.Count(string(data), marker) != 3 || !strings.Contains(string(data), stop) {
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

// doctor reads the installed hook form and the Claude Code version, and a
// missing, failing or slow claude only leaves the version unknown.
func TestDoctorHooksReadsFormAndVersion(t *testing.T) {
	dir := isolated(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	project := filepath.Join(dir, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	version := hooksClaudeVersion
	t.Cleanup(func() { hooksClaudeVersion = version })
	hooksClaudeVersion = func(context.Context) (string, bool) { return "9.9.9", true }
	if st := doctorHooks(context.Background()); st.Form != "" || st.Err != nil || st.Version != "" {
		t.Fatalf("%+v", st)
	}
	settings := filepath.Join(project, ".claude", "settings.local.json")
	realGOOS, realExe := contextGOOS, hookExecutable
	t.Cleanup(func() { contextGOOS, hookExecutable = realGOOS, realExe })
	contextGOOS = "windows"
	hookExecutable = func() (string, error) { return `C:\bin\clawbridge.exe`, nil }
	if err := applyHooks(ctxwatch.ScopeLocal, project, true, false, false, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	st := doctorHooks(context.Background())
	if st.Form != ctxwatch.FormExec || st.Version != "9.9.9" || st.GOOS != "windows" || len(st.Paths) != 1 || !strings.HasSuffix(st.Paths[0], "settings.local.json") {
		t.Fatalf("%+v", st)
	}
	hooksClaudeVersion = func(context.Context) (string, bool) { return "", false }
	if st = doctorHooks(context.Background()); st.Form != ctxwatch.FormExec || st.Version != "" {
		t.Fatalf("%+v", st)
	}
	if err := os.WriteFile(settings, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st = doctorHooks(context.Background()); st.Err == nil {
		t.Fatalf("%+v", st)
	}
}
