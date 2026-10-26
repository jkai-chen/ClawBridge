//go:build !unix

package agentd

import "errors"

var errUnsupported = errors.New("clawbridge-agentd runs on Linux only")

func checkSocket(string) error { return errUnsupported }
func notRoot() error           { return errUnsupported }
