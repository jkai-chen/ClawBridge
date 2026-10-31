package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/clirender"
	"clawbridge/internal/doctor"
)

// neverSetUp is a machine where ClawBridge was never set up: no profile
// file, no route in the environment, and no agent configuration.
func neverSetUp(t *testing.T) string {
	t.Helper()
	dir := isolated(t)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "home", ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "home", ".codex"))
	return dir
}

// A machine with no profile at all gets one answer, not three failures, and
// still exits 1. The store is not created either.
func TestDoctorNeverSetUpReportsOneCheck(t *testing.T) {
	dir := neverSetUp(t)
	report, checks, err := doctorChecks(t)
	if err == nil {
		t.Fatal("doctor exited 0 on a machine that is not set up")
	}
	if report.Status != doctor.Fail {
		t.Fatalf("report status %q", report.Status)
	}
	c, ok := checks["setup.none"]
	if !ok || c.Status != doctor.Fail || c.Message != "ClawBridge isn't set up on this machine yet: run clawbridge to start (or clawbridge join with an invite)." {
		t.Fatalf("setup.none: %+v", c)
	}
	for _, id := range []string{"config.trusted", "authority.health", "project.resolution", "auth.token-selection", "mirror.freshness"} {
		if _, ok := checks[id]; ok {
			t.Fatalf("%s is reported with no profile", id)
		}
	}
	for _, id := range []string{"skill.version", "mcp.entries"} {
		if checks[id].Status != doctor.Info {
			t.Fatalf("%s: %+v", id, checks[id])
		}
	}
	fails := 0
	for _, c := range report.Checks {
		if c.Status == doctor.Fail {
			fails++
		}
	}
	if fails != 1 {
		t.Fatalf("%d failures, want 1", fails)
	}
	if _, e := os.Stat(filepath.Join(dir, "state", "local.db")); !os.IsNotExist(e) {
		t.Fatalf("doctor created the local store: %v", e)
	}
}

func TestDoctorNeverSetUpTextForm(t *testing.T) {
	neverSetUp(t)
	var out bytes.Buffer
	err := Run(context.Background(), []string{"--json=false", "doctor"}, &out, &bytes.Buffer{})
	if err == nil {
		t.Fatal("doctor exited 0 on a machine that is not set up")
	}
	text := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(text, "isn't set up on this machine yet: run clawbridge to start (or clawbridge join with an invite)") || !strings.Contains(text, "1 fail") {
		t.Fatalf("text form: %s", text)
	}
	for _, gone := range []string{"Trusted configuration", "Authority health", "Project resolution"} {
		if strings.Contains(text, gone) {
			t.Fatalf("text form still has %q: %s", gone, text)
		}
	}
}

// Local mode needs no config file, so a store at the default path is a
// profile: doctor runs its usual checks and does not say "not set up".
func TestDoctorDefaultLocalStoreIsAProfile(t *testing.T) {
	dir := neverSetUp(t)
	write(t, filepath.Join(dir, "state", "local.db"), "")
	_, checks, _ := doctorChecks(t)
	if _, ok := checks["setup.none"]; ok {
		t.Fatal("setup.none with a local store present")
	}
	if _, ok := checks["config.trusted"]; !ok {
		t.Fatal("no config.trusted check with a local store present")
	}
}

// Anything that names a profile keeps today's checks: a partial local file,
// a cloud file, and a route given only by the environment or a flag.
func TestDoctorWithAnyProfileKeepsItsChecks(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string) []string{
		"partial local file": func(t *testing.T, dir string) []string {
			write(t, filepath.Join(dir, "state", "config.toml"), "mode = \"local\"\nworkspace = \"w\"\n")
			return nil
		},
		"empty file": func(t *testing.T, dir string) []string {
			write(t, filepath.Join(dir, "state", "config.toml"), "")
			return nil
		},
		"cloud file": func(t *testing.T, dir string) []string {
			write(t, filepath.Join(dir, "state", "config.toml"), "mode = \"cloud\"\nserver = \"https://cb.example.test\"\nworkspace = \"acme\"\nprincipal = \"ada@example.test\"\ncache = \"cache.db\"\ncredential_source = \"device\"\ndevice_id = \"dev_abcdefghjkmnpqrstvwxyz0123\"\nkey_store = \"file\"\n")
			return nil
		},
		"environment mode": func(t *testing.T, dir string) []string {
			t.Setenv("CLAWBRIDGE_MODE", "local")
			return nil
		},
		"environment workspace": func(t *testing.T, dir string) []string {
			t.Setenv("CLAWBRIDGE_WORKSPACE", "w")
			return nil
		},
		"flag": func(t *testing.T, dir string) []string { return []string{"--workspace", "w"} },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := neverSetUp(t)
			args := setup(t, dir)
			var out bytes.Buffer
			_ = Run(clirender.WithMode(context.Background(), clirender.JSON), append(args, "doctor"), &out, &bytes.Buffer{})
			var report doctor.Report
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err, out.String())
			}
			ids := map[string]bool{}
			for _, c := range report.Checks {
				ids[c.ID] = true
			}
			if ids["setup.none"] || !ids["config.trusted"] || !ids["authority.health"] || !ids["project.resolution"] {
				t.Fatalf("%s: %v", name, ids)
			}
		})
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An agent folder that exists but is empty (an installer made it) is
// treated like a missing one: a note, not "not installed". One with real
// configuration keeps its result.
func TestDoctorEmptyAgentFolderIsAsMissing(t *testing.T) {
	dir := freshHome(t)
	claude := filepath.Join(dir, "home", ".claude")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	_, checks, _ := doctorChecks(t)
	for _, id := range []string{"skill.version", "mcp.entries"} {
		if checks[id].Status != doctor.Info {
			t.Fatalf("%s with an empty agent folder: %+v", id, checks[id])
		}
	}
	write(t, filepath.Join(claude, "settings.json"), "{}\n")
	_, checks, _ = doctorChecks(t)
	for _, id := range []string{"skill.version", "mcp.entries"} {
		if checks[id].Status != doctor.Warn {
			t.Fatalf("%s with a configured agent folder: %+v", id, checks[id])
		}
	}
}
