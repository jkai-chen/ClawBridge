package doctor

import (
	"errors"
	"strings"
	"testing"
)

func hooksCheck(t *testing.T, s HooksState) (Check, bool) {
	t.Helper()
	var got []Check
	checkHooks(s, func(c Check) { got = append(got, c) })
	if len(got) > 1 {
		t.Fatal(got)
	}
	if len(got) == 0 {
		return Check{}, false
	}
	return got[0], true
}

// The hooks check names the installed form and the Claude Code version, and
// on Windows with the exec form warns until the version is probed. It never
// fails.
func TestCheckHooks(t *testing.T) {
	if _, ok := hooksCheck(t, HooksState{GOOS: "linux"}); ok {
		t.Fatal("a check with no hooks installed")
	}
	c, _ := hooksCheck(t, HooksState{GOOS: "linux", Form: "shell", Version: "9.9.9", Paths: []string{"/p/settings.json"}})
	if c.ID != "hooks" || c.Status != Pass || !strings.Contains(c.Message, "shell form") || !strings.Contains(c.Message, "Claude Code 9.9.9") || c.Path != "/p/settings.json" {
		t.Fatalf("%+v", c)
	}
	c, _ = hooksCheck(t, HooksState{GOOS: "linux", Form: "exec"})
	if c.Status != Pass || !strings.Contains(c.Message, "exec form") || !strings.Contains(c.Message, "version is unknown") {
		t.Fatalf("%+v", c)
	}
	c, _ = hooksCheck(t, HooksState{GOOS: "windows", Form: "exec", Version: "9.9.9"})
	if c.Status != Warn || !strings.Contains(c.Message, "pending the Windows probe") || !strings.Contains(c.Message, "exec form") || c.Action == "" {
		t.Fatalf("%+v", c)
	}
	// The shell form can't work on Windows, alone or as half of a mix, even
	// for a probed version.
	for _, form := range []string{"shell", "mixed"} {
		c, _ = hooksCheck(t, HooksState{GOOS: "windows", Form: form, Version: "9.9.9"})
		if c.Status != Warn || !strings.Contains(c.Message, "POSIX shell") || !strings.Contains(c.Action, "clawbridge mcp install") {
			t.Fatalf("%s: %+v", form, c)
		}
	}
	// A probed version passes.
	old := WindowsProbedVersions
	WindowsProbedVersions = []string{"9.9.9"}
	t.Cleanup(func() { WindowsProbedVersions = old })
	if c, _ = hooksCheck(t, HooksState{GOOS: "windows", Form: "exec", Version: "9.9.9"}); c.Status != Pass {
		t.Fatalf("%+v", c)
	}
	if c, _ = hooksCheck(t, HooksState{GOOS: "windows", Form: "shell", Version: "9.9.9"}); c.Status != Warn {
		t.Fatalf("%+v", c)
	}
	if c, _ = hooksCheck(t, HooksState{GOOS: "windows", Form: "exec", Version: "1.0.0"}); c.Status != Warn {
		t.Fatalf("%+v", c)
	}
	c, _ = hooksCheck(t, HooksState{GOOS: "linux", Err: errors.New("the settings file x is not readable")})
	if c.Status != Warn || !strings.Contains(c.Message, "not readable") {
		t.Fatalf("%+v", c)
	}
}

func TestWindowsProbedVersionsStartEmpty(t *testing.T) {
	if len(WindowsProbedVersions) != 0 {
		t.Fatal("a version is listed before the probe is recorded")
	}
}
