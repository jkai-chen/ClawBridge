package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
)

func l1InstalledCheck(t *testing.T, report doctor.Report, agent, name string) doctor.Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == "mcp."+agent+"."+name {
			return check
		}
	}
	t.Fatal("doctor omitted selected installed entry")
	return doctor.Check{}
}
func l1MCPSelection(o install.Options) doctor.MCPConfig {
	return doctor.MCPConfig{LinearProfile: o.LinearProfile, ClientConfig: o.ClientConfig, Agent: o.Agent, Name: "clawbridge", Path: o.Config, DB: o.DB, Workspace: o.Workspace, Project: o.Project, Session: o.Session, Instance: o.Instance, Mode: o.Mode, Server: o.Server, Principal: o.Principal, Cache: o.Cache, CredentialSource: o.CredentialSource, DeviceID: o.DeviceID, KeyStore: o.KeyStore}
}

func TestL1InstallerDoctorLinearProfileExactRoute(t *testing.T) {
	for _, agent := range []string{"codex", "claude-code"} {
		for _, mode := range []string{"local", "cloud"} {
			for _, profile := range []string{"", l1ProfileID} {
				label := profile
				if label == "" {
					label = "legacy-no-profile"
				}
				t.Run(agent+"/"+mode+"/"+label, func(t *testing.T) {
					o := deviceInstallOptions(t)
					o.Agent = agent
					if agent == "codex" {
						o.Config = filepath.Join(filepath.Dir(o.Config), "codex.toml")
					}
					if mode == "local" {
						o = install.Options{Agent: agent, Config: o.Config, Command: o.Command, DB: o.DB, Mode: "local", Workspace: o.Workspace, Project: o.Project, Session: o.Session, Instance: o.Instance}
					}
					o.LinearProfile = profile
					result, err := install.Install(o)
					if err != nil {
						t.Fatal(err)
					}
					args := result.Entry["args"].([]string)
					found := 0
					for n, arg := range args {
						if arg == "--linear-profile" {
							found++
							if n+1 >= len(args) || args[n+1] != profile {
								t.Fatal("installer lost explicit native profile")
							}
						}
					}
					want := 0
					if profile != "" {
						want = 1
					}
					if found != want {
						t.Fatal("installer fabricated/dropped native profile selection")
					}
					before, err := os.ReadFile(o.Config)
					if err != nil {
						t.Fatal(err)
					}
					for _, expected := range []string{profile, "lin_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
						selection := l1MCPSelection(o)
						selection.LinearProfile = expected
						report := doctor.Run(context.Background(), doctor.Input{MCPConfigs: []doctor.MCPConfig{selection}}, nil)
						check := l1InstalledCheck(t, report, agent, "clawbridge")
						want := doctor.Fail
						if expected == profile {
							want = doctor.Pass
						}
						if check.Status != want {
							t.Fatalf("doctor profile agreement mismatch installed=%q selected=%q status=%s want=%s", profile, expected, check.Status, want)
						}
					}
					if profile != "" {
						selection := l1MCPSelection(o)
						selection.LinearProfile = ""
						report := doctor.Run(context.Background(), doctor.Input{MCPConfigs: []doctor.MCPConfig{selection}}, nil)
						// Without a profile of its own, doctor takes the
						// entry's and says so: never silently.
						if check := l1InstalledCheck(t, report, agent, "clawbridge"); check.Status != doctor.Pass || !strings.Contains(check.Message, "Linear profile "+profile) {
							t.Fatal("doctor did not name the installed opt-in profile", check)
						}
					}
					after, err := os.ReadFile(o.Config)
					if err != nil || string(before) != string(after) {
						t.Fatal("doctor rewrote installed config")
					}
					repeat, err := install.Install(o)
					if err != nil || repeat.Changed {
						t.Fatal("same native route installation not idempotent", err)
					}
				})
			}
		}
	}
}

func TestL1CLIInstalledLinearProfileDoctorSelection(t *testing.T) {
	p := newProcessFixture(t)
	p.seed()
	for _, agent := range []string{"codex", "claude-code"} {
		t.Run(agent, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "selected client config.toml")
			if agent == "claude-code" {
				path = filepath.Join(filepath.Dir(path), "selected client config.json")
			}
			cmd := p.command("--linear-profile", l1ProfileID, "--session", "mcp-linear", "--instance", "mcp-linear-instance", "mcp", "install", "--agent", agent, "--config", path, "--command", p.binary)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("isolated actual CLI install failed: %v %s", err, out)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, selected := range []string{l1ProfileID, "lin_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ""} {
				args := []string{}
				if selected != "" {
					args = append(args, "--linear-profile", selected)
				}
				args = append(args, "doctor", "--mcp-agent", agent, "--mcp-config", path, "--mcp-session", "mcp-linear", "--mcp-instance", "mcp-linear-instance")
				out, runErr := p.command(args...).Output()
				var report doctor.Report
				if err := json.Unmarshal(out, &report); err != nil {
					t.Fatalf("doctor did not emit report: %v runErr=%v", err, runErr)
				}
				check := l1InstalledCheck(t, report, agent, "clawbridge")
				// No profile selected: doctor takes the entry's own.
				want := doctor.Fail
				if selected == l1ProfileID || selected == "" {
					want = doctor.Pass
				}
				if check.Status != want {
					t.Fatalf("CLI lost/misvalidated native profile: selected=%q check=%s want=%s", selected, check.Status, want)
				}
				if selected == "" && !strings.Contains(check.Message, "Linear profile "+l1ProfileID) {
					t.Fatal("doctor did not name the installed opt-in profile", check.Message)
				}
				if selected == l1ProfileID && runErr != nil {
					t.Fatalf("correct installed native route made local doctor fail: %v", runErr)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("actual CLI doctor changed installed config")
			}
		})
	}
}
