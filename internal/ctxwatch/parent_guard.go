package ctxwatch

import (
	"strings"
	"time"
)

// acceptParent is the PID-reuse guard: a process id can be reissued once its
// owner exits, so a parent is believed only when it still exists and began
// strictly before its child. A younger process under the same id is not the
// parent.
func acceptParent(parentStart, childStart time.Time, parentExists bool) bool {
	return parentExists && parentStart.Before(childStart)
}

// isShellImage says whether a Windows image name is a shell that may sit
// between Claude Code and a hook command.
func isShellImage(name string) bool {
	switch strings.ToLower(name) {
	case "bash.exe", "sh.exe", "powershell.exe", "pwsh.exe":
		return true
	}
	return false
}
