package doctor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/install"
	"clawbridge/internal/linearprovider"
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

func TestLinearCapabilitiesAreVisibleInDoctor(t *testing.T) {
	cases := map[string]struct {
		caps   linearprovider.Capabilities
		err    error
		status string
		want   []string
	}{
		"on":         {linearprovider.Capabilities{LabelEligibilityByFilter: true, ParentSameDestination: true}, nil, Pass, []string{"label_eligibility_by_filter=on", "parent_same_destination=on"}},
		"off":        {linearprovider.Capabilities{ParentSameDestination: true, Reasons: map[string]string{"label_eligibility_by_filter": "list_issue_labels schema changed: includeGroups missing or no longer a boolean defaulting to false"}}, nil, Warn, []string{"label_eligibility_by_filter=off", "includeGroups"}},
		"unreadable": {linearprovider.Capabilities{}, fmt.Errorf("dial secret transport detail: %w", &linearprovider.Error{Kind: "unavailable"}), Warn, []string{"could not be read (unavailable)"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var checks []Check
			checkLinearCapabilities(context.Background(), func(context.Context) (linearprovider.Capabilities, error) { return tc.caps, tc.err }, func(c Check) { checks = append(checks, c) })
			if len(checks) != 1 || checks[0].ID != "linear.capabilities" || checks[0].Status != tc.status {
				t.Fatal(checks)
			}
			for _, want := range tc.want {
				if !strings.Contains(checks[0].Message, want) {
					t.Fatalf("message %q lacks %q", checks[0].Message, want)
				}
			}
			if strings.Contains(checks[0].Message, "secret") {
				t.Fatal("transport detail leaked", checks[0].Message)
			}
		})
	}
	report := Run(context.Background(), Input{}, nil)
	for _, check := range report.Checks {
		if check.ID == "linear.capabilities" {
			t.Fatal("Linear section shown without a configured profile")
		}
	}
}
