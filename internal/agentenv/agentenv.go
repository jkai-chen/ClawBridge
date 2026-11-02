// Package agentenv is the one definition of the agent marker: the
// environment variable that says an agent, not a person, runs this command.
//
// The marker is an accident guard, NOT a security boundary. It is the
// client's own word, read from its own environment, and an agent on the
// person's machine can unset these variables and pass as the person. It
// stops an agent that runs a person-only command by mistake; it does not
// stop one that means to. The server refuses a person-only operation that
// arrives with a marker, and allows one that arrives with none (an older
// client sends none).
//
// This package imports nothing from this module, so every layer may use it.
package agentenv

import (
	"os"
	"strings"
)

// Names lists the markers: CLAUDECODE (Claude Code), CODEX_* (Codex),
// CLAWBRIDGE_SESSION and CLAWBRIDGE_INSTANCE (a ClawBridge session). CODEX_
// is a prefix.
const Names = "CLAUDECODE, CODEX_*, CLAWBRIDGE_SESSION, CLAWBRIDGE_INSTANCE"

// Environ is the process environment Marker reads; tests replace it.
var Environ = os.Environ

// Marker is the name of the first agent marker variable set in the process
// environment, "" for none. Never its value.
func Marker() string { return FromEnv(Environ()) }

// FromEnv is Marker over an environment in os.Environ's form. A variable
// counts when it is set, even to an empty value.
func FromEnv(environ []string) string {
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if Known(name) {
			return name
		}
	}
	return ""
}

// Known reports whether name is an agent marker variable name.
func Known(name string) bool {
	return name == "CLAUDECODE" || strings.HasPrefix(name, "CODEX_") || name == "CLAWBRIDGE_SESSION" || name == "CLAWBRIDGE_INSTANCE"
}

// Display is the text a message may say for a marker name received from a
// client: a known name as itself (any CODEX_ name as CODEX_*), anything else
// as a generic phrase, so a refusal never echoes an arbitrary string.
func Display(name string) string {
	switch {
	case strings.HasPrefix(name, "CODEX_"):
		return "CODEX_*"
	case Known(name):
		return name
	}
	return "an agent environment variable"
}
