package launchcmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"clawbridge/internal/bus"
	"github.com/pelletier/go-toml/v2"
)

// Executor is the part of the client's executor the lookup reads through.
type Executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}

// Source finds a session and its owner through the existing reads
// (list_sessions and whoami) and builds its line.
type Source struct {
	Exec      Executor
	Workspace string
	Project   string
	// StateDir is the client's state directory; per-session MCP files live in
	// <StateDir>/mcp.
	StateDir string
	// Prepare makes the agent's MCP side ready for the session, through the
	// code `mcp install` runs. For claude-code it writes the per-session file
	// at path if it is missing or out of date. For codex path is empty and
	// nothing is written: it answers the MCP server the line passes to Codex.
	Prepare func(ctx context.Context, agent, session, instance, path string) (Prep, error)
}

// Line is a launch command and what the page shows beside it.
type Line struct {
	Line    string `json:"line"`
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Host    string `json:"host,omitempty"`
	// Warnings are things to fix so this agent acts only as this session.
	Warnings []string `json:"warnings,omitempty"`
}

// Prep is what Prepare found.
type Prep struct {
	// CodexCommand and CodexArgs are the server `mcp install --agent codex`
	// would write for the session.
	CodexCommand string
	CodexArgs    []string
	Warnings     []string
}

type sessionRow struct {
	Name     string `json:"name"`
	Instance string `json:"instance"`
	Owner    string `json:"owner_principal"`
	Host     string `json:"host"`
}

// Command builds the line for session and agent. It refuses a session the
// viewer's principal does not own before anything is written.
func (s Source) Command(ctx context.Context, session, agent string) (Line, error) {
	if session == "" {
		return Line{}, errors.New("a session name is required")
	}
	if agent != ClaudeCode && agent != Codex {
		return Line{}, fmt.Errorf("unknown agent %q: use claude-code or codex", printable(agent))
	}
	viewer, err := s.viewer(ctx)
	if err != nil {
		return Line{}, err
	}
	row, err := s.find(ctx, session)
	if err != nil {
		return Line{}, err
	}
	in := Input{Agent: agent, Session: row.Name, Instance: row.Instance, OwnerPrincipal: row.Owner, ViewerPrincipal: viewer}
	// Refuse before anything is written.
	if err = authorize(row.Owner, viewer); err != nil {
		return Line{}, err
	}
	out := Line{Agent: agent, Session: row.Name, Host: row.Host}
	if agent == ClaudeCode {
		in.MCPConfigPath = ConfigPath(s.StateDir, row.Name)
	}
	if s.Prepare != nil {
		prep, err := s.Prepare(ctx, agent, row.Name, row.Instance, in.MCPConfigPath)
		if err != nil {
			return Line{}, err
		}
		in.CodexCommand, in.CodexArgs, out.Warnings = prep.CodexCommand, prep.CodexArgs, prep.Warnings
	}
	if out.Line, err = Build(in); err != nil {
		return Line{}, err
	}
	return out, nil
}

func (s Source) viewer(ctx context.Context) (string, error) {
	res, err := s.Exec.Execute(ctx, bus.Request{Operation: "whoami", Workspace: s.Workspace, Project: s.Project, Args: map[string]any{}})
	if err != nil {
		return "", err
	}
	var who struct {
		Principal string `json:"principal"`
	}
	if err = remarshal(res.Data, &who); err != nil || who.Principal == "" {
		return "", errors.New("could not tell who you are, so no launch command is given")
	}
	return who.Principal, nil
}

func (s Source) find(ctx context.Context, name string) (sessionRow, error) {
	var after int64
	for page := 0; page < 50; page++ {
		res, err := s.Exec.Execute(ctx, bus.Request{Operation: "list_sessions", Workspace: s.Workspace, Project: s.Project, Args: map[string]any{"limit": 100, "after": after}})
		if err != nil {
			return sessionRow{}, err
		}
		var data struct {
			Sessions []sessionRow `json:"sessions"`
			Next     int64        `json:"next_cursor"`
			More     bool         `json:"has_more"`
		}
		if err = remarshal(res.Data, &data); err != nil {
			return sessionRow{}, err
		}
		for _, r := range data.Sessions {
			if r.Name == name {
				return r, nil
			}
		}
		if !data.More {
			break
		}
		after = data.Next
	}
	return sessionRow{}, fmt.Errorf("no session named %q in this project", printable(name))
}

func remarshal(from, to any) error {
	b, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, to)
}

var unsafeSlug = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Slug is a session name made safe for a file name and for an MCP entry
// name ([A-Za-z0-9_-]). A name that had to change gets a short hash so two
// names never share a file.
func Slug(session string) string {
	slug := unsafeSlug.ReplaceAllString(session, "-")
	if slug == session && slug != "" {
		return slug
	}
	sum := sha256.Sum256([]byte(session))
	if len(slug) > 60 {
		slug = slug[:60]
	}
	return slug + "-" + hex.EncodeToString(sum[:3])
}

// ConfigPath is the per-session MCP file: <stateDir>/mcp/<slug>.json.
func ConfigPath(stateDir, session string) string {
	return filepath.Join(stateDir, "mcp", Slug(session)+".json")
}

// CodexOtherEntries names the entries of a Codex config.toml, other than
// the fixed "clawbridge", whose command is the clawbridge binary. Codex
// starts every entry, so each is one more agent identity in the session.
func CodexOtherEntries(config []byte) []string {
	var doc struct {
		Servers map[string]struct {
			Command string `toml:"command"`
		} `toml:"mcp_servers"`
	}
	if toml.Unmarshal(config, &doc) != nil {
		return nil
	}
	var names []string
	for name, srv := range doc.Servers {
		base := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(srv.Command, `\`, "/"))), ".exe")
		if name != "clawbridge" && base == "clawbridge" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// OtherEntryWarning is what the drawer and the CLI say about one such entry.
func OtherEntryWarning(name, session string) string {
	return "Your Codex config also starts ClawBridge as " + printable(name) + "; remove it so this agent acts only as " + printable(session) + "."
}
