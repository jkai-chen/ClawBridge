package linearboard

import (
	"os"

	"clawbridge/internal/agentenv"
)

// agentEnviron is the environment AgentMarker reads for an app write;
// tests replace it.
var agentEnviron = os.Environ

// AgentMarker names the environment variable that says an agent runs this
// command, "" when there is none. The definition is agentenv's, and the
// marker is an accident guard, not a security boundary: an agent that clears
// it in its person's shell passes as the person.
func AgentMarker(environ []string) string { return agentenv.FromEnv(environ) }
