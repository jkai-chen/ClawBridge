package ctxwatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const bin = "/opt/claw bridge/clawbridge"

func TestHookCommand(t *testing.T) {
	local, err := HookCommand(ScopeLocal, bin, "bind")
	if err != nil || local != `[ -x '/opt/claw bridge/clawbridge' ] || exit 0; CLAWBRIDGE_AGENT_PID=$PPID '/opt/claw bridge/clawbridge' context bind --from-hook `+HookMarker {
		t.Fatalf("%q %v", local, err)
	}
	shared, err := HookCommand(ScopeProjectShared, "", "compacting")
	if err != nil || shared != "command -v clawbridge >/dev/null 2>&1 || exit 0; CLAWBRIDGE_AGENT_PID=$PPID clawbridge context compacting --from-hook "+HookMarker {
		t.Fatalf("%q %v", shared, err)
	}
	for name, c := range map[string]struct{ scope, binary string }{
		"relative": {ScopeUser, "clawbridge"},
		"quote":    {ScopeLocal, "/opt/it's/clawbridge"},
		"newline":  {ScopeLocal, "/opt/x\n/clawbridge"},
		"scope":    {"project", bin},
	} {
		if got, err := HookCommand(c.scope, c.binary, "bind"); err == nil {
			t.Fatalf("%s: %q", name, got)
		}
	}
	for _, own := range []string{local, shared} {
		if !isOwnHook(own) {
			t.Fatalf("not recognised: %q", own)
		}
	}
	// Only exactly ClawBridge's text: a similar hook of the user's stays.
	for _, other := range []string{
		strings.TrimSuffix(local, " "+HookMarker),
		local + " && echo hi",
		"echo pwned; " + local,
		`[ -x '/a/clawbridge' ] || exit 0; CLAWBRIDGE_AGENT_PID=$PPID '/b/clawbridge' context bind --from-hook ` + HookMarker,
		`[ -x '/a/clawbridge' ] || exit 0; '/a/clawbridge' context bind --from-hook ` + HookMarker,
		`[ -x 'rel/clawbridge' ] || exit 0; CLAWBRIDGE_AGENT_PID=$PPID 'rel/clawbridge' context bind --from-hook ` + HookMarker,
		`[ -x '/a/clawbridge' ] || exit 0; CLAWBRIDGE_AGENT_PID=$PPID '/a/clawbridge' context show --from-hook ` + HookMarker,
		"clawbridge context bind --from-hook " + HookMarker,
	} {
		if isOwnHook(other) {
			t.Fatalf("recognised as ClawBridge's: %q", other)
		}
	}
}

func settingsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".claude", "settings.local.json")
	if content != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func apply(t *testing.T, path, scope, binary string, install bool) HookChange {
	t.Helper()
	c, err := PlanHooks(path, scope, binary, install)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestInstallAndUninstallHooks(t *testing.T) {
	user := `{
  "model": "x",
  "permissions": {"allow": ["Bash(ls)"]},
  "hooks": {
    "SessionStart": [{"matcher": "compact", "hooks": [{"type": "command", "command": "~/bin/my-reminder.sh"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "say done"}]}]
  },
  "big": 12345678901234567890
}
`
	path := settingsFile(t, user)
	c := apply(t, path, ScopeLocal, bin, true)
	if !c.Changed() || !strings.Contains(c.Diff(), "+ ") {
		t.Fatalf("no change: %s", c.Diff())
	}
	data, _ := os.ReadFile(path)
	// Commands keep their characters; numbers keep their digits.
	if strings.Contains(string(data), `\u00`) || !strings.Contains(string(data), "12345678901234567890") {
		t.Fatalf("%s", data)
	}
	var got struct {
		Model string
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct {
				Type, Command string
				Timeout       int
			}
		}
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	bindCmd, _ := HookCommand(ScopeLocal, bin, "bind")
	compactCmd, _ := HookCommand(ScopeLocal, bin, "compacting")
	ss, pc := got.Hooks["SessionStart"], got.Hooks["PreCompact"]
	if got.Model != "x" || len(got.Hooks["Stop"]) != 1 || len(ss) != 2 || ss[0].Hooks[0].Command != "~/bin/my-reminder.sh" ||
		ss[1].Matcher != "" || ss[1].Hooks[0].Command != bindCmd || ss[1].Hooks[0].Timeout != hookTimeout ||
		len(pc) != 1 || pc[0].Hooks[0].Command != compactCmd {
		t.Fatalf("%s", data)
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", info.Mode())
	}
	// Installing again changes nothing, even with a user hook added after.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	hooks := raw["hooks"].(map[string]any)
	hooks["SessionStart"] = append(hooks["SessionStart"].([]any), map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "later"}}})
	withLater, _ := json.Marshal(raw)
	if err := os.WriteFile(path, withLater, 0o644); err != nil {
		t.Fatal(err)
	}
	if c := apply(t, path, ScopeLocal, bin, true); c.Changed() {
		t.Fatalf("reinstall changed:\n%s", c.Diff())
	}
	// A moved binary replaces the old entries rather than adding to them.
	apply(t, path, ScopeLocal, "/usr/local/bin/clawbridge", true)
	data, _ = os.ReadFile(path)
	if strings.Count(string(data), HookMarker) != 2 || strings.Contains(string(data), bin) {
		t.Fatalf("%s", data)
	}
	// Uninstall leaves the user's own settings and hooks as they were.
	apply(t, path, ScopeLocal, "", false)
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), HookMarker) || strings.Contains(string(data), "PreCompact") ||
		!strings.Contains(string(data), "my-reminder.sh") || !strings.Contains(string(data), "later") {
		t.Fatalf("%s", data)
	}
	if c := apply(t, path, ScopeLocal, "", false); c.Changed() {
		t.Fatal("a second uninstall changed the file")
	}
}

func TestHooksOnANewFile(t *testing.T) {
	path := settingsFile(t, "")
	if c, err := PlanHooks(path, ScopeLocal, bin, false); err != nil || c.Changed() {
		t.Fatalf("uninstall without a file: %v %v", c.Changed(), err)
	}
	apply(t, path, ScopeProjectShared, "", true)
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("%v %v", info, err)
	}
	// The command is written as it runs, not with \u escapes.
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), ">/dev/null 2>&1 || exit 0") {
		t.Fatalf("%s", data)
	}
	apply(t, path, ScopeLocal, "", false)
	if data, _ := os.ReadFile(path); strings.TrimSpace(string(data)) != "{}" {
		t.Fatalf("%s", data)
	}
}

func TestHooksRefuseWhatTheyCantEdit(t *testing.T) {
	for name, content := range map[string]string{
		"not JSON":      "{",
		"a list":        "[]",
		"two values":    "{} {}",
		"hooks list":    `{"hooks": []}`,
		"event object":  `{"hooks": {"SessionStart": {}}}`,
		"trailing text": `{"a": 1} x`,
	} {
		if _, err := PlanHooks(settingsFile(t, content), ScopeLocal, bin, true); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	real := settingsFile(t, "{}")
	link := filepath.Join(t.TempDir(), "settings.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanHooks(link, ScopeLocal, bin, true); err == nil {
		t.Fatal("a link was accepted")
	}
	// A file changed between reading and writing is not overwritten.
	c, err := PlanHooks(real, ScopeLocal, bin, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(`{"model":"y"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(); err == nil {
		t.Fatal("overwrote a changed file")
	}
}
