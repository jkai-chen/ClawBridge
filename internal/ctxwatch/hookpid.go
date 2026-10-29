package ctxwatch

import (
	"os"
	"strconv"
)

// HookAgentPID is the Claude Code process a hook runs under, or 0. The
// CLAWBRIDGE_AGENT_PID the hook command set wins. Without it, the agent is
// this process's parent; on Windows, where a hook may be started through a
// shell, a shell parent is skipped for its own parent.
func HookAgentPID() int {
	if pid, err := strconv.Atoi(os.Getenv("CLAWBRIDGE_AGENT_PID")); err == nil && pid > 0 {
		return pid
	}
	parent, ok := ParentPID(os.Getpid())
	if !ok {
		return 0
	}
	if parentIsShell(parent) {
		parent, ok = ParentPID(parent)
		if !ok {
			return 0
		}
	}
	return parent
}
