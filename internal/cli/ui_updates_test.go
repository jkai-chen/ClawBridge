package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/guibackend"
	"clawbridge/internal/selfupdate"
)

// updates.check is refused under an agent marker, and then makes no request.
func TestUIUpdatesCheckRefusedUnderAgentMarker(t *testing.T) {
	old := uiUpdateCheck
	defer func() { uiUpdateCheck = old }()
	calls := 0
	uiUpdateCheck = func(context.Context) (selfupdate.CheckResult, error) {
		calls++
		return selfupdate.CheckResult{Plan: selfupdate.Plan{Action: selfupdate.UpToDate}}, nil
	}
	cfg := uiUpdatesConfig(&uiInputs{})
	t.Setenv("CLAUDECODE", "1")
	if _, err := guibackend.UpdatesCheck(cfg).Call(context.Background(), nil); err == nil {
		t.Fatal("updates.check ran under CLAUDECODE")
	}
	if calls != 0 {
		t.Fatalf("the check ran %d times", calls)
	}
}

func TestUIUpdatesCheckRunsForAPerson(t *testing.T) {
	old := uiUpdateCheck
	defer func() { uiUpdateCheck = old }()
	calls := 0
	uiUpdateCheck = func(context.Context) (selfupdate.CheckResult, error) {
		calls++
		return selfupdate.CheckResult{Plan: selfupdate.Plan{Action: selfupdate.UpToDate}}, nil
	}
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		if name == "CLAUDECODE" || strings.HasPrefix(name, "CODEX_") || name == "CLAWBRIDGE_SESSION" || name == "CLAWBRIDGE_INSTANCE" {
			t.Setenv(name, val) // restores the value when the test ends
			os.Unsetenv(name)
		}
	}
	cfg := uiUpdatesConfig(&uiInputs{})
	if _, err := guibackend.UpdatesCheck(cfg).Call(context.Background(), nil); err != nil || calls != 1 {
		t.Fatalf("err %v, calls %d", err, calls)
	}
}

func TestDiskVersionEnvIsMinimal(t *testing.T) {
	env := diskVersionEnv([]string{"PATH=/bin", "HOME=/h", "CLAWBRIDGE_TOKEN=secret", "LINEAR_API_KEY=k", "AWS_SECRET_ACCESS_KEY=x", "CLAUDECODE=1"})
	want := map[string]bool{"CLAWBRIDGE_OUTPUT=json": true, "PATH=/bin": true, "HOME=/h": true}
	if len(env) != len(want) {
		t.Fatalf("env = %v", env)
	}
	for _, kv := range env {
		if !want[kv] {
			t.Errorf("unexpected %q", kv)
		}
	}
}

func TestDiskVersionIsCachedByPathAndMtime(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "clawbridge")
	if err := os.WriteFile(exe, []byte("one"), 0o755); err != nil {
		t.Fatal(err)
	}
	runs := 0
	run := func(context.Context, string) (string, error) { runs++; return "v1.0." + strconv.Itoa(runs), nil }
	stat := func() os.FileInfo {
		i, err := os.Stat(exe)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	var c diskVersions
	for i := 0; i < 3; i++ {
		if v, err := c.get(context.Background(), exe, stat(), run); err != nil || v != "v1.0.1" {
			t.Fatalf("v %q err %v", v, err)
		}
	}
	if runs != 1 {
		t.Fatalf("ran %d times for an unchanged file", runs)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(exe, later, later); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.get(context.Background(), exe, stat(), run); v != "v1.0.2" || runs != 2 {
		t.Fatalf("a changed file kept the old answer: %q after %d runs", v, runs)
	}
}
