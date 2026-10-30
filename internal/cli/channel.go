package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"clawbridge/internal/buildinfo"
	"clawbridge/internal/channel"
	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/notify"
)

const channelUsage = `usage:
  clawbridge channel serve
      The channel server a ClawBridge Claude Code plugin starts (FR26): an
      MCP server on stdin and stdout that wakes the idle agent of
      CLAWBRIDGE_SESSION when its messages wait. It pushes only a fixed
      notice and a count; the agent reads its mail with the inbox tool.
  clawbridge channel allow [--write [--force]]
      The one-time admin step per machine: shows the managed-settings
      drop-in that lets Claude Code take ClawBridge's channel, and its
      warnings. --write writes it after you type yes at a terminal, and
      needs an administrator (sudo); a different drop-in already there is
      replaced only with --force. Never an agent's step.
  clawbridge channel setup
      For you, not an agent: shows the two claude plugin commands that
      install the ClawBridge plugin, runs them if you say yes at a
      terminal, then shows the admin step and the launch line.`

// channelStdin is what the channel server reads.
var channelStdin io.Reader = os.Stdin

// runChannel runs `clawbridge channel serve|allow|setup`. The server's
// scope comes from the environment the plugin passes, never from flags;
// session is a global --session, which only setup refuses.
func runChannel(ctx context.Context, args []string, session string, stdout io.Writer) error {
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") || len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(stdout, channelUsage+"\n")
		return err
	}
	if len(args) > 0 && args[0] == "allow" {
		return runChannelAllow(args[1:], stdout)
	}
	if len(args) > 0 && args[0] == "setup" {
		return runChannelSetup(ctx, args[1:], session, stdout)
	}
	if len(args) != 1 || args[0] != "serve" {
		return errors.New(channelUsage)
	}
	return channel.Serve(ctx, channelStdin, stdout, channel.Config{
		Dir:     contextDirOrEmpty(),
		Version: buildinfo.Current().Version,
		Route: func(ctx context.Context, r ctxwatch.QueueRecord) (notify.Executor, func(), error) {
			return queueHookRoute(ctx, r)
		},
	})
}

// The machine the allow and setup steps look at, and how they run claude:
// variables, so tests use a temporary root and never run claude.
var (
	channelManagedDir = func() string { return channel.ManagedSettingsDir(runtime.GOOS) }
	channelGOOS       = runtime.GOOS
	channelClaude     = func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		path, err := exec.LookPath("claude")
		if err != nil {
			return errors.New("claude is not on PATH; run the commands yourself")
		}
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
		return cmd.Run()
	}
)

// allowWriteHint is how to run --write as an administrator on this OS.
func allowWriteHint() string {
	if channelGOOS == "windows" {
		return "To write it, in a terminal run as administrator: clawbridge channel allow --write"
	}
	return `To write it: sudo "$(command -v clawbridge)" channel allow --write`
}

// runChannelAllow runs `channel allow [--write [--force]]` (FR26 D1). It
// never raises its own privileges and writes no other managed file.
func runChannelAllow(args []string, stdout io.Writer) error {
	write, force := false, false
	for _, a := range args {
		switch a {
		case "--write":
			write = true
		case "--force":
			force = true
		default:
			return errors.New(channelUsage)
		}
	}
	if force && !write {
		return errors.New("--force only goes with --write")
	}
	if marker := agentMarker(closesEnv()); write && marker != "" {
		return fmt.Errorf("writing machine-wide managed settings is a person's step, and %s says an agent runs this command; run it yourself in a terminal", marker)
	}
	root := channelManagedDir()
	path := filepath.Join(root, "managed-settings.d", channel.AllowFileName)
	fmt.Fprintf(stdout, "The admin step that lets Claude Code take ClawBridge's channel on this machine (FR26).\nDrop-in: %s\n\n%s\n", path, channel.AllowJSON)
	fmt.Fprintf(stdout, "Warnings:\n"+
		"- It is machine-wide: it applies to every Claude Code user on this machine.\n"+
		"- allowedChannelPlugins replaces Anthropic's default channel list when no other managed list exists, so it also lists Anthropic's four official channel plugins (discord, telegram, fakechat, imessage).\n"+
		"- Anthropic may change its default list later; this file won't follow.\n"+
		"- To undo it, delete %s\n", path)
	if !write {
		_, err := fmt.Fprintln(stdout, allowWriteHint())
		return err
	}
	if err := confirmAllowWrite(path, stdout); err != nil {
		return err
	}
	w, err := channel.WriteAllow(root, force)
	switch {
	case errors.Is(err, channel.ErrAllowDiffers):
		fmt.Fprintf(stdout, "A different drop-in is already at %s:\n%s", w.Path, w.Diff)
		return errors.New("nothing written: run again with --force to replace it")
	case errors.Is(err, channel.ErrAllowNotPermitted):
		return fmt.Errorf("%v; run it as an administrator: %s", err, strings.TrimPrefix(allowWriteHint(), "To write it: "))
	case err != nil:
		return err
	case !w.Written:
		_, err = fmt.Fprintf(stdout, "%s is already in place; nothing written.\n", w.Path)
		return err
	}
	_, err = fmt.Fprintf(stdout, "Written: %s\n", w.Path)
	return err
}

// confirmAllowWrite asks a person at a terminal to type yes before --write
// changes the drop-in. The agent markers alone don't hold: sudo's
// env_reset drops them, and sudo is how --write is run. An agent's tool
// shell has no terminal, and --force doesn't skip the question.
func confirmAllowWrite(path string, stdout io.Writer) error {
	if !stdinTerminal() {
		return errors.New("--write needs a person at a terminal to type the confirmation, and stdin is not a terminal; nothing written")
	}
	fmt.Fprintf(stdout, "This writes %s, machine-wide: every Claude Code user on this machine gets it.\nType yes to write it: ", path)
	line, err := bufio.NewReader(closesStdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no confirmation was typed; nothing written")
	}
	if strings.TrimSpace(line) != "yes" {
		return errors.New("the answer was not yes; nothing written")
	}
	return nil
}

// channelMissing is what is left to install the plugin: the `claude`
// commands (argv after claude) for what Claude Code's settings don't show.
func channelMissing(project string) [][]string {
	config, err := ctxwatch.ClaudeConfigDir()
	if err != nil {
		return channel.SetupCommands
	}
	st := channel.DetectPlugin(config, project)
	var missing [][]string
	if !st.Marketplace {
		missing = append(missing, channel.SetupCommands[0])
	}
	if !st.Enabled {
		missing = append(missing, channel.SetupCommands[1])
	}
	return missing
}

// writeChannelSteps lists, read only, the channel setup still to do for a
// Claude Code session, then its launch line (FR26).
func writeChannelSteps(w io.Writer, project, session, instance string) {
	missing := channelMissing(project)
	allowed, _, allowErr := channel.AllowlistedIn(channelManagedDir())
	if len(missing) == 0 && allowed {
		fmt.Fprintln(w, "Channel setup (FR26): the plugin is installed and enabled, and this machine allowlists it.")
	} else {
		fmt.Fprintln(w, "Channel setup still to do (FR26):")
		for _, m := range missing {
			fmt.Fprintln(w, "  claude "+strings.Join(m, " "))
		}
		switch {
		case allowErr != nil:
			fmt.Fprintf(w, "  the admin step: the managed settings could not be read (%v); see clawbridge channel allow\n", allowErr)
		case !allowed:
			fmt.Fprintln(w, "  the admin step, once per machine: clawbridge channel allow")
		}
	}
	fmt.Fprintf(w, "Then start the agent with:\n  %s\n", channel.LaunchLine(session, instance))
}

// runChannelSetup runs `channel setup`: the person's step, never an
// agent's. It runs the claude plugin commands only after a yes typed at a
// terminal, exec'd from PATH with no shell.
func runChannelSetup(ctx context.Context, args []string, session string, stdout io.Writer) error {
	for _, a := range args {
		if a == "--session" || strings.HasPrefix(a, "--session=") {
			session = a
		}
	}
	if session != "" {
		return errors.New("channel setup is never run with --session; run it yourself in a terminal")
	}
	if len(args) != 0 {
		return errors.New(channelUsage)
	}
	if marker := agentMarker(closesEnv()); marker != "" {
		return fmt.Errorf("setting up the channel is a person's step, and %s says an agent runs this command; run it yourself in a terminal", marker)
	}
	missing := channelMissing("")
	if len(missing) == 0 {
		fmt.Fprintln(stdout, "The ClawBridge plugin is installed and enabled.")
	} else {
		fmt.Fprintln(stdout, "These commands install the ClawBridge plugin for Claude Code:")
		for _, m := range missing {
			fmt.Fprintln(stdout, "  claude "+strings.Join(m, " "))
		}
		if !stdinTerminal() {
			fmt.Fprintln(stdout, "Stdin is not a terminal, so nothing was run: run them yourself.")
		} else {
			fmt.Fprint(stdout, "Run them now? [y/N] ")
			answer, _ := bufio.NewReader(closesStdin).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(answer)); a == "y" || a == "yes" {
				for _, m := range missing {
					if err := channelClaude(ctx, m, stdout, stdout); err != nil {
						return fmt.Errorf("claude %s failed: %v", strings.Join(m, " "), err)
					}
				}
			} else {
				fmt.Fprintln(stdout, "Nothing run.")
			}
		}
	}
	if allowed, where, _ := channel.AllowlistedIn(channelManagedDir()); allowed {
		fmt.Fprintf(stdout, "This machine allowlists the channel in %s.\n", where)
	} else {
		fmt.Fprintln(stdout, "Then the admin step, once per machine: clawbridge channel allow (it shows the drop-in; --write with sudo writes it).")
	}
	_, err := fmt.Fprintf(stdout, "Start each agent with its own session and instance:\n  %s\n", channel.LaunchLine("<session>", "<instance>"))
	return err
}
