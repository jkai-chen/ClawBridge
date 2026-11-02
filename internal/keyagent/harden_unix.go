//go:build !windows

package keyagent

import "golang.org/x/sys/unix"

func hardenPlatform() []string {
	var failed []string
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}); err != nil {
		failed = append(failed, "cannot turn core dumps off")
	}
	return append(failed, hardenOS()...)
}
