package doctor

import (
	"context"
	"strings"
	"testing"
)

// With no entry selected, the warning names every flag a check needs, and
// the file a session got with claude --mcp-config.
func TestMCPEntriesUnselectedNamesTheFlags(t *testing.T) {
	report := Run(context.Background(), Input{}, nil)
	for _, c := range report.Checks {
		if c.ID != "mcp.entries" {
			continue
		}
		for _, want := range []string{"--mcp-agent", "--mcp-config", "--mcp-session", "--mcp-instance", "--mcp-name", "claude --mcp-config"} {
			if !strings.Contains(c.Action, want) {
				t.Fatalf("%q lacks %s", c.Action, want)
			}
		}
		return
	}
	t.Fatal("no mcp.entries check")
}
