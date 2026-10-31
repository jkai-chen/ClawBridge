//go:build windows

package cli

// SPIKE: native Windows ConPTY is not in this spike (Q4 runs in WSL).

import (
	"errors"
	"os/exec"
)

type guiPTY struct{}

func startGUIPTY(*exec.Cmd, int, int) (*guiPTY, error) {
	return nil, errors.New("terminals on native Windows are not in this spike")
}

func (t *guiPTY) Read([]byte) (int, error)  { return 0, errors.New("unsupported") }
func (t *guiPTY) Write([]byte) (int, error) { return 0, errors.New("unsupported") }
func (t *guiPTY) Resize(int, int) error     { return errors.New("unsupported") }
func (t *guiPTY) Close() error              { return nil }
