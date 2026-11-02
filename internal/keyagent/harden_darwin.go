//go:build darwin

package keyagent

import "golang.org/x/sys/unix"

func hardenOS() []string {
	if err := unix.PtraceDenyAttach(); err != nil {
		return []string{"cannot deny debugger attach"}
	}
	return nil
}
