package ctxwatch

import "golang.org/x/sys/unix"

// ParentPID is pid's parent process, from the kernel's process table.
func ParentPID(pid int) (int, bool) {
	if pid <= 0 {
		return 0, false
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(p.Proc.P_pid) != pid {
		return 0, false
	}
	return int(p.Eproc.Ppid), true
}
