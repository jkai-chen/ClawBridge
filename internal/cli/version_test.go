package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"clawbridge/internal/buildinfo"
)

func TestVersionIgnoresInvalidConfigurationAndRejectsExtraArguments(t *testing.T) {
	dir := isolated(t)
	config := filepath.Join(dir, "malformed.toml")
	if err := os.WriteFile(config, []byte("[broken"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWBRIDGE_CONFIG", config)
	t.Setenv("CLAWBRIDGE_MODE", "invalid")
	t.Setenv("CLAWBRIDGE_DB", filepath.Join(dir, "missing", "state.db"))
	t.Setenv("CLAWBRIDGE_CREDENTIALS_FILE", filepath.Join(dir, "missing-credentials"))
	for _, command := range []string{"version", "--version"} {
		var out bytes.Buffer
		if err := Run(context.Background(), []string{command}, &out, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Source string         `json:"source"`
			Data   buildinfo.Info `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Source != "build" || result.Data != buildinfo.Current() {
			t.Fatalf("unexpected build output: %s (%v)", out.String(), err)
		}
		for _, extra := range []string{"oops", "--client-config=/inaccessible", "--help"} {
			rejectCLI(t, command, extra)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "malformed.toml" {
		t.Fatal("version accessed or created application state", entries, err)
	}
}

func TestVersionLinkedMetadataRealSubprocess(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	binary := filepath.Join(t.TempDir(), "clawbridge")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	commit := strings.Repeat("a", 40)
	flags := "-X clawbridge/internal/buildinfo.Version=v0.1.0 -X clawbridge/internal/buildinfo.Commit=" + commit + " -X clawbridge/internal/buildinfo.BuildTime=2026-10-21T18:00:00Z"
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", flags, "-o", binary, "./cmd/clawbridge")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH, "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	dir := isolated(t)
	t.Setenv("CLAWBRIDGE_CONFIG", filepath.Join(dir, "does-not-exist"))
	t.Setenv("CLAWBRIDGE_MODE", "invalid")
	for _, command := range []string{"version", "--version"} {
		out, err := exec.Command(binary, command).Output()
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Source string         `json:"source"`
			Data   buildinfo.Info `json:"data"`
		}
		if err := json.Unmarshal(out, &result); err != nil || result.Source != "build" || result.Data != (buildinfo.Info{Version: "v0.1.0", Commit: commit, BuildTime: "2026-10-21T18:00:00Z"}) {
			t.Fatalf("linked metadata: %s (%v)", out, err)
		}
		if err := exec.Command(binary, command, "extra").Run(); err == nil {
			t.Fatal("extra version argument succeeded")
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("version subprocess created state", err)
	}
}

func TestSkillCommandNeedsNoTrustedConfiguration(t *testing.T) {
	dir := isolated(t)
	config := filepath.Join(dir, "malformed.toml")
	if err := os.WriteFile(config, []byte("[broken"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWBRIDGE_CONFIG", config)
	skills := filepath.Join(dir, "skills")
	for _, args := range [][]string{{"skill", "install", "--dir", skills}, {"skill", "status", "--dir", skills}} {
		var out bytes.Buffer
		if err := Run(context.Background(), args, &out, &bytes.Buffer{}); err != nil || !strings.Contains(out.String(), `"current":true`) {
			t.Fatal(args, err, out.String())
		}
	}
}
