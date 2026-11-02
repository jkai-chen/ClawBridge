//go:build !windows

package webui

import (
	"os/exec"
	"syscall"
)

// detach puts the browser in its own session so Ctrl-C in the terminal
// does not reach it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func systemWindows(*Probe) {}
