package fde

import (
	"context"
	"os"
)

const methodWindowsPolicy = "windows-policy"
const windowsPolicyDetail = "Windows and WSL use a passphrase by default"

// Replaced by tests.
var (
	getenv   = os.Getenv
	pathStat = os.Stat
)

const wslInterop = "/proc/sys/fs/binfmt_misc/WSLInterop"

func isWSL() bool {
	if getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	_, err := pathStat(wslInterop)
	return err == nil
}

// windowsPolicy is the result on Windows and on WSL. Key storage there uses a
// passphrase by default, so no disk-encryption check is made at all. On WSL
// the answer must not come from the Linux path either: the distro's ext4
// sits inside a VHDX and says nothing about the host drive.
func windowsPolicy(context.Context) Result {
	return unknown(methodWindowsPolicy, windowsPolicyDetail)
}
