package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"clawbridge/internal/clirender"
	"clawbridge/internal/launchcmd"
)

// runLaunchCommand prints the shell line that starts a session's agent in
// the person's own terminal (FR44). Only the principal that owns the
// session gets one.
func runLaunchCommand(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	agent := launchcmd.ClaudeCode
	_, err := commandFlags("launch-command", args, stderr, func(f *flag.FlagSet) {
		f.StringVar(&agent, "agent", launchcmd.ClaudeCode, "claude-code or codex")
	})
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if flags.Session == "" {
		return errors.New("launch-command requires --session NAME, the session to start")
	}
	name := flags.Session
	// The reads are the person's, not the session's: no actor, no instance.
	flags.Session, flags.Instance = "", ""
	in, err := openUIInputs(ctx, flags)
	if err != nil {
		return err
	}
	defer in.Close()
	return printLaunch(ctx, newLaunchSource(flags, in), name, agent, flags.JSONFlag == "true", stdout, stderr)
}

// printLaunch builds the line and prints it, or the refusal as an error.
func printLaunch(ctx context.Context, src launchcmd.Source, name, agent string, asJSON bool, stdout, stderr io.Writer) error {
	line, err := src.Command(ctx, name, agent)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(struct {
			Source string         `json:"source"`
			Data   launchcmd.Line `json:"data"`
		}{"launch-command", line})
	}
	for _, w := range line.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", clirender.Safe(w))
	}
	_, err = fmt.Fprintln(stdout, line.Line)
	return err
}

// newLaunchSource is the lookup both launch-command and the GUI's
// sessions.launchCommand use.
func newLaunchSource(flags Config, in *uiInputs) launchcmd.Source {
	home, _ := stateHome()
	var mu sync.Mutex
	return launchcmd.Source{
		Exec: in.Executor, Workspace: in.Config.Workspace, Project: in.Config.Project, StateDir: home,
		Prepare: func(ctx context.Context, agent, session, instance, path string) (launchcmd.Prep, error) {
			mu.Lock()
			defer mu.Unlock()
			f := flags
			f.Session, f.Instance = session, instance
			return prepareLaunchMCP(ctx, f, home, agent, path)
		},
	}
}

type installOut struct {
	Changed bool           `json:"changed"`
	Entry   map[string]any `json:"entry"`
}

// prepareLaunchMCP runs `mcp install` for the session.
//
// Claude Code gets its own file at path (no hooks: those belong to a
// checkout), which is ours, so a stale one, from before the session resumed
// with a new instance, is written again. Its server is named "clawbridge";
// --mcp-config wins over a same-named project or user entry (docs/usage.md).
//
// Codex starts every entry of its config.toml, so nothing is written there:
// a dry run of `mcp install --agent codex` gives the exact server entry, and
// the line passes it with -c under the one name "clawbridge". Other entries
// that start ClawBridge are reported, since Codex would start them too.
func prepareLaunchMCP(ctx context.Context, f Config, home, agent, path string) (launchcmd.Prep, error) {
	if agent == launchcmd.Codex {
		var out bytes.Buffer
		// A path nothing is written to: the dry run reads it and stops.
		preview := filepath.Join(home, "mcp", ".codex-entry-preview.toml")
		if err := runInstall(ctx, f, []string{"--agent", "codex", "--name", "clawbridge", "--config", preview, "--dry-run"}, &out, io.Discard); err != nil {
			return launchcmd.Prep{}, err
		}
		var r installOut
		if json.Unmarshal(out.Bytes(), &r) != nil {
			return launchcmd.Prep{}, errors.New("mcp install gave an answer this command cannot read")
		}
		command, _ := r.Entry["command"].(string)
		var args []string
		list, _ := r.Entry["args"].([]any)
		for _, a := range list {
			s, ok := a.(string)
			if !ok {
				return launchcmd.Prep{}, errors.New("mcp install gave an argument this command cannot read")
			}
			args = append(args, s)
		}
		prep := launchcmd.Prep{CodexCommand: command, CodexArgs: args}
		if b, err := os.ReadFile(codexConfigPath()); err == nil {
			for _, name := range launchcmd.CodexOtherEntries(b) {
				prep.Warnings = append(prep.Warnings, launchcmd.OtherEntryWarning(name, f.Session))
			}
		}
		return prep, nil
	}
	args := []string{"--agent", "claude-code", "--config", path, "--hooks", "none"}
	check := func() (bool, error) {
		var out bytes.Buffer
		if err := runInstall(ctx, f, append(args, "--dry-run"), &out, io.Discard); err != nil {
			return false, err
		}
		var r installOut
		if json.Unmarshal(out.Bytes(), &r) != nil {
			return false, errors.New("mcp install gave an answer this command cannot read")
		}
		return r.Changed, nil
	}
	changed, err := check()
	if err != nil && strings.Contains(err.Error(), "already exists with different settings") {
		if err = os.Remove(path); err != nil {
			return launchcmd.Prep{}, err
		}
		changed, err = check()
	}
	if err != nil {
		return launchcmd.Prep{}, err
	}
	if !changed {
		return launchcmd.Prep{}, nil
	}
	return launchcmd.Prep{}, runInstall(ctx, f, args, io.Discard, io.Discard)
}

// codexConfigPath is where Codex reads its settings: $CODEX_HOME or ~/.codex.
func codexConfigPath() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		u, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(u, ".codex")
	}
	return filepath.Join(home, "config.toml")
}
