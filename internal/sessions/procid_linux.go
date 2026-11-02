package sessions

import (
	"errors"
	"os"
	"strconv"
	"syscall"
)

// platformLookup reads the start time from /proc/<pid>/stat.
func platformLookup(pid int) (int64, procState) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return 0, procGone
		}
		return 0, procUnknown
	}
	_, start, err := parseLinuxStat(string(data))
	if err != nil {
		return 0, procUnknown
	}
	return start, procRunning
}
