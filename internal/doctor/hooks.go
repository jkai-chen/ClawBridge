package doctor

import (
	"fmt"
	"slices"
	"strings"

	"clawbridge/internal/ctxwatch"
)

// WindowsProbedVersions are the Claude Code versions on which the Windows
// exec-form hooks are known to work. It is empty until the Windows probe is
// recorded; until then doctor warns on Windows.
var WindowsProbedVersions = []string{}

// HooksState is what the client read locally about the Claude Code hooks it
// installed.
type HooksState struct {
	// GOOS is the platform doctor runs on.
	GOOS string
	// Form is ctxwatch.FormExec, FormShell, "mixed", or "" when no ClawBridge
	// hook is installed in any settings file looked at.
	Form string
	// Paths are the settings files holding the hooks.
	Paths []string
	// Version is the Claude Code version, "" when claude was not found or its
	// version could not be read in time.
	Version string
	Err     error
}

// checkHooks reports the form of the installed hooks and the Claude Code
// version. It warns, and never fails. With no hooks installed it says
// nothing.
func checkHooks(s HooksState, add func(Check)) {
	if s.Err == nil && s.Form == "" {
		return
	}
	c := Check{ID: "hooks", Title: "Claude Code hooks", Status: Pass, Path: strings.Join(s.Paths, ", ")}
	if s.Err != nil {
		c.Status = Warn
		c.Message = s.Err.Error()
		add(c)
		return
	}
	version := "Claude Code was not found, so its version is unknown"
	if s.Version != "" {
		version = "Claude Code " + s.Version
	}
	c.Message = fmt.Sprintf("The hooks are installed in %s form (%s).", s.Form, version)
	if s.GOOS == "windows" && s.Form != ctxwatch.FormExec {
		// The shell form needs a POSIX shell: a settings file installed on
		// macOS or Linux (a committed shared one) can't work here.
		c.Status = Warn
		c.Message += " The shell form needs a POSIX shell, which Windows hooks don't have."
		c.Action = "Reinstall the hooks on this machine with `clawbridge mcp install`, or use the local or user scope."
	} else if s.GOOS == "windows" && !slices.Contains(WindowsProbedVersions, s.Version) {
		c.Status = Warn
		c.Message += " The Windows hooks are pending the Windows probe: they are written, but waking an agent through them is not confirmed for this Claude Code version."
		c.Action = "To help the probe, report your Claude Code version and whether a Stop hook held back a stop while mail was waiting."
	}
	add(c)
}
