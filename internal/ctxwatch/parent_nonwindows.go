//go:build !windows

package ctxwatch

// parentIsShell is always false off Windows: the hook's parent is the agent.
func parentIsShell(int) bool { return false }
