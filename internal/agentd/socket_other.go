//go:build !unix

package agentd

import (
	"errors"
	"net"
)

var errUnsupported = errors.New("clawbridge-agentd runs on Linux only")

func checkSocket(string) error                  { return errUnsupported }
func notRoot() error                            { return errUnsupported }
func listenSocket(string) (net.Listener, error) { return nil, errUnsupported }
