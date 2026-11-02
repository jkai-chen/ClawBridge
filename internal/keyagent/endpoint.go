package keyagent

// Endpoint is where the agent for stateDir listens: a socket path on Unix
// or a named pipe on Windows. It checks the directory that holds a socket
// when that directory exists, and makes nothing.
func Endpoint(stateDir string) (string, error) {
	state, err := canonicalState(stateDir)
	if err != nil {
		return "", err
	}
	return endpointFor(state, false)
}
