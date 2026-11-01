// Package launchcmd builds the shell line that starts a session's agent in
// the person's own terminal (FR44). The CLI's launch-command and the GUI's
// sessions.launchCommand both call it, so the two cannot drift.
//
// The line holds only non-secret names: CLAWBRIDGE_SESSION,
// CLAWBRIDGE_INSTANCE and the agent with its MCP flag. It is refused for a
// session another principal owns; the server refuses that too, so this is
// the first guard, not the only one.
package launchcmd

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Agents the line can start.
const (
	ClaudeCode = "claude-code"
	Codex      = "codex"
)

// Input is everything a line is built from.
type Input struct {
	// Agent is "claude-code" or "codex".
	Agent string
	// Session and Instance name the session being started.
	Session, Instance string
	// MCPConfigPath is the per-session MCP file; used by claude-code only.
	MCPConfigPath string
	// CodexCommand and CodexArgs are the MCP server `mcp install --agent codex`
	// would write, passed to Codex on the line; codex only.
	CodexCommand string
	CodexArgs    []string
	// OwnerPrincipal owns the session; ViewerPrincipal is the person asking.
	OwnerPrincipal, ViewerPrincipal string
}

// Build returns the shell-quoted line, or says why there is none.
func Build(in Input) (string, error) {
	if in.Agent != ClaudeCode && in.Agent != Codex {
		return "", fmt.Errorf("unknown agent %q: use claude-code or codex", printable(in.Agent))
	}
	if err := authorize(in.OwnerPrincipal, in.ViewerPrincipal); err != nil {
		return "", err
	}
	if in.Session == "" || in.Instance == "" {
		return "", errors.New("the session and its instance are both needed for a launch command")
	}
	line := "CLAWBRIDGE_SESSION=" + Quote(in.Session) + " CLAWBRIDGE_INSTANCE=" + Quote(in.Instance)
	if in.Agent == Codex {
		// Codex starts every [mcp_servers.*] entry of its config.toml, so a
		// per-session entry there would run next to the others and act as
		// all of them. The server goes on the line instead, with -c
		// overrides of the one fixed name; an existing entry of that name
		// has its command and args replaced (verified on codex-cli 0.163.0).
		if in.CodexCommand == "" || len(in.CodexArgs) == 0 {
			return "", errors.New("codex needs the MCP server's command and arguments")
		}
		return line + " codex -c " + Quote(codexKey+".command="+TOMLString(in.CodexCommand)) +
			" -c " + Quote(codexKey+".args="+TOMLArray(in.CodexArgs)), nil
	}
	if in.MCPConfigPath == "" {
		return "", errors.New("claude-code needs the session's MCP config path")
	}
	return line + " claude --mcp-config " + Quote(in.MCPConfigPath), nil
}

// codexKey is the one server name the line uses.
const codexKey = "mcp_servers.clawbridge"

// TOMLString is s as a TOML basic string: quotes, backslashes and control
// characters escaped, everything else (Unicode included) as is.
func TOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TOMLArray is a TOML array of basic strings.
func TOMLArray(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = TOMLString(it)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// authorize is the owner rule: only the principal that owns a session can
// start its agent.
func authorize(owner, viewer string) error {
	if owner == "" || viewer == "" {
		return errors.New("cannot tell who owns this session, so no launch command is given")
	}
	if owner != viewer {
		return fmt.Errorf("only %s can start this agent", printable(owner))
	}
	return nil
}

// Quote is POSIX single-quoting: safe words stay bare, anything else is
// wrapped in single quotes with each ' written as '\”. Nothing inside
// single quotes is expanded, newlines included.
func Quote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !safeRune(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func safeRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("_@%+=:,./-", r)
}

// printable drops control and format characters so a hostile name cannot
// act on a terminal when an error is printed.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, s)
}
