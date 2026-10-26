//go:build unix

package agentd

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// checkSocket refuses a path that is not a socket, or one owned by root: the
// root Docker daemon's socket, wherever it is linked from, is root-owned,
// while the rootless daemon's and the proxy's belong to unprivileged users.
func checkSocket(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !ok || st.Uid == 0:
		return fmt.Errorf("%s is owned by root; agentd only uses the rootless daemon of the agents' own user, through its proxy", path)
	case info.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("%s is not a socket", path)
	}
	return nil
}

// notRoot refuses to run as root: neither agentd nor its proxy needs it.
func notRoot() error {
	if geteuid() == 0 {
		return fmt.Errorf("clawbridge-agentd does not run as root; run it as its own unprivileged user")
	}
	return nil
}

// listenSocket creates a unix socket that is never more open than 0660.
func listenSocket(path string) (net.Listener, error) {
	old := syscall.Umask(0o117)
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}
