package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

type terminalFile interface {
	Fd() uintptr
}

// CheckTerminal fails clearly when either stream is unsuitable for a full-screen
// interactive program. Noninteractive use should call UpdateModel or Render instead
// so no command hangs waiting for input.
func CheckTerminal(input io.Reader, output io.Writer) error {
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return errors.New("ClawBridge TUI requires an interactive terminal; TERM=dumb is not supported")
	}
	in, ok := input.(terminalFile)
	if !ok || !term.IsTerminal(in.Fd()) {
		return errors.New("ClawBridge TUI requires an interactive terminal on stdin; use the CLI for noninteractive operation")
	}
	out, ok := output.(terminalFile)
	if !ok || !term.IsTerminal(out.Fd()) {
		return errors.New("ClawBridge TUI requires an interactive terminal on stdout; use the CLI for noninteractive operation")
	}
	return nil
}

// Run starts the full-screen TUI on the process terminal. It refuses redirected
// stdin/stdout rather than entering a tea.Program that can never receive input.
func Run(ctx context.Context, cfg Config) error {
	if err := CheckTerminal(os.Stdin, os.Stdout); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cfg.Context = ctx
	m, err := New(cfg)
	if err != nil {
		return err
	}
	program := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
	)
	_, err = program.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// RunProgram runs a prevalidated model on an injected terminal pair. It is kept
// separate from Run so tests and embedders do not depend on process globals.
func RunProgram(ctx context.Context, model Model, input io.Reader, output io.Writer) error {
	if err := CheckTerminal(input, output); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	program := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithInput(input),
		tea.WithOutput(output),
	)
	_, err := program.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return fmt.Errorf("TUI stopped: %w", ctx.Err())
	}
	return err
}
