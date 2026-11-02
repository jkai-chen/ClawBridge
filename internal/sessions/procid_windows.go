package sessions

import (
	"errors"

	"golang.org/x/sys/windows"
)

// platformLookup reads the creation time with GetProcessTimes, in
// nanoseconds since the epoch.
func platformLookup(pid int) (int64, procState) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, procGone
		}
		return 0, procUnknown
	}
	defer windows.CloseHandle(h)
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return 0, procUnknown
	}
	// A process that has exited but is still held open by someone has an
	// exit time.
	if exit.HighDateTime != 0 || exit.LowDateTime != 0 {
		return 0, procGone
	}
	start := created.Nanoseconds()
	if start <= 0 {
		return 0, procUnknown
	}
	return start, procRunning
}
