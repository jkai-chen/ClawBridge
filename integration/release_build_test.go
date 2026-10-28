package integration

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestReleaseBuilderIndependent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX build script fixture; release build gates run on Ubuntu")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable for isolated release clone fixture")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// A cold CI build takes over 3 minutes (209 s on 28 Oct): headroom.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "release_fixtures/build_probe.py", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("independent release builder: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}

func TestReleaseBuilderRejectsExternalSourceOverrides(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX build script fixture; release build gates run on Ubuntu")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable for isolated source override fixture")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "release_fixtures/build_override_probe.py", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("release source overrides: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
