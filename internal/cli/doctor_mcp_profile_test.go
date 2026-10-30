package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/clirender"
	"clawbridge/internal/doctor"
)

// An entry `mcp install --linear-profile` wrote passes doctor run without a
// profile, which takes the entry's own well-formed one, and with the same
// one by --mcp-linear-profile or --linear-profile; a different one conflicts
// and says which, --mcp-linear-profile winning over --linear-profile.
func TestDoctorAcceptsAnEntryInstalledWithALinearProfile(t *testing.T) {
	const profile, other = "lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "lin_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, agent := range []string{"claude-code", "codex"} {
		t.Run(agent, func(t *testing.T) {
			dir := isolated(t)
			config := filepath.Join(dir, "agent-config")
			scope := []string{"--workspace", "w", "--project", "p"}
			install := append(append([]string{}, scope...), "--session", "agent", "--linear-profile", profile, "mcp", "install", "--agent", agent, "--config", config)
			if agent == "claude-code" {
				install = append(install, "--hooks", "none")
			}
			var out, errs bytes.Buffer
			if err := Run(context.Background(), install, &out, &errs); err != nil {
				t.Fatal(err, errs.String())
			}
			check := func(extra ...string) doctor.Check {
				t.Helper()
				args := append(append(append([]string{}, scope...), "doctor", "--mcp-agent", agent, "--mcp-config", config, "--mcp-session", "agent"), extra...)
				var out bytes.Buffer
				ctx := clirender.WithMode(context.Background(), clirender.JSON)
				_ = Run(ctx, args, &out, &bytes.Buffer{})
				var report doctor.Report
				if err := json.Unmarshal(out.Bytes(), &report); err != nil {
					t.Fatalf("%v: %v %s", args, err, out.String())
				}
				for _, c := range report.Checks {
					if c.ID == "mcp."+agent+".clawbridge" {
						return c
					}
				}
				t.Fatalf("%v: no MCP check in %s", args, out.String())
				return doctor.Check{}
			}
			for _, extra := range [][]string{nil, {"--mcp-linear-profile", profile}, {"--linear-profile", profile}} {
				if c := check(extra...); c.Status != doctor.Pass {
					t.Fatalf("%v: a correct install flagged: %+v", extra, c)
				}
			}
			for _, extra := range [][]string{{"--mcp-linear-profile", other}, {"--linear-profile", other}, {"--linear-profile", profile, "--mcp-linear-profile", other}} {
				if c := check(extra...); c.Status != doctor.Fail || !strings.Contains(c.Message, "Linear profile "+profile+", expected "+other) {
					t.Fatalf("%v: a different profile accepted or unexplained: %+v", extra, c)
				}
			}
		})
	}
}
