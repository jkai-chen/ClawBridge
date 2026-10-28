package linearboard

import (
	"os"
	"strings"
)

// agentEnviron is the environment AgentMarker reads for an app write;
// tests replace it.
var agentEnviron = os.Environ

// AgentMarker names the environment variable that says an agent runs this
// command: Claude Code's, Codex's, or a ClawBridge session's. "" when there
// is none. It is the client's own word: an agent that clears it in its
// person's shell passes as the person.
func AgentMarker(environ []string) string {
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if name == "CLAUDECODE" || strings.HasPrefix(name, "CODEX_") || name == "CLAWBRIDGE_SESSION" || name == "CLAWBRIDGE_INSTANCE" {
			return name
		}
	}
	return ""
}
