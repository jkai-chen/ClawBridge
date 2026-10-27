// Package clirender is the CLI's one output path (live/CLI-OUTPUT-PLAN.md):
// today's JSON, byte for byte, for --json, agents and anything that isn't a
// terminal, and a human layout for a person at one. Commands hand it the
// value they used to encode; they never format output themselves.
package clirender

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
)

// Mode is how a command's result is written.
type Mode int

const (
	// JSON is today's output: the value, as json.Encoder writes it.
	JSON Mode = iota
	// Human is the layout for a person at a terminal.
	Human
)

// Flag is the --json flag as given: unset, or true or false.
type Flag int

const (
	FlagUnset Flag = iota
	FlagTrue
	FlagFalse
)

// agentMarkers are the environment variables an agent's shell sets (M1): an
// agent in a PTY still gets JSON, whatever its skill says. Claude Code sets
// CLAUDECODE in its shells, the runner's sessions included (the runner
// passes no CLAWBRIDGE_* variable to them, G9-5).
var agentMarkers = []string{"CLAUDECODE"}

// agentPrefixes are marker prefixes: Codex sets CODEX_* variables
// (CODEX_SANDBOX and others) in the shells it runs.
var agentPrefixes = []string{"CODEX_"}

// ErrOutputEnv is CLAWBRIDGE_OUTPUT set to neither json nor human (L5).
var ErrOutputEnv = errors.New("CLAWBRIDGE_OUTPUT must be json or human")

// Choose picks the mode, the first rule that applies winning (§2): the
// --json flag, CLAWBRIDGE_OUTPUT, an agent's shell, then whether stdout is a
// terminal. environ is os.Environ's form.
func Choose(flag Flag, environ []string, stdout io.Writer) (Mode, error) {
	switch flag {
	case FlagTrue:
		return JSON, nil
	case FlagFalse:
		return Human, nil
	}
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	if v, ok := env["CLAWBRIDGE_OUTPUT"]; ok && v != "" {
		switch v {
		case "json":
			return JSON, nil
		case "human":
			return Human, nil
		}
		return JSON, ErrOutputEnv
	}
	if agentShell(env) {
		return JSON, nil
	}
	if isTerminal(stdout) {
		return Human, nil
	}
	return JSON, nil
}

// isTerminal reports whether w is a terminal; tests replace it.
var isTerminal = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

func agentShell(env map[string]string) bool {
	for _, k := range agentMarkers {
		if env[k] != "" {
			return true
		}
	}
	for k, v := range env {
		for _, p := range agentPrefixes {
			if strings.HasPrefix(k, p) && v != "" {
				return true
			}
		}
	}
	return false
}

type modeKey struct{}

// WithMode carries the chosen mode to the code that writes the result.
func WithMode(ctx context.Context, m Mode) context.Context {
	return context.WithValue(ctx, modeKey{}, m)
}

// ModeOf is the mode ctx carries; JSON when it carries none, so a path that
// was never given one keeps today's output.
func ModeOf(ctx context.Context) Mode {
	m, _ := ctx.Value(modeKey{}).(Mode)
	return m
}
