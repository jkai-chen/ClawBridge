//go:build windows

package keyagent

func hardenPlatform() []string {
	return []string{"no core-dump or debugger hardening is applied on Windows"}
}
