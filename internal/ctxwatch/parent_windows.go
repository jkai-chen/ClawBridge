//go:build windows

package ctxwatch

import (
	"errors"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processEntry is pid's entry in a Toolhelp32 snapshot.
func processEntry(pid int) (windows.ProcessEntry32, bool) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return windows.ProcessEntry32{}, false
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if int(e.ProcessID) == pid {
			return e, true
		}
	}
	return windows.ProcessEntry32{}, false
}

// creationTime is pid's start time; ok is false when it can't be opened.
func creationTime(pid int) (time.Time, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return time.Time{}, false
	}
	defer windows.CloseHandle(h)
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, created.Nanoseconds()), true
}

// ParentPID is pid's parent process, from a Toolhelp32 snapshot. The parent
// is returned only if it still exists and began before pid (acceptParent),
// since Windows keeps the recorded parent id after the parent exits and may
// reissue it.
func ParentPID(pid int) (int, bool) {
	if pid <= 0 {
		return 0, false
	}
	e, ok := processEntry(pid)
	if !ok || e.ParentProcessID == 0 {
		return 0, false
	}
	parent := int(e.ParentProcessID)
	childStart, ok := creationTime(pid)
	if !ok {
		return 0, false
	}
	parentStart, exists := creationTime(parent)
	if !acceptParent(parentStart, childStart, exists) {
		return 0, false
	}
	return parent, true
}

// parentIsShell says whether pid's image is a shell (bash, sh, PowerShell).
func parentIsShell(pid int) bool {
	e, ok := processEntry(pid)
	return ok && isShellImage(windows.UTF16ToString(e.ExeFile[:]))
}

// ProcessAlive says whether pid is a running process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// A process that exists but can't be opened is alive, as with
		// EPERM on unix: mayRebind treats "dead" as free to take over.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}
