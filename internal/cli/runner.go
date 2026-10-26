package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/install"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/runner"
)

const runnerUsage = `clawbridge runner launch ISSUE_UUID --profile ID [--role ROLE] [--repo PATH] [--base REF] [--model MODEL] [--dry-run]
clawbridge runner fetch SESSION
clawbridge runner gc [--confirm]
launch starts a Claude Code session for one Linear issue in a visible Orca terminal: its own clone of the repository
on an agent/* branch that cannot push, a registered ClawBridge session, and settings that never allow bypass mode and
sandbox Bash. fetch brings a session's branch into your checkout as agent/<id>, refusing one that adds token-shaped text. gc removes the runner's finished clones (merged,
or a week old without commits or changes); without --confirm it only shows them.
Settings: <state>/runner/config.json {"model": "", "allowed_domains": [...], "max_retained": 10, "work_dir": "~/clawbridge-runs"}.`

// runnerState is the runner's own directory.
func runnerState() (string, error) {
	home, err := stateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "runner"), nil
}

func gitIn(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, errors.New(text)
	}
	return text, nil
}

// newRunner is the runner with this machine's git, Orca and claude.
func newRunner(state string) *runner.Runner {
	return &runner.Runner{StateDir: state, Git: gitIn, Terminal: orcaTerminal, Close: orcaClose, ClaudeVersion: claudeVersion}
}

// claudeVersion is the installed claude's version line.
var claudeVersion = func() (string, error) {
	out, err := exec.Command("claude", "--version").Output()
	if err != nil {
		return "", errors.New("claude is not installed or did not report its version")
	}
	return strings.TrimSpace(string(out)), nil
}

// orcaTerminal opens a visible Orca terminal tab in the owner's checkout,
// which Orca manages; the command changes into the clone itself.
var orcaTerminal = func(owner, title, command string) (string, error) {
	out, err := exec.Command("orca", "terminal", "create", "--worktree", "path:"+owner, "--title", title, "--command", command, "--json").CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if len(text) > 300 {
			text = text[:300]
		}
		return "", fmt.Errorf("%v: %s", err, text)
	}
	var created struct {
		Result struct {
			Terminal struct {
				Handle string `json:"handle"`
			} `json:"terminal"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &created) != nil || created.Result.Terminal.Handle == "" {
		return "", errors.New("orca did not name the terminal it opened")
	}
	return created.Result.Terminal.Handle, nil
}

// orcaClose closes a terminal the launch opened.
var orcaClose = func(handle string) error {
	return exec.Command("orca", "terminal", "close", "--terminal", handle, "--json").Run()
}

func runRunner(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(stdout, runnerUsage+"\n")
		return err
	}
	state, err := runnerState()
	if err != nil {
		return err
	}
	r := newRunner(state)
	emit := func(v any) error { return json.NewEncoder(stdout).Encode(bus.Result{Source: "runner", Data: v}) }
	switch args[0] {
	case "gc":
		f := flag.NewFlagSet("runner gc", flag.ContinueOnError)
		f.SetOutput(stderr)
		confirm := f.Bool("confirm", false, "remove the finished clones shown")
		if err = f.Parse(args[1:]); err != nil || f.NArg() != 0 {
			return errors.Join(err, errors.New(runnerUsage))
		}
		out, err := r.GC(*confirm)
		if err != nil {
			return err
		}
		return emit(map[string]any{"confirmed": *confirm, "clones": out})
	case "launch":
		return runnerLaunch(ctx, flags, r, args[1:], stderr, emit)
	case "fetch":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return errors.New("runner fetch takes one session name")
		}
		out, err := r.Fetch(args[1])
		if err != nil {
			_ = emit(out)
			return err
		}
		return emit(out)
	}
	return errors.New(runnerUsage)
}

type launchFlags struct {
	profile, role, repo, base, model string
	dryRun                           bool
}

func parseRunnerLaunch(args []string, stderr io.Writer) (launchFlags, string, error) {
	var v launchFlags
	f := flag.NewFlagSet("runner launch", flag.ContinueOnError)
	f.SetOutput(stderr)
	f.StringVar(&v.profile, "profile", "", "the personal Linear profile that proves the issue (read access is enough)")
	f.StringVar(&v.role, "role", "engineer", "the session's role")
	f.StringVar(&v.repo, "repo", "", "the repository checkout to branch from (default: this checkout)")
	f.StringVar(&v.base, "base", "", "the base ref (default: origin/HEAD)")
	f.StringVar(&v.model, "model", "", "the Claude model for this launch (default: the runner setting, else your own default)")
	f.BoolVar(&v.dryRun, "dry-run", false, "show what would be launched; change nothing")
	ordered, err := interspersedFlags(f, args)
	if err != nil {
		return v, "", err
	}
	if err = f.Parse(ordered); err != nil {
		return v, "", err
	}
	if f.NArg() != 1 || !linearprovider.ValidID(f.Arg(0)) {
		return v, "", errors.New("runner launch takes one issue UUID")
	}
	if !validLinearProfile(v.profile) {
		return v, "", errors.New("runner launch requires --profile with the opaque ID from linear profiles")
	}
	if strings.TrimSpace(v.role) == "" || strings.ContainsAny(v.role, " \t\n") || len(v.role) > 40 {
		return v, "", errors.New("--role is one short word, such as engineer")
	}
	return v, f.Arg(0), nil
}

func runnerLaunch(ctx context.Context, flags Config, r *runner.Runner, args []string, stderr io.Writer, emit func(any) error) error {
	v, issueID, err := parseRunnerLaunch(args, stderr)
	if err != nil {
		return err
	}
	settings, err := r.LoadSettings()
	if err != nil {
		return err
	}
	if v.model != "" {
		settings.Model = v.model
	}
	repo := v.repo
	if repo == "" {
		repo = checkoutRoot(ctx)
	}
	if repo == "" {
		return errors.New("run runner launch inside the repository checkout, or pass --repo")
	}
	if repo, err = filepath.Abs(repo); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// The issue must be on the project's board: proven through the person's
	// own Linear profile, read access being enough.
	directory, err := linearDirectory()
	if err != nil {
		return err
	}
	executor, closeExecutor, scope, err := linearExecutor(bounded, flags)
	if err != nil {
		return err
	}
	defer closeExecutor()
	service := newLinearService(directory, v.profile, newLinearManager(directory), executor, scope.Args["linear_authority"].(string))
	issue, err := service.IssueV2(bounded, scope, issueID)
	if err != nil {
		return fmt.Errorf("the issue is not readable on this project's board: %w", err)
	}
	if _, err = runner.Branch(issue.Identifier); err != nil {
		return fmt.Errorf("the issue has no usable identifier: %w", err)
	}
	spec := runner.Spec{Repo: repo, Base: v.base, IssueID: issue.ID, Identifier: issue.Identifier, Settings: settings, GoModCache: goModCache(repo), DryRun: v.dryRun}
	name := filepath.Base(repo)
	if v.dryRun {
		spec.Session, spec.Instance, spec.MCPEntry = runner.SessionName(name, v.role, 1), "dry-run", map[string]any{}
	} else {
		// A fresh name: registering a taken one is refused, never taken over.
		var b [16]byte
		if _, err = rand.Read(b[:]); err != nil {
			return err
		}
		spec.Instance = "run-" + hex.EncodeToString(b[:])
		taken, err := sessionNames(bounded, executor, scope)
		if err != nil {
			return fmt.Errorf("list the project's sessions: %w", err)
		}
		for n := 1; n <= 1000 && spec.Session == ""; n++ {
			if candidate := runner.SessionName(name, v.role, n); !taken[candidate] {
				spec.Session = candidate
			}
		}
		// Registering a name taken meanwhile is refused, never taken over.
		if _, err = executor.Execute(bounded, bus.Request{Operation: "register_session", Workspace: scope.Workspace, Project: scope.Project, Session: spec.Session, Args: map[string]any{"instance": spec.Instance, "role": v.role, "agent_kind": "claude-code", "notes": "started by clawbridge runner for " + issue.Identifier}}); err != nil {
			return fmt.Errorf("register the session %s: %w", spec.Session, err)
		}
		if spec.MCPEntry, err = runnerMCPEntry(ctx, flags, spec.Session, spec.Instance, v.profile); err != nil {
			retireRunnerSession(bounded, executor, scope, spec)
			return err
		}
	}
	prepared, err := r.Launch(spec)
	if err != nil {
		// A launch that did not start, or whose sandbox self-test failed,
		// leaves no live session: it is retired, with its own deadline
		// since the self-test wait outlasts the launch's.
		if !v.dryRun {
			retire, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			retireRunnerSession(retire, executor, scope, spec)
			cancel()
		}
		if prepared.Clone != "" {
			return fmt.Errorf("%w; the clone %s stays for clawbridge runner gc", err, prepared.Clone)
		}
		return err
	}
	return emit(map[string]any{"issue_identifier": issue.Identifier, "issue_id": issue.ID, "launch": prepared})
}

// runnerMCPEntry is the session's MCP server entry, exactly as
// `clawbridge mcp install --agent claude-code` writes it.
func runnerMCPEntry(ctx context.Context, flags Config, session, instance, profile string) (map[string]any, error) {
	flags.Session, flags.Instance, flags.LinearProfile = session, instance, profile
	var out bytes.Buffer
	if err := runInstall(ctx, flags, []string{"--agent", "claude-code", "--dry-run"}, &out, io.Discard); err != nil {
		return nil, fmt.Errorf("build the MCP entry: %w", err)
	}
	var r install.Result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Entry == nil {
		return nil, errors.New("the MCP entry could not be built")
	}
	return r.Entry, nil
}

// retireRunnerSession undoes a registration whose launch did not start.
func retireRunnerSession(ctx context.Context, executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}, scope bus.Request, spec runner.Spec) {
	_, _ = executor.Execute(ctx, bus.Request{Operation: "retire_session", Workspace: scope.Workspace, Project: scope.Project, Session: spec.Session, Args: map[string]any{"instance": spec.Instance}})
}

// sessionNames are every session name in the project, any status: a name
// is unique for good.
func sessionNames(ctx context.Context, executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}, scope bus.Request) (map[string]bool, error) {
	names := map[string]bool{}
	var after any = 0
	for page := 0; page < 20; page++ {
		r, err := executor.Execute(ctx, bus.Request{Operation: "list_sessions", Workspace: scope.Workspace, Project: scope.Project, Args: map[string]any{"limit": 1000, "after": after}})
		if err != nil {
			return nil, err
		}
		data, _ := r.Data.(map[string]any)
		var rows []any
		switch list := data["sessions"].(type) {
		case []map[string]any:
			for _, m := range list {
				rows = append(rows, m)
			}
		case []any:
			rows = list
		}
		for _, row := range rows {
			if m, ok := row.(map[string]any); ok {
				if n, _ := m["name"].(string); n != "" {
					names[n] = true
				}
			}
		}
		if more, _ := data["has_more"].(bool); !more {
			return names, nil
		}
		after = data["next_cursor"]
	}
	return nil, errors.New("too many sessions to list")
}

// goModCache is the owner's Go module cache, when Go is installed and the
// cache exists.
func goModCache(repo string) string {
	cmd := exec.Command("go", "env", "GOMODCACHE")
	cmd.Dir = repo
	out, err := cmd.Output()
	dir := strings.TrimSpace(string(out))
	if err != nil || !filepath.IsAbs(dir) {
		return ""
	}
	if info, err := os.Stat(filepath.Join(dir, "cache", "download")); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}
