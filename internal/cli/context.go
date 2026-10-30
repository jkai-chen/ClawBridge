package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"clawbridge/internal/channel"
	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/doctor"
)

const contextUsage = `usage:
  clawbridge context show [--session NAME] [--json]
      How much context the session's agent is using, read from the agent's
      own transcript (the session's binding record; NAME defaults to
      CLAWBRIDGE_SESSION).
  clawbridge context show --agent claude-code|codex --transcript PATH [--json]
      The same for one transcript, named explicitly.
  clawbridge context bind [--session NAME] --codex-thread UUID
  clawbridge context bind [--session NAME] --agent claude-code|codex --transcript PATH
      Bind the session to its transcript: a Codex thread's rollout under
      CODEX_HOME, or the file named.
  clawbridge context hooks install|uninstall [--scope local|user|project-shared]
      [--confirm-shared] [--dry-run] [--channel]
      Claude Code hooks that bind the session when it starts and note each
      compaction; install --channel adds the turn hook the ClawBridge
      channel needs, and uninstall removes it too. local (the default) is the project's
      .claude/settings.local.json, refused when git tracks it; user is your
      own settings.json; project-shared is the committed .claude/settings.json
      and needs --confirm-shared. The change is shown before it is written,
      and uninstall removes only ClawBridge's own entries.
  clawbridge context levels set [--warn-tokens N] [--handover-tokens N] | --clear
      Lower this session's own levels below the project's, or go back to
      the project's (needs the server and the session).
  clawbridge context bind|compacting --from-hook
      Run by those hooks, with the hook's JSON on stdin. They always exit 0.
Counts and times only: nothing from the transcript's content is shown.`

// contextDir is where binding records live.
func contextDir() (string, error) {
	home, err := stateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "context"), nil
}

// contextDirOrEmpty is contextDir, or "" when there is no state home, which
// leaves context reporting off.
func contextDirOrEmpty() string {
	dir, err := contextDir()
	if err != nil {
		return ""
	}
	return dir
}

// contextArgs are a context subcommand's arguments with a global --session
// passed on to the subcommands that take one; the subcommand's own
// --session, parsed later, still wins.
func contextArgs(args []string, session string) []string {
	if session == "" || len(args) == 0 {
		return args
	}
	switch args[0] {
	case "show", "bind", "compacting":
		return append([]string{args[0], "--session", session}, args[1:]...)
	}
	return args
}

// runContext runs `clawbridge context …`. It reads local files only, so it
// needs no configuration or route.
func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(contextUsage)
	}
	switch args[0] {
	case "show":
	case "bind":
		return runContextBind(ctx, args[1:], stdout, stderr)
	case "compacting":
		return runContextCompacting(ctx, args[1:], stderr)
	case "hooks":
		return runContextHooks(args[1:], stdout, stderr)
	default:
		return errors.New(contextUsage)
	}
	f := flag.NewFlagSet("context show", flag.ContinueOnError)
	f.SetOutput(stderr)
	f.Usage = func() { fmt.Fprintln(stderr, contextUsage) }
	session := f.String("session", os.Getenv("CLAWBRIDGE_SESSION"), "session name")
	agent := f.String("agent", "", "claude-code or codex, with --transcript")
	transcript := f.String("transcript", "", "a transcript path, with --agent")
	asJSON := f.Bool("json", false, "JSON output")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || (*transcript == "") != (*agent == "") {
		return errors.New(contextUsage)
	}
	var r ctxwatch.Reading
	path := *transcript
	if path == "" {
		if *session == "" {
			return errors.New("name the session with --session (or CLAWBRIDGE_SESSION), or a transcript with --agent and --transcript")
		}
		dir, err := contextDir()
		if err != nil {
			return err
		}
		b, bound, err := ctxwatch.LoadBinding(dir, *session)
		if err != nil {
			return err
		}
		if !bound {
			if *asJSON {
				return emit(ctx, stdout, "context_show", map[string]any{"session": *session, "bound": false})
			}
			_, err := fmt.Fprintf(stdout, "context: %s is not bound to a transcript\n", *session)
			return err
		}
		// A binding is read as it was bound, and checked again (ReadBound).
		path = b.Transcript
		if r, err = ctxwatch.ReadBound(b); err != nil {
			return err
		}
	} else {
		var err error
		if path, err = filepath.Abs(path); err != nil {
			return err
		}
		if r, err = ctxwatch.Read(*agent, path); err != nil {
			return err
		}
	}
	if *asJSON {
		return emit(ctx, stdout, "context_show", struct {
			ctxwatch.Reading
			Transcript string `json:"transcript"`
		}{r, path})
	}
	_, err := fmt.Fprintln(stdout, describeReading(r)+"\ntranscript: "+path)
	return err
}

// describeReading is one line of counts: "context: 612k tokens (window
// 258k), 2 compactions in the part read".
func describeReading(r ctxwatch.Reading) string {
	if r.Status != ctxwatch.StatusOK {
		return "context: unsupported transcript (" + r.Reason + ")"
	}
	s := "context: " + kTokens(r.Tokens) + " tokens"
	switch {
	case r.UsageAt == "" && r.Compactions > 0:
		s = "context: unknown until the first model call after the compaction"
	case r.UsageAt == "":
		s = "context: no model call yet"
	case r.Window > 0:
		s += " (window " + kTokens(r.Window) + ")"
	}
	// A partial read may have missed earlier compactions.
	least := ""
	if r.Partial {
		least = "at least "
	}
	switch {
	case r.Compactions == 0 && r.Partial:
		s += ", no compaction in the last 4 MiB"
	case r.Compactions == 0:
		s += ", no compaction"
	case r.Compactions == 1:
		s += ", " + least + "1 compaction"
	default:
		s += fmt.Sprintf(", %s%d compactions", least, r.Compactions)
	}
	return s
}

// kTokens writes a token count in thousands: 612k, or 0.
func kTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%dk", n/1000)
}

// doctorContext reads session's binding record and transcript for doctor.
func doctorContext(session string) doctor.ContextState {
	st := doctor.ContextState{Session: session}
	dir, err := contextDir()
	if err == nil {
		st.BindingPath, err = ctxwatch.BindingPath(dir, session)
	}
	var b ctxwatch.Binding
	if err == nil {
		b, st.Bound, err = ctxwatch.LoadBinding(dir, session)
	}
	if err == nil && st.Bound {
		st.Transcript, st.Levels = b.Transcript, b.Levels
		st.Reading, err = ctxwatch.ReadBound(b)
	}
	st.Err = err
	return st
}

// queueServer and queueDB are the route an MCP server's queue record names
// for its hooks: the hosted server's URL, or the local database.
func queueServer(c Config) string {
	if c.Mode == "cloud" {
		return c.Server
	}
	return ""
}

func queueDB(c Config) string {
	if c.Mode == "cloud" {
		return ""
	}
	return c.DB
}

// doctorQueue reads the session's queue record and compaction hold.
func doctorQueue(session string) doctor.QueueState {
	st := doctor.QueueState{Session: session}
	dir, err := contextDir()
	if err != nil {
		st.Err = err
		return st
	}
	if path, e := ctxwatch.BindingPath(dir, session); e == nil {
		st.Path = strings.TrimSuffix(path, ".json") + ".queue.json"
	}
	r, ok, err := ctxwatch.LoadQueueRecord(dir, session)
	if ok {
		st.Record = &r
	}
	if h, held := ctxwatch.LoadCompactionHold(dir, session); held {
		st.Hold = &h
	}
	st.Err = err
	return st
}

// doctorChannel reads, without writing anything, the session's channel
// record, whether this project's or the user's settings hold the turn hook,
// the plugin's state and the machine's allowlist (FR26).
func doctorChannel(session string) doctor.ChannelState {
	st := doctor.ChannelState{Session: session, GOOS: channelGOOS}
	var paths []string
	config, configErr := ctxwatch.ClaudeConfigDir()
	if configErr == nil {
		paths = append(paths, filepath.Join(config, "settings.json"))
	}
	project, _, rootErr := projectRoot()
	if rootErr == nil {
		paths = append(paths, filepath.Join(project, ".claude", "settings.local.json"), filepath.Join(project, ".claude", "settings.json"))
	} else {
		project = ""
	}
	for _, path := range paths {
		if asked, err := ctxwatch.TurnHookInstalled(path); err == nil && asked {
			st.Asked = true
		}
	}
	if configErr == nil {
		plugin := channel.DetectPlugin(config, project)
		st.Marketplace, st.Enabled = plugin.Marketplace, plugin.Enabled
	}
	st.Allowlisted, _, st.AllowErr = channel.AllowlistedIn(channelManagedDir())
	dir, err := contextDir()
	if err != nil {
		st.Err = err
		return st
	}
	if path, e := ctxwatch.BindingPath(dir, session); e == nil {
		st.Path = strings.TrimSuffix(path, ".json") + ".channel.json"
	}
	r, ok, err := ctxwatch.LoadChannelState(dir, session)
	if ok {
		st.Record = &r
	}
	st.Err = err
	return st
}

// hooksClaudeVersion runs `claude --version`; a variable so tests can replace it.
var hooksClaudeVersion = func(ctx context.Context) (string, bool) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return "", false
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(bounded, path, "--version")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	// "2.1.0 (Claude Code)": the first word.
	if words := strings.Fields(string(out)); len(words) > 0 && len(words[0]) <= 40 {
		return words[0], true
	}
	return "", false
}

// doctorHooks reads the form of the hooks installed in this project's local
// and shared settings and the user's, and the Claude Code version. It never
// fails or waits long: an unreadable file is reported, a missing or slow
// claude is only an unknown version.
func doctorHooks(ctx context.Context) doctor.HooksState {
	st := doctor.HooksState{GOOS: contextGOOS}
	var paths []string
	if config, err := ctxwatch.ClaudeConfigDir(); err == nil {
		paths = append(paths, filepath.Join(config, "settings.json"))
	}
	if root, _, err := projectRoot(); err == nil {
		paths = append(paths, filepath.Join(root, ".claude", "settings.local.json"), filepath.Join(root, ".claude", "settings.json"))
	}
	forms := map[string]bool{}
	for _, path := range paths {
		form, err := ctxwatch.InstalledHookForm(path)
		if err != nil {
			st.Err = err
			return st
		}
		if form == "" {
			continue
		}
		st.Paths = append(st.Paths, path)
		if form == "mixed" {
			forms[ctxwatch.FormExec], forms[ctxwatch.FormShell] = true, true
		} else {
			forms[form] = true
		}
	}
	switch {
	case forms[ctxwatch.FormExec] && forms[ctxwatch.FormShell]:
		st.Form = "mixed"
	case forms[ctxwatch.FormExec]:
		st.Form = ctxwatch.FormExec
	case forms[ctxwatch.FormShell]:
		st.Form = ctxwatch.FormShell
	}
	if st.Form != "" {
		st.Version, _ = hooksClaudeVersion(ctx)
	}
	return st
}
