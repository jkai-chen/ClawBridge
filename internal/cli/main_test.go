package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/faketool"
)

// TestMain lets a copy of this test binary act as a fake command-line tool,
// or, with CLAWBRIDGE_TEST_CHILD set, as one of the processes of
// TestExecFormHookActsOnlyForItsAgent: an MCP server stand-in that only
// waits, or the Stop hook run as Claude Code runs an exec-form hook.
func TestMain(m *testing.M) {
	switch os.Getenv("CLAWBRIDGE_TEST_CHILD") {
	case "wait":
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	case "stop-hook":
		queueHookRoute = func(context.Context, ctxwatch.QueueRecord) (queueExecutor, func(), error) {
			return &hookServer{status: map[string]any{"policy": "accept", "unacked_count": float64(2), "latest_seq": float64(7), "unread_count": float64(2)}}, func() {}, nil
		}
		if err := Run(context.Background(), []string{"hook", "stop", "--from-hook", "--hook-tag", ctxwatch.HookTag}, os.Stdout, io.Discard); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	// Doctor reads the Claude Code settings and runs `claude --version`: no
	// test reads the developer's own files or runs a real claude.
	// A directory that is never created, so nothing there is read.
	dir := filepath.Join(os.TempDir(), "clawbridge-cli-test-no-claude-"+strconv.Itoa(os.Getpid()))
	os.Setenv("CLAUDE_CONFIG_DIR", dir)
	hooksClaudeVersion = func(context.Context) (string, bool) { return "", false }
	// No test deletes from the developer's own OS keychain.
	deleteLegacyCredential = func(context.Context, string, string) (bool, error) { return false, nil }
	faketool.Main(m)
}
