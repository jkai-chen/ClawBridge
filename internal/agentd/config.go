package agentd

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
)

// Config is agentd's own configuration, from its root-only (0600) env file.
type Config struct {
	// ServerURL is the ClawBridge server, given to the agents as their only
	// env variable.
	ServerURL string
	// Image is the agent image, pinned by digest.
	Image string
	// LauncherToken (cbl_…) opens only the launcher endpoints. Never logged.
	LauncherToken string
	// Docker is the validating proxy's socket, never a daemon's.
	Docker string
}

var launcherTokenPattern = regexp.MustCompile(`^cbl_[A-Za-z0-9_-]{20,128}$`)

// LoadConfig reads the configuration from getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		ServerURL:     getenv("CLAWBRIDGE_SERVER_URL"),
		Image:         getenv("CLAWBRIDGE_AGENT_IMAGE"),
		LauncherToken: getenv("CLAWBRIDGE_LAUNCHER_TOKEN"),
		Docker:        getenv("CLAWBRIDGE_AGENTD_DOCKER"),
	}
	if err := validServerURL(c.ServerURL); err != nil {
		return Config{}, err
	}
	if err := validImage(c.Image); err != nil {
		return Config{}, err
	}
	if !launcherTokenPattern.MatchString(c.LauncherToken) {
		// The value is never echoed.
		return Config{}, errors.New("CLAWBRIDGE_LAUNCHER_TOKEN must be a launcher token (cbl_…)")
	}
	if err := validSocketPath(c.Docker); err != nil {
		return Config{}, fmt.Errorf("CLAWBRIDGE_AGENTD_DOCKER: %w", err)
	}
	return c, nil
}

// validServerURL accepts https, or http to this host only, with nothing
// beyond scheme, host and an optional path; the agent applies the same rule.
func validServerURL(raw string) error {
	u, err := url.Parse(raw)
	bad := err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != ""
	if !bad {
		switch u.Scheme {
		case "https":
		case "http":
			h := u.Hostname()
			bad = h != "localhost" && h != "127.0.0.1" && h != "::1"
		default:
			bad = true
		}
	}
	if bad {
		return fmt.Errorf("the server URL must be https://HOST (http only for localhost), not %q", raw)
	}
	return nil
}

// rootDaemonSockets are where the root dockerd listens. agentd and its proxy
// never talk to that daemon: it runs the api and Postgres.
var rootDaemonSockets = []string{"/var/run/docker.sock", "/run/docker.sock"}

func validSocketPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("must be an absolute, clean socket path, not %q", path)
	}
	for _, root := range rootDaemonSockets {
		if path == root {
			return fmt.Errorf("%s is the root Docker daemon's socket; use the rootless daemon of the agents' own user", path)
		}
	}
	return nil
}

// geteuid is the process's user; tests set it.
var geteuid = os.Geteuid
