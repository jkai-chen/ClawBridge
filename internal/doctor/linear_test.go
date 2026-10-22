package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"clawbridge/internal/install"
)

func TestLinearInstalledProfileMatchesDoctorExactly(t *testing.T) {
	for _, agent := range []string{"codex", "claude-code"} {
		for _, profile := range []string{"", "lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
			t.Run(agent+"-"+profile, func(t *testing.T) {
				dir := t.TempDir()
				command := filepath.Join(dir, "clawbridge")
				if err := os.WriteFile(command, []byte("fixture"), 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "agent-config")
				db := filepath.Join(dir, "state.db")
				_, err := install.Install(install.Options{Agent: agent, Config: path, Command: command, DB: db, Mode: "local", Workspace: "w", Project: "p", Session: "actor", Instance: "instance", LinearProfile: profile})
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				expected := MCPConfig{Agent: agent, Name: "clawbridge", Path: path, DB: db, Mode: "local", Workspace: "w", Project: "p", Session: "actor", Instance: "instance", LinearProfile: profile}
				checkMCP(expected, func(check Check) {
					if check.Status != Pass {
						t.Fatal("matching native profile rejected", check)
					}
				})
				if profile != "" {
					for _, wrong := range []string{"", "lin_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
						expected.LinearProfile = wrong
						checkMCP(expected, func(check Check) {
							if check.Status != Fail {
								t.Fatal("changed or omitted native profile accepted", check)
							}
						})
					}
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("doctor modified selected config", err)
				}
			})
		}
	}
	if _, err := expectedMCPEntry(MCPConfig{LinearProfile: "secret\nvalue"}); err == nil {
		t.Fatal("hostile profile accepted")
	}
}
