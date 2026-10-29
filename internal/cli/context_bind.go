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
	"runtime"
	"strings"

	"clawbridge/internal/ctxwatch"
)

// contextStdin is where a hook's JSON input is read from.
var contextStdin io.Reader = os.Stdin

// contextGOOS is the platform the hooks would run on.
var contextGOOS = runtime.GOOS

// hookExecutable is the path of this clawbridge binary, for the hooks; a
// variable so tests can name a Windows path on any OS.
var hookExecutable = func() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	return exe, err
}

// compactReminder is printed to the agent by the SessionStart hook after a
// compaction (§4.5); the cached handover hint follows it.
const compactReminder = "You were just compacted. Re-read your handover note before acting; confirm any operation in flight."

// sharedWarning is shown before the committed settings file is edited.
const sharedWarning = "This file is committed: the hooks will run for every teammate who pulls it, as whatever `clawbridge` their project folder or PATH resolves, the same trust boundary as any committed hook."

// runContextBind runs `context bind`. With --from-hook it is Claude Code's
// SessionStart hook: it always succeeds, prints nothing but the reminder,
// and binds nothing from input it can't trust (CW-L2).
func runContextBind(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("context bind", flag.ContinueOnError)
	f.SetOutput(stderr)
	f.Usage = func() { fmt.Fprintln(stderr, contextUsage) }
	session := f.String("session", os.Getenv("CLAWBRIDGE_SESSION"), "session name")
	agent := f.String("agent", "", "claude-code or codex, with --transcript")
	transcript := f.String("transcript", "", "a transcript path, with --agent")
	thread := f.String("codex-thread", "", "a Codex thread ID")
	fromHook := f.Bool("from-hook", false, "read Claude Code's hook input on stdin")
	f.String("hook-tag", "", "marks the exec-form hook entry; ignored")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New(contextUsage)
	}
	if *fromHook {
		if *agent != "" || *transcript != "" || *thread != "" {
			return errors.New(contextUsage)
		}
		bindFromHook(ctx, *session, stdout)
		return nil
	}
	b := ctxwatch.Binding{BoundAt: ctxwatch.Now()}
	switch {
	case *thread != "" && *agent == "" && *transcript == "":
		home, err := ctxwatch.CodexHome()
		if err != nil {
			return err
		}
		if b.Transcript, err = ctxwatch.CodexRollout(home, strings.ToLower(*thread)); err != nil {
			return err
		}
		b.Agent = ctxwatch.Codex
	case *thread == "" && *agent != "" && *transcript != "":
		if *agent != ctxwatch.ClaudeCode && *agent != ctxwatch.Codex {
			return fmt.Errorf("unknown agent kind %q: want %s or %s", *agent, ctxwatch.ClaudeCode, ctxwatch.Codex)
		}
		path, err := filepath.Abs(*transcript)
		if err == nil {
			path, err = filepath.EvalSymlinks(path)
		}
		if err != nil {
			return fmt.Errorf("the transcript %s is not readable", *transcript)
		}
		// The reader's own checks: a regular file this user owns.
		if _, err := ctxwatch.Read(*agent, path); err != nil {
			return err
		}
		b.Agent, b.Transcript = *agent, path
	default:
		return errors.New(contextUsage)
	}
	if *session == "" {
		return errors.New("name the session with --session (or CLAWBRIDGE_SESSION)")
	}
	dir, err := contextDir()
	if err != nil {
		return err
	}
	if err := saveBinding(dir, *session, b); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "bound %s to its %s transcript %s\n", *session, b.Agent, b.Transcript)
	return err
}

// saveBinding writes b for session, keeping what the record already caches
// from the server and the compactions reported, and the last compaction
// announced while the transcript is the same.
func saveBinding(dir, session string, b ctxwatch.Binding) error {
	if old, bound, err := ctxwatch.LoadBinding(dir, session); err == nil && bound {
		b.Levels, b.HandoverHint = old.Levels, old.HandoverHint
		// The server's count is the session's, whatever transcript it reads.
		b.ReportedCompactions, b.SeenCompactionAt = old.ReportedCompactions, old.SeenCompactionAt
		if old.Agent == b.Agent && old.Transcript == b.Transcript {
			b.CompactingAt, b.CompactTrigger = old.CompactingAt, old.CompactTrigger
		}
	}
	return ctxwatch.SaveBinding(dir, session, b)
}

// hookBinding is the session and transcript a Claude Code hook names, or
// false when there is nothing to trust.
func hookBinding(session string) (ctxwatch.HookInput, string, string, bool) {
	in, ok := ctxwatch.ReadHookInput(contextStdin)
	if !ok {
		return in, "", "", false
	}
	if _, err := ctxwatch.BindingPath("/", session); err != nil {
		return in, "", "", false
	}
	config, err := ctxwatch.ClaudeConfigDir()
	if err != nil {
		return in, "", "", false
	}
	transcript, err := ctxwatch.ClaudeTranscript(in.TranscriptPath, config)
	if err != nil {
		return in, "", "", false
	}
	dir, err := contextDir()
	if err != nil {
		return in, "", "", false
	}
	return in, dir, transcript, true
}

// hookAgentPID is the Claude Code process the hook runs under, or 0.
func hookAgentPID() int { return ctxwatch.HookAgentPID() }

// mayRebind says whether a hook from Claude Code session id in process pid
// may take over the binding old. The session that holds it may always
// rebind (a resume, a compaction), and so may its own process under a new
// session (/clear, /resume): a process is never nested in itself. Another
// process may only once the holding one is gone: a nested `claude -p` in
// the same project shares CLAWBRIDGE_SESSION, and must not take its
// parent's binding.
func mayRebind(old ctxwatch.Binding, bound bool, id string, pid int) bool {
	switch {
	case !bound || old.Agent != ctxwatch.ClaudeCode || old.ClaudeSessionID == "" || old.ClaudeSessionID == id:
		return true
	case pid != 0 && pid == old.AgentPID:
		return true
	}
	return !ctxwatch.ProcessAlive(old.AgentPID)
}

func bindFromHook(ctx context.Context, session string, stdout io.Writer) {
	in, dir, transcript, ok := hookBinding(session)
	if in.SessionID != "" {
		// The queue's step needs only a usable input and the MCP server's
		// record, not the transcript binding.
		defer bindQueue(ctx, session)
	}
	if !ok {
		return
	}
	old, bound, err := ctxwatch.LoadBinding(dir, session)
	pid := hookAgentPID()
	if err == nil && mayRebind(old, bound, in.SessionID, pid) {
		_ = saveBinding(dir, session, ctxwatch.Binding{Agent: ctxwatch.ClaudeCode, Transcript: transcript, BoundAt: ctxwatch.Now(), ClaudeSessionID: in.SessionID, AgentPID: pid})
	}
	// Any compacted agent gets the reminder; the hint is the binding's own,
	// only for the session that holds it.
	if in.Source == "compact" {
		reminder := compactReminder
		if b, bound, err := ctxwatch.LoadBinding(dir, session); err == nil && bound && b.HandoverHint != "" && b.ClaudeSessionID == in.SessionID {
			reminder += " " + b.HandoverHint
		}
		fmt.Fprintln(stdout, reminder)
	}
}

// runContextCompacting runs `context compacting --from-hook`, Claude Code's
// PreCompact hook: it notes the compaction and its trigger on the session's
// binding, when that is the hook's transcript. It always succeeds, silently.
func runContextCompacting(ctx context.Context, args []string, stderr io.Writer) error {
	f := flag.NewFlagSet("context compacting", flag.ContinueOnError)
	f.SetOutput(stderr)
	f.Usage = func() { fmt.Fprintln(stderr, contextUsage) }
	session := f.String("session", os.Getenv("CLAWBRIDGE_SESSION"), "session name")
	fromHook := f.Bool("from-hook", false, "read Claude Code's hook input on stdin")
	f.String("hook-tag", "", "marks the exec-form hook entry; ignored")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || !*fromHook {
		return errors.New(contextUsage)
	}
	in, dir, transcript, ok := hookBinding(*session)
	if in.SessionID != "" {
		defer compactingQueue(ctx, *session)
	}
	if !ok {
		return nil
	}
	b, bound, err := ctxwatch.LoadBinding(dir, *session)
	if err != nil || !bound || b.Agent != ctxwatch.ClaudeCode || b.Transcript != transcript || (b.ClaudeSessionID != "" && b.ClaudeSessionID != in.SessionID) {
		return nil
	}
	b.CompactingAt, b.CompactTrigger = ctxwatch.Now(), ""
	if in.Trigger == "manual" || in.Trigger == "auto" {
		b.CompactTrigger = in.Trigger
	}
	_ = ctxwatch.SaveBinding(dir, *session, b)
	return nil
}

// runContextHooks runs `context hooks install|uninstall`.
func runContextHooks(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "install" && args[0] != "uninstall") {
		return errors.New(contextUsage)
	}
	install := args[0] == "install"
	f := flag.NewFlagSet("context hooks "+args[0], flag.ContinueOnError)
	f.SetOutput(stderr)
	f.Usage = func() { fmt.Fprintln(stderr, contextUsage) }
	agent := f.String("agent", ctxwatch.ClaudeCode, "the agent whose settings get the hooks")
	scope := f.String("scope", ctxwatch.ScopeLocal, "local, user or project-shared")
	confirmShared := f.Bool("confirm-shared", false, "confirm editing the committed .claude/settings.json")
	dryRun := f.Bool("dry-run", false, "show the change without making it")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New(contextUsage)
	}
	if *agent != ctxwatch.ClaudeCode {
		return fmt.Errorf("hooks are for %s only; bind a Codex session with `context bind --codex-thread`", ctxwatch.ClaudeCode)
	}
	return applyHooks(*scope, "", install, *dryRun, *confirmShared, stdout, stderr)
}

// applyHooks installs or removes ClawBridge's Claude Code hooks at scope.
// The local and shared scopes are the settings of the project at root, or
// with no root of the git work tree holding the current directory.
func applyHooks(scope, root string, install, dryRun, confirmShared bool, stdout, stderr io.Writer) error {
	var path string
	switch scope {
	case ctxwatch.ScopeUser:
		config, err := ctxwatch.ClaudeConfigDir()
		if err != nil {
			return err
		}
		path = filepath.Join(config, "settings.json")
	case ctxwatch.ScopeLocal, ctxwatch.ScopeProjectShared:
		inGit := false
		if root == "" {
			var err error
			if root, inGit, err = projectRoot(); err != nil {
				return err
			}
		} else {
			inGit = exec.Command("git", "-C", root, "rev-parse", "--show-toplevel").Run() == nil
		}
		name := "settings.local.json"
		if scope == ctxwatch.ScopeProjectShared {
			name = "settings.json"
		}
		path = filepath.Join(root, ".claude", name)
		if install && scope == ctxwatch.ScopeLocal && inGit {
			rel := filepath.Join(".claude", name)
			if gitTracks(root, rel) {
				return fmt.Errorf("%s is tracked by git, so it is not private to you; untrack it, or use --scope user", path)
			}
			if !gitIgnores(root, rel) {
				fmt.Fprintf(stderr, "warning: git doesn't ignore %s, so a `git add` could commit it; add it to .gitignore\n", path)
			}
		}
	default:
		return fmt.Errorf("unknown scope %q: want %s, %s or %s", scope, ctxwatch.ScopeLocal, ctxwatch.ScopeUser, ctxwatch.ScopeProjectShared)
	}
	binary := ""
	if install && scope != ctxwatch.ScopeProjectShared {
		exe, err := hookExecutable()
		if err != nil {
			return errors.New("the path of this clawbridge binary is unknown")
		}
		binary = exe
	}
	change, err := ctxwatch.PlanHooksFor(contextGOOS, path, scope, binary, install)
	if err != nil {
		return err
	}
	if !change.Changed() {
		_, err := fmt.Fprintf(stdout, "%s: no change\n", path)
		return err
	}
	fmt.Fprintf(stdout, "%s:\n%s", path, change.Diff())
	if dryRun {
		_, err := fmt.Fprintln(stdout, "dry run: nothing written")
		return err
	}
	if install && scope == ctxwatch.ScopeProjectShared {
		fmt.Fprintln(stdout, sharedWarning)
		if !confirmShared {
			return errors.New("nothing written: run again with --confirm-shared to edit the committed file")
		}
	}
	if err := change.Apply(); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "written")
	return err
}

// projectRoot is the git work tree holding the current directory, or the
// directory itself outside git.
func projectRoot() (string, bool, error) {
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root, true, nil
		}
	}
	dir, err := os.Getwd()
	return dir, false, err
}

// gitIgnores says whether git ignores rel in the work tree at root.
func gitIgnores(root, rel string) bool {
	return exec.Command("git", "-C", root, "check-ignore", "-q", "--", rel).Run() == nil
}

// gitTracks says whether git tracks rel in the work tree at root.
func gitTracks(root, rel string) bool {
	return exec.Command("git", "-C", root, "ls-files", "--error-unmatch", "--", rel).Run() == nil
}
