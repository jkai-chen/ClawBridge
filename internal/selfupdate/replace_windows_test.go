//go:build windows

package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// On real Windows a running binary can't be overwritten, but it can be
// renamed: the update sets the running clawbridge.exe aside and puts the new
// one in its place while the old one still runs (a `clawbridge mcp`).
func TestWindowsReplacesARunningBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nimport \"time\"\n\nfunc main() { time.Sleep(time.Minute) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "clawbridge.exe")
	build := exec.Command("go", "build", "-o", dest, src)
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the stand-in binary: %v: %s", err, out)
	}
	running := exec.Command(dest)
	if err := running.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = running.Process.Kill()
		_, _ = running.Process.Wait()
	})
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(dest, []byte("overwrite"), 0o755); err == nil {
		t.Fatal("Windows let a running binary be overwritten; the rename-aside isn't needed")
	}
	staged := filepath.Join(dir, ".clawbridge-update-1.exe")
	if err := os.WriteFile(staged, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	aside := asidePath(t.TempDir(), dest)
	if err := replaceWindows(staged, dest, aside); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "new binary" {
		t.Fatalf("the new binary isn't in place: %q %v", got, err)
	}
	if _, err := os.Stat(aside); err != nil {
		t.Fatal("the running binary wasn't set aside:", err)
	}
}
