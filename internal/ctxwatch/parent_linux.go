package ctxwatch

import (
	"os"
	"strconv"
	"strings"
)

// ParentPID is pid's parent process, from /proc/PID/stat.
func ParentPID(pid int) (int, bool) {
	if pid <= 0 {
		return 0, false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// The command name is in parentheses and may hold spaces: the fields
	// after its closing parenthesis are the state, then the parent.
	s := string(data)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 2 {
		return 0, false
	}
	parent, err := strconv.Atoi(fields[1])
	return parent, err == nil
}
