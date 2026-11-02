//go:build !windows && !linux && !darwin

package keyagent

import (
	"errors"
	"net"
)

// peerUIDOf cannot tell on this system, so every peer is refused.
func peerUIDOf(*net.UnixConn) (uint32, error) {
	return 0, errors.New("no way to read a socket peer's uid on this system")
}
