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

// freshHome is a new machine: a local profile from setup, and no Claude
// Code or Codex configuration (agent dirs pointed at nothing).
func freshHome(t *testing.T) string {
	t.Helper()
	dir := isolated(t)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "home", ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "home", ".codex"))
	var errs bytes.Buffer
	for _, args := range [][]string{{"setup", "--local", "--name", "w", "--project", "p"}, {"project", "create", "p"}} {
		if err := Run(context.Background(), args, &bytes.Buffer{}, &errs); err != nil {
			t.Fatal(err, errs.String())
		}
	}
	return dir
}

func doctorChecks(t *testing.T, args ...string) (doctor.Report, map[string]doctor.Check, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(clirender.WithMode(context.Background(), clirender.JSON), append([]string{"doctor"}, args...), &out, &bytes.Buffer{})
	var report doctor.Report
	if e := json.Unmarshal(out.Bytes(), &report); e != nil {
		t.Fatal(e, out.String())
	}
	checks := map[string]doctor.Check{}
	for _, c := range report.Checks {
		checks[c.ID] = c
	}
	return report, checks, err
}

// On a fresh install with no agent and no --mcp-* flags, the MCP entries
// and the agent skill are notes (info), not warnings: doctor still exits 0,
// its status is pass, and the human summary counts the notes.
func TestFreshInstallDoctorNotesNoAgentAsInfo(t *testing.T) {
	freshHome(t)
	report, checks, err := doctorChecks(t)
	if err != nil {
		for _, c := range report.Checks {
			t.Logf("%s %s %s", c.ID, c.Status, c.Message)
		}
		t.Fatal(err)
	}
	for _, id := range []string{"mcp.entries", "skill.version"} {
		if checks[id].Status != doctor.Info {
			t.Fatalf("%s on a fresh install: %+v", id, checks[id])
		}
	}
	for _, c := range report.Checks {
		if c.Status == doctor.Warn {
			t.Fatalf("a fresh local install warns: %+v", c)
		}
	}
	if report.Status != doctor.Pass {
		t.Fatalf("report status %q", report.Status)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"--json=false", "doctor"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if text := out.String(); !strings.Contains(text, " · 0 warn · 0 fail · 2 info") || !strings.Contains(text, "No agent is set up here, so no skill was checked.") {
		t.Fatalf("human summary: %s", text)
	}
	if s := doctorSummary(report); s != "pass" {
		t.Fatalf("the onboarding summary of a fresh install: %q", s)
	}
}

// Real problems stay warnings: --mcp-* flags given with no entry there, and
// an agent set up without the skill.
func TestDoctorStillWarnsWhenAskedOrWhenAnAgentIsSetUp(t *testing.T) {
	dir := freshHome(t)
	_, checks, _ := doctorChecks(t, "--mcp-agent", "claude-code", "--mcp-config", filepath.Join(dir, "absent.json"), "--mcp-session", "agent")
	if c := checks["mcp.claude-code.clawbridge"]; c.Status != doctor.Warn {
		t.Fatalf("an asked-for entry that's absent: %+v", c)
	}
	if _, ok := checks["mcp.entries"]; ok {
		t.Fatal("mcp.entries with an entry selected")
	}
	// A folder with nothing in it is no agent set up (doctor_not_set_up_test.go).
	if err := os.MkdirAll(filepath.Join(dir, "home", ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "home", ".claude", "settings.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report, checks, _ := doctorChecks(t)
	for _, id := range []string{"skill.version", "mcp.entries"} {
		if checks[id].Status != doctor.Warn {
			t.Fatalf("%s with Claude Code set up and no skill: %+v", id, checks[id])
		}
	}
	if s := doctorSummary(report); !strings.Contains(s, "warn Agent skill") {
		t.Fatalf("the onboarding summary hides a missing skill: %q", s)
	}
}
