package integration

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestReleasePOSIXInstallerIndependent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell probe; native PowerShell is covered separately on Windows")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable for fake transport subprocess harness")
	}
	script, err := filepath.Abs("../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "release_fixtures/install_posix_probe.py", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("independent POSIX installer: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
