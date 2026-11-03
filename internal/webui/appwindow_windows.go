//go:build windows

package webui

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// systemWindows reads the known folders and the App Paths registry keys,
// not the environment.
func systemWindows(p *Probe) {
	ids := map[string]*windows.KNOWNFOLDERID{
		"ProgramFiles":    windows.FOLDERID_ProgramFiles,
		"ProgramFilesX86": windows.FOLDERID_ProgramFilesX86,
		"LocalAppData":    windows.FOLDERID_LocalAppData,
	}
	p.WinFolder = func(name string) string {
		id, ok := ids[name]
		if !ok {
			return ""
		}
		dir, err := windows.KnownFolderPath(id, 0)
		if err != nil {
			return ""
		}
		return dir
	}
	p.WinAppPath = func(exe string) string {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`+exe, registry.QUERY_VALUE)
		if err != nil {
			return ""
		}
		defer k.Close()
		v, _, err := k.GetStringValue("")
		if err != nil {
			return ""
		}
		return v
	}
}
