package runner

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"clawbridge/internal/secretenv"
)

// agentEnv are the variables the agent's claude, and with it its Bash and
// its MCP child, may take from the shell that starts it (G9-5): the
// terminal's basics and locale, the network proxies claude needs to reach
// its API, and claude's own sign-in, endpoint and model settings, named
// one by one. Never a prefix: a runner launched from inside a Claude Code
// session would otherwise pass on that session's own CLAUDE_CODE_*
// variables, its cross-session messaging socket and token among them
// (R141-M1).
// Everything else the owner's shell holds (GITHUB_TOKEN, NPM_TOKEN, AWS_*,
// SSH_AUTH_SOCK, CLAWBRIDGE_*) never reaches the session: the sandbox
// lets Bash reach the allowed registries, so a variable it could read
// could leave with a request.
var agentEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "COLORTERM", "TERM_PROGRAM",
	"LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "TMPDIR", "TZ",
	"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "all_proxy", "no_proxy",
	// claude's own: its key or sign-in token, endpoint, models and
	// configuration directory, and the Bedrock and Vertex switches (their
	// cloud credentials are not passed).
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
	"ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
}

// canaryVar is set by the launch script before it clears the environment;
// the self-test proves the session can't see it.
const canaryVar = "CLAWBRIDGE_RUNNER_CANARY"

// LaunchScript starts the session's claude with only the allow-listed
// environment. Names are written into the script, never values: /bin/sh
// expands each one in the terminal that runs it, and one that isn't set
// is left out.
func LaunchScript(canary, settingsFile, mcpFile, model, prompt string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# ClawBridge runner: start the session's claude with an allow-listed environment.\n")
	b.WriteString(canaryVar + "=" + quote(canary) + "\nexport " + canaryVar + "\n")
	b.WriteString("exec env -i")
	for _, n := range agentEnv {
		fmt.Fprintf(&b, ` ${%s+"%s=$%s"}`, n, n, n)
	}
	parts := []string{"claude", "--setting-sources", "''", "--permission-mode", "default", "--settings", quote(settingsFile), "--mcp-config", quote(mcpFile), "--strict-mcp-config"}
	if model != "" {
		parts = append(parts, "--model", quote(model))
	}
	b.WriteString(" " + strings.Join(append(parts, quote(prompt)), " ") + "\n")
	return b.String()
}

// mcpEnv is what the session's MCP child, clawbridge itself, needs beyond
// claude's environment: the owner's ClawBridge settings that aren't
// secrets, and on Linux the session bus its key store answers on. It goes
// in the MCP config, which the agent can't read, so Bash never sees it.
func mcpEnv(environ []string) map[string]any {
	out := map[string]any{}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		switch {
		case name == "DBUS_SESSION_BUS_ADDRESS", name == "XDG_RUNTIME_DIR":
			out[name] = value
		case strings.HasPrefix(name, "CLAWBRIDGE_") && name != canaryVar && !secretenv.Secret(name):
			out[name] = value
		}
	}
	return out
}

// cleanOrigin is the owner's upstream as the agent's clone may record it
// (G9-6): with no user or password, since the clone's config is the
// agent's to read. A URL with a transport the runner doesn't know, or a
// remote helper, is refused rather than copied.
func cleanOrigin(raw string) (string, error) {
	if strings.Contains(raw, "::") {
		return "", fmt.Errorf("origin uses a remote helper, which the runner doesn't copy into a clone")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("origin is not a URL the runner can read")
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git", "file":
		default:
			return "", fmt.Errorf("origin uses the %s transport, which the runner doesn't copy into a clone", u.Scheme)
		}
		u.User = nil
		return u.String(), nil
	}
	// scp-like: [user[:password]@]host:path, with the @ before any slash.
	// A local path has a slash before any colon or @.
	slash := strings.Index(raw, "/")
	if at := strings.LastIndex(raw, "@"); at > 0 && (slash < 0 || at < slash) {
		rest := raw[at+1:]
		if colon := strings.Index(rest, ":"); colon > 0 && !strings.Contains(rest[:colon], "/") {
			return rest, nil
		}
		return "", fmt.Errorf("origin has a user but no host:path, so the runner can't copy it without them")
	}
	return raw, nil
}

// environ is the launching process's environment.
func (r *Runner) environ() []string {
	if r.Environ != nil {
		return r.Environ()
	}
	return os.Environ()
}
