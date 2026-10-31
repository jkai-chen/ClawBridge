//go:build !windows

package cli

// SPIKE: the backend's PTY on macOS, Linux and WSL.

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type guiPTY struct {
	f   *os.File
	cmd *exec.Cmd
}

func startGUIPTY(cmd *exec.Cmd, cols, rows int) (*guiPTY, error) {
	size := &pty.Winsize{Cols: 80, Rows: 24}
	if cols > 0 && rows > 0 {
		size = &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}
	}
	f, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, err
	}
	return &guiPTY{f: f, cmd: cmd}, nil
}

func (t *guiPTY) Read(b []byte) (int, error)  { return t.f.Read(b) }
func (t *guiPTY) Write(b []byte) (int, error) { return t.f.Write(b) }
func (t *guiPTY) Resize(cols, rows int) error {
	return pty.Setsize(t.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}
func (t *guiPTY) Close() error {
	_ = t.cmd.Process.Kill()
	_ = t.cmd.Wait()
	return t.f.Close()
}
