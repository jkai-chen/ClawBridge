package ctxwatch

import (
	"os"
	"runtime"
	"strconv"
)

// hookGOOS is the platform HookAgentPID derives for; tests replace it.
var hookGOOS = runtime.GOOS

// HookAgentPID is the Claude Code process a hook runs under, or 0. The
// CLAWBRIDGE_AGENT_PID the hook command set wins. Without it, the agent is
// this process's parent; on Windows, where a hook may be started through a
// shell, a shell parent is skipped for its own parent, and a value of 1 or
// less is ignored: Git Bash's $PPID is 1 for a POSIX-text hook, which would
// otherwise win and bind PID 1.
func HookAgentPID() int {
	if pid, err := strconv.Atoi(os.Getenv("CLAWBRIDGE_AGENT_PID")); err == nil && pid > 0 && (pid > 1 || hookGOOS != "windows") {
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
