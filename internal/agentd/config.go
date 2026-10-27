package agentd

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Spec is what the fixed container specs are built from, shared by agentd
// and its socket proxy.
type Spec struct {
	// ServerURL is the ClawBridge server, given to the agents as their only
	// env variable.
	ServerURL string
	// AgentImage and EgressImage are pinned by repo digest or image ID.
	AgentImage, EgressImage string
	// PolicyDir holds the egress policy agentd writes; the egress container
	// mounts it read-only.
	PolicyDir string
	// PrivateServer is the server's host:port when the operator listed it as
	// private (CLAWBRIDGE_EGRESS_PRIVATE_SERVER), else "".
	PrivateServer string
}

func loadSpec(getenv func(string) string) (Spec, error) {
	s := Spec{
		ServerURL:   getenv("CLAWBRIDGE_SERVER_URL"),
		AgentImage:  getenv("CLAWBRIDGE_AGENT_IMAGE"),
		EgressImage: getenv("CLAWBRIDGE_EGRESS_IMAGE"),
		PolicyDir:   getenv("CLAWBRIDGE_EGRESS_POLICY_DIR"),
	}
	if err := validServerURL(s.ServerURL); err != nil {
		return Spec{}, err
	}
	_, private, err := serverTarget(s.ServerURL, getenv("CLAWBRIDGE_EGRESS_PRIVATE_SERVER"))
	if err != nil {
		return Spec{}, err
	}
	s.PrivateServer = private
	for _, image := range []string{s.AgentImage, s.EgressImage} {
		if err := validImage(image); err != nil {
			return Spec{}, err
		}
	}
	if !filepath.IsAbs(s.PolicyDir) || filepath.Clean(s.PolicyDir) != s.PolicyDir || strings.ContainsAny(s.PolicyDir, ":,") {
		return Spec{}, fmt.Errorf("CLAWBRIDGE_EGRESS_POLICY_DIR must be an absolute, clean directory, not %q", s.PolicyDir)
	}
	return s, nil
}

// serverTarget is the server's host:port for the egress policy, and private
// is that same target when the operator listed it in privateServer (Q2).
// By default the server must be public to the agents, as the egress proxy
// requires, so check refuses a private address up front rather than every
// round failing. Listing the server lets it be, or resolve to, a private or
// CGNAT address (a server on the LAN or a VPN), never loopback, link-local
// or agentd's own subnets.
func serverTarget(serverURL, privateServer string) (target, private string, err error) {
	u, err := url.Parse(serverURL)
	if err != nil || u.Scheme != "https" {
		return "", "", fmt.Errorf("the agents reach the server only over https, not %q", serverURL)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if target, err = normalTarget(net.JoinHostPort(u.Hostname(), port)); err != nil {
		return "", "", err
	}
	ip, ipErr := netip.ParseAddr(hostOf(target))
	if privateServer == "" {
		if ipErr == nil && !publicAddr(ip) {
			return "", "", fmt.Errorf("the agents can't reach the server at %s: the egress proxy allows public addresses only, "+
				"unless CLAWBRIDGE_EGRESS_PRIVATE_SERVER lists the server's host:port", ip)
		}
		return target, "", nil
	}
	listed, err := normalTarget(privateServer)
	if err != nil || listed != target {
		return "", "", fmt.Errorf("CLAWBRIDGE_EGRESS_PRIVATE_SERVER must be the server's own host:port, %s", target)
	}
	if ipErr == nil && publicAddr(ip) {
		return "", "", errors.New("CLAWBRIDGE_EGRESS_PRIVATE_SERVER lists a public address: leave it unset")
	}
	if ipErr == nil && !privateServerAddr(ip) {
		return "", "", fmt.Errorf("the server at %s can't be reached through the egress proxy: loopback, link-local and agentd's own subnets never are", ip)
	}
	return target, target, nil
}

// Config is agentd's own configuration, from its root-only (0600) env file.
type Config struct {
	Spec
	// LauncherToken (cbl_…) opens only the launcher endpoints. Never logged.
	LauncherToken string
	// Docker is the validating proxy's socket, never a daemon's.
	Docker string
}

var launcherTokenPattern = regexp.MustCompile(`^cbl_[A-Za-z0-9_-]{20,128}$`)

// LoadConfig reads the configuration from getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	spec, err := loadSpec(getenv)
	if err != nil {
		return Config{}, err
	}
	c := Config{Spec: spec, LauncherToken: getenv("CLAWBRIDGE_LAUNCHER_TOKEN"), Docker: getenv("CLAWBRIDGE_AGENTD_DOCKER")}
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
