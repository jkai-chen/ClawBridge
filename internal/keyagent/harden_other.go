//go:build !windows && !linux && !darwin

package keyagent

func hardenOS() []string { return []string{"no debugger-attach hardening on this system"} }
