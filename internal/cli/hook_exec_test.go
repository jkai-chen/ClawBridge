package cli

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/ctxwatch"
)

// startChild starts this test binary as a child of the test process in the
// given role (see TestMain) and stops it with the test.
func startChild(t *testing.T, role string, stdin string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	// An empty CLAWBRIDGE_AGENT_PID is ignored: the hook derives its agent.
	cmd.Env = append(os.Environ(), "CLAWBRIDGE_TEST_CHILD="+role, "CLAWBRIDGE_AGENT_PID=")
	var out bytes.Buffer
	cmd.Stdout = &out
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd, &out
}

// The exec form starts the hook with no shell, so the hook's parent is
// Claude Code and CLAWBRIDGE_AGENT_PID is not set. Here the test process is
// Claude Code and a child of it is its MCP server: the hook, also a child
// of the test process, acts only when the record's agent is its parent and
// the MCP server is that agent's child.
func TestExecFormHookActsOnlyForItsAgent(t *testing.T) {
	isolated(t)
	t.Setenv("CLAWBRIDGE_SESSION", "")
	dir, err := contextDir()
	if err != nil {
		t.Fatal(err)
	}
	mcp, _ := startChild(t, "wait", "")
	self := os.Getpid()
	save := func(agent, mcpPID int) {
		t.Helper()
		r := ctxwatch.QueueRecord{Session: "agent", Workspace: "w", Project: "p", Instance: "inst-1", Server: "https://clawbridge.example.test",
			MCPPID: mcpPID, AgentPID: agent, Stream: "stream", UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
		if err := ctxwatch.SaveQueueRecord(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	runHook := func() string {
		t.Helper()
		cmd, out := startChild(t, "stop-hook", stopInput)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("the hook failed: %v", err)
		}
		return out.String()
	}
	for name, c := range map[string]struct{ agent, mcp int }{
		"another agent":                 {self + 1000, mcp.Process.Pid},
		"the MCP server is not a child": {self, self},
		"the agent is the MCP server":   {mcp.Process.Pid, mcp.Process.Pid},
	} {
		save(c.agent, c.mcp)
		if got := runHook(); got != "" {
			t.Errorf("%s: the hook acted: %q", name, got)
		}
	}
	// Last, since an acting hook leaves a mark that stops it blocking twice.
	save(self, mcp.Process.Pid)
	if got := runHook(); got != blocked {
		t.Fatalf("the hook did not act for its agent: %q", got)
	}
}
