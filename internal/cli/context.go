package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
      [--confirm-shared] [--dry-run]
      Claude Code hooks that bind the session when it starts and note each
      compaction. local (the default) is the project's
      .claude/settings.local.json, refused when git tracks it; user is your
      own settings.json; project-shared is the committed .claude/settings.json
      and needs --confirm-shared. The change is shown before it is written,
      and uninstall removes only ClawBridge's own entries.
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

// runContext runs `clawbridge context …`. It reads local files only, so it
// needs no configuration or route.
func runContext(_ context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(contextUsage)
	}
	switch args[0] {
	case "show":
	case "bind":
		return runContextBind(args[1:], stdout, stderr)
	case "compacting":
		return runContextCompacting(args[1:], stderr)
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
	if *transcript == "" {
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
				return json.NewEncoder(stdout).Encode(map[string]any{"session": *session, "bound": false})
			}
			_, err := fmt.Fprintf(stdout, "context: %s is not bound to a transcript\n", *session)
			return err
		}
		*agent, *transcript = b.Agent, b.Transcript
	}
	path, err := filepath.Abs(*transcript)
	if err != nil {
		return err
	}
	r, err := ctxwatch.Read(*agent, path)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(struct {
			ctxwatch.Reading
			Transcript string `json:"transcript"`
		}{r, path})
	}
	_, err = fmt.Fprintln(stdout, describeReading(r)+"\ntranscript: "+path)
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
		st.Reading, err = ctxwatch.Read(b.Agent, b.Transcript)
	}
	st.Err = err
	return st
}
