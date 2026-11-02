package sessions

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// platformLookup reads the start time from the kernel's process table
// (sysctl kern.proc.pid), in microseconds since the epoch.
func platformLookup(pid int) (int64, procState) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) {
			return 0, procGone
		}
		return 0, procUnknown
	}
	if int(p.Proc.P_pid) != pid {
		return 0, procGone
	}
	tv := p.Proc.P_starttime
	start := int64(tv.Sec)*1_000_000 + int64(tv.Usec)
	if start <= 0 {
		return 0, procUnknown
	}
	return start, procRunning
}
