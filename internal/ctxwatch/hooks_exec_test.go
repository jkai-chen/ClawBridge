package ctxwatch

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The exec form is built for a given GOOS, so every platform tests it.
const winBin = `C:\Program Files\claw bridge\clawbridge.exe`

func TestHookEntryExecForm(t *testing.T) {
	got, err := HookEntry("windows", ScopeLocal, winBin, "stop")
	want := map[string]any{"type": "command", "command": winBin, "timeout": hookTimeout,
		"args": []any{"hook", "stop", "--from-hook", "--hook-tag", "clawbridge-context-hook-v1"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v %v", got, err)
	}
	got, err = HookEntry("windows", ScopeUser, winBin, "compacting")
	if err != nil || !reflect.DeepEqual(got["args"], []any{"context", "compacting", "--from-hook", "--hook-tag", HookTag}) {
		t.Fatalf("%v %v", got, err)
	}
	got, err = HookEntry("windows", ScopeProjectShared, "", "bind")
	if err != nil || got["command"] != "clawbridge" || !reflect.DeepEqual(got["args"], []any{"context", "bind", "--from-hook", "--hook-tag", HookTag}) {
		t.Fatalf("%v %v", got, err)
	}
	for name, c := range map[string]struct{ scope, binary, sub string }{
		"relative":   {ScopeUser, "clawbridge.exe", "bind"},
		"unix path":  {ScopeUser, "/usr/bin/clawbridge.exe", "bind"},
		"not exe":    {ScopeLocal, `C:\bin\clawbridge`, "bind"},
		"quote":      {ScopeLocal, `C:\bin\"x\clawbridge.exe`, "bind"},
		"newline":    {ScopeLocal, "C:\\bin\\x\n\\clawbridge.exe", "bind"},
		"cr":         {ScopeLocal, "C:\\bin\\x\r\\clawbridge.exe", "bind"},
		"drive only": {ScopeLocal, `C:clawbridge.exe`, "bind"},
		"scope":      {"project", winBin, "bind"},
		"sub":        {ScopeLocal, winBin, "other"},
	} {
		if got, err := HookEntry("windows", c.scope, c.binary, c.sub); err == nil {
			t.Errorf("%s: %v", name, got)
		}
	}
	// Elsewhere the entry is the POSIX text, unchanged.
	posix, err := HookEntry("linux", ScopeLocal, bin, "stop")
	text, _ := HookCommand(ScopeLocal, bin, "stop")
	if err != nil || posix["command"] != text || posix["args"] != nil {
		t.Fatalf("%v %v", posix, err)
	}
}

// entryOf round-trips an entry through JSON, as a settings file does.
func entryOf(t *testing.T, e map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(e)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOwnEntryExactness(t *testing.T) {
	for _, sub := range []string{"bind", "compacting", "stop"} {
		for _, c := range []struct{ goos, scope, binary string }{
			{"windows", ScopeLocal, winBin}, {"windows", ScopeUser, `\\host\share\clawbridge.exe`},
			{"windows", ScopeProjectShared, ""}, {"linux", ScopeLocal, bin}, {"linux", ScopeProjectShared, ""},
		} {
			e, err := HookEntry(c.goos, c.scope, c.binary, sub)
			if err != nil {
				t.Fatal(err)
			}
			if ownEntryKey(entryOf(t, e)) == "" {
				t.Errorf("not recognised: %v", e)
			}
		}
	}
	args := func(a ...any) map[string]any {
		return map[string]any{"type": "command", "command": winBin, "args": a}
	}
	posix, _ := HookCommand(ScopeLocal, bin, "stop")
	for name, e := range map[string]map[string]any{
		"extra arg":    args("hook", "stop", "--from-hook", "--hook-tag", HookTag, "--x"),
		"no tag":       args("hook", "stop", "--from-hook"),
		"other tag":    args("hook", "stop", "--from-hook", "--hook-tag", "other"),
		"other sub":    args("hook", "start", "--from-hook", "--hook-tag", HookTag),
		"swapped":      args("--from-hook", "hook", "stop", "--hook-tag", HookTag),
		"non-string":   args("hook", 3, "--from-hook", "--hook-tag", HookTag),
		"relative":     {"command": "clawbridge.exe", "args": []any{"hook", "stop", "--from-hook", "--hook-tag", HookTag}},
		"not exe":      {"command": `C:\bin\clawbridge`, "args": []any{"hook", "stop", "--from-hook", "--hook-tag", HookTag}},
		"posix + args": {"command": posix, "args": []any{}},
		"no command":   {"args": []any{"hook", "stop", "--from-hook", "--hook-tag", HookTag}},
	} {
		if k := ownEntryKey(e); k != "" {
			t.Errorf("%s recognised as ClawBridge's", name)
		}
	}
}

func readSettings(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func planFor(t *testing.T, goos, path, scope, binary string, install bool) HookChange {
	t.Helper()
	c, err := PlanHooksFor(goos, path, scope, binary, install)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Apply(); err != nil {
		t.Fatal(err)
	}
	return c
}

// A Windows install is idempotent, keeps the user's own hooks, switches a
// POSIX install to the exec form without duplicates, and uninstalls both.
func TestExecFormInstallReinstallUninstall(t *testing.T) {
	mine := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo mine"}]}]},"model":"x"}`
	path := settingsFile(t, mine)
	planFor(t, "windows", path, ScopeLocal, winBin, true)
	first := readSettings(t, path)
	if n := strings.Count(first, HookTag); n != 3 {
		t.Fatalf("%d tags:\n%s", n, first)
	}
	if !strings.Contains(first, "echo mine") || !strings.Contains(first, `"model": "x"`) {
		t.Fatalf("user settings lost:\n%s", first)
	}
	if form, err := InstalledHookForm(path); err != nil || form != FormExec {
		t.Fatal(form, err)
	}
	if c := planFor(t, "windows", path, ScopeLocal, winBin, true); c.Changed() {
		t.Fatal("a second install changed the file")
	}
	// A different binary path replaces the entries.
	other := `D:\tools\clawbridge.exe`
	planFor(t, "windows", path, ScopeLocal, other, true)
	if s := readSettings(t, path); strings.Count(s, HookTag) != 3 || strings.Contains(s, "claw bridge") || !strings.Contains(s, `D:\\tools\\clawbridge.exe`) {
		t.Fatalf("%s", s)
	}
	// A POSIX install then an exec install: replaced, never both.
	planFor(t, "linux", path, ScopeLocal, bin, true)
	if form, _ := InstalledHookForm(path); form != FormShell || strings.Count(readSettings(t, path), HookMarker) != 3 {
		t.Fatalf("%s", readSettings(t, path))
	}
	planFor(t, "windows", path, ScopeLocal, winBin, true)
	s := readSettings(t, path)
	if strings.Contains(s, HookMarker) || strings.Count(s, HookTag) != 3 {
		t.Fatalf("%s", s)
	}
	// Uninstall, on any OS, removes only ClawBridge's.
	planFor(t, "linux", path, ScopeLocal, "", false)
	s = readSettings(t, path)
	if strings.Contains(s, HookTag) || !strings.Contains(s, "echo mine") {
		t.Fatalf("%s", s)
	}
	if form, _ := InstalledHookForm(path); form != "" {
		t.Fatal(form)
	}
}

func TestInstalledHookFormMixedAndMissing(t *testing.T) {
	if form, err := InstalledHookForm(settingsFile(t, "")); err != nil || form != "" {
		t.Fatal(form, err)
	}
	path := settingsFile(t, "{")
	if _, err := InstalledHookForm(path); err == nil {
		t.Fatal("bad JSON accepted")
	}
	path = settingsFile(t, "{}")
	planFor(t, "linux", path, ScopeLocal, bin, true)
	// Add one exec entry beside the POSIX ones.
	e, _ := HookEntry("windows", ScopeProjectShared, "", "stop")
	b, _ := json.Marshal(map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{e}}}}})
	other := settingsFile(t, string(b))
	if form, _ := InstalledHookForm(other); form != FormExec {
		t.Fatal(form)
	}
	if form, _ := InstalledHookForm(path); form != FormShell {
		t.Fatal(form)
	}
}
