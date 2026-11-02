//go:build windows

package webui

func syscallUmask(m int) int { return 0 }
