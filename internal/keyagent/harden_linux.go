//go:build linux

package keyagent

import "golang.org/x/sys/unix"

func hardenOS() []string {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return []string{"cannot make the process undumpable"}
	}
	return nil
}
