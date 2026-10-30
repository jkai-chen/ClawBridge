package integration

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
)

// Installed entries must not pass diagnostics merely because their argv match:
// agent configuration can also inject forbidden credential material via env.
func TestAuditDoctorRejectsSecretBearingMCPEnvironment(t *testing.T) {
	checkAuditMCPEnvironment(t, "env", map[string]any{"CLAWBRIDGE_OAUTH_CLIENT_SECRET": "synthetic-audit-secret-never-print"}, false)
}

func TestAuditDoctorEnvironmentContract(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
		pass        bool
	}{
		{"benign", "env", map[string]any{"LANG": "C.UTF-8"}, true},
		{"route-override", "env", map[string]any{"CLAWBRIDGE_SERVER": "synthetic-audit-secret-never-print"}, false},
		{"credential-file", "env", map[string]any{"GOOGLE_APPLICATION_CREDENTIALS": "synthetic-audit-secret-never-print"}, false},
		{"malformed-map", "env", "synthetic-audit-secret-never-print", false},
		{"non-string-value", "env", map[string]any{"LANG": 12}, false},
		{"inherited-token", "env_vars", []any{"GOOGLE_ACCESS_TOKEN"}, false},
		{"inherited-routing", "env_vars", []any{"CLAWBRIDGE_CONFIG"}, false},
		{"benign-inherited", "env_vars", []any{"LANG"}, true},
		{"malformed-inherited", "env_vars", map[string]any{"LANG": "C"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) { checkAuditMCPEnvironment(t, tc.field, tc.value, tc.pass) })
	}
}

func checkAuditMCPEnvironment(t *testing.T, field string, value any, wantPass bool) {
	t.Helper()
	o := deviceInstallOptions(t)
	installed, err := install.Install(o)
	if err != nil {
		t.Fatal(err)
	}
	const sentinel = "synthetic-audit-secret-never-print"
	installed.Entry[field] = value
	raw, err := json.Marshal(map[string]any{"mcpServers": map[string]any{o.Name: installed.Entry}})
	// Install defaults the empty Options.Name without mutating the caller.
	if o.Name == "" {
		raw, err = json.Marshal(map[string]any{"mcpServers": map[string]any{"clawbridge": installed.Entry}})
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.Config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	report := doctor.Run(context.Background(), doctor.Input{
		MCPConfigs: []doctor.MCPConfig{{
			Agent: o.Agent, Name: "clawbridge", Path: o.Config,
			Workspace: o.Workspace, Project: o.Project, Session: o.Session, Instance: o.Instance,
			Mode: o.Mode, Server: o.Server,
			Principal: o.Principal, Cache: o.Cache, CredentialSource: o.CredentialSource,
			DeviceID: o.DeviceID, KeyStore: o.KeyStore,
		}},
	}, routingExecutor{})
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), sentinel) {
		t.Fatal("doctor reflected the synthetic credential")
	}
	for _, check := range report.Checks {
		if check.ID == "mcp.claude-code.clawbridge" {
			if (check.Status == doctor.Pass) != wantPass {
				t.Fatalf("MCP environment diagnostic pass=%t, want %t", check.Status == doctor.Pass, wantPass)
			}
			return
		}
	}
	t.Fatal("doctor did not inspect the installed MCP entry")
}
