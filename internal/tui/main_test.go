package tui

import (
	"os"
	"testing"

	"clawbridge/internal/agentenv"
)

// TestMain keeps a test run inside an agent session from sending that
// session's agent marker, which a person-only operation refuses.
func TestMain(m *testing.M) {
	agentenv.Environ = func() []string { return nil }
	os.Exit(m.Run())
}
