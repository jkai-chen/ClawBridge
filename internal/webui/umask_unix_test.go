//go:build !windows

package webui

import "syscall"

func syscallUmask(m int) int { return syscall.Umask(m) }
