// Package linearkey keeps a person's Linear API key and is the only code
// that sends it (live/LINEAR-VIEWS-PLAN.md §11.3, §11.5 M1, review K1–K7).
// The key acts as the person everywhere in Linear, so it is held tightly:
//   - only the CLI's `linear key` and `linear view` commands may read it: a
//     process that entered any other way (mcp, runner, serve, notify, ...)
//     is marked key-forbidden at its start, a mark that is never cleared
//     (K1), and a package test proves the MCP server, the runner and the
//     server don't import this package;
//   - it is never read under an agent's environment (K2, best effort);
//   - it is sent only with the GraphQL texts in this package's allow-list;
//   - it is never in argv, the environment, logs or errors.
package linearkey

import (
	"errors"
	"os"
	"strings"
	"sync/atomic"
)

var forbidden atomic.Bool

// Forbid marks this process as one that never reads the key. It can't be
// undone.
func Forbid() { forbidden.Store(true) }

// Forbidden reports whether Forbid was called.
func Forbidden() bool { return forbidden.Load() }

// environ is the process environment; tests replace it.
var environ = os.Environ

// ErrForbidden is the refusal in a process that may not read the key.
var ErrForbidden = errors.New("the Linear API key is read only by clawbridge linear key and linear view, run by a person in their own terminal")

// AgentMarker names the environment variable that says an agent runs this
// process: Claude Code's, Codex's, or a ClawBridge session's; "" for none.
func AgentMarker(env []string) string {
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == "CLAUDECODE" || strings.HasPrefix(name, "CODEX_") || name == "CLAWBRIDGE_SESSION" || name == "CLAWBRIDGE_INSTANCE" {
			return name
		}
	}
	return ""
}

// Allowed is guard, for a command to refuse before it asks for a key.
func Allowed() error { return guard() }

// guard refuses a key-forbidden process, and one an agent runs.
func guard() error {
	if Forbidden() {
		return ErrForbidden
	}
	if marker := AgentMarker(environ()); marker != "" {
		return errors.New("the Linear API key is the project owner's, and " + marker + " says an agent runs this command; the project owner runs this in their own terminal")
	}
	return nil
}
