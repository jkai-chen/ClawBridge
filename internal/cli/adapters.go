package cli

import (
	"clawbridge/internal/ctxwatch"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
	"clawbridge/internal/remote"
	"clawbridge/internal/tui"
)

func runInstall(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return install.RunWithIO([]string{"--help"}, stdout, stderr)
		}
	}
	if flags.Session == "" {
		return errors.New("mcp install requires an explicit distinct --session; operator session is never inherited")
	}
	c, path, err := resolveConfig(ctx, flags)
	if err != nil {
		return err
	}
	c.Session = flags.Session
	c.Instance = flags.Instance
	args, hooks, err := installHookFlags(args)
	if err != nil {
		return err
	}
	inherited := []string{}
	for _, item := range []struct{ name, value string }{
		{"linear-profile", flags.LinearProfile}, {"db", c.DB}, {"workspace", c.Workspace}, {"project", c.Project}, {"session", c.Session}, {"instance", c.Instance}, {"mode", c.Mode},
		{"server", c.Server}, {"audience", c.Audience}, {"account", c.Account}, {"impersonate-service-account", c.ImpersonateServiceAccount},
		{"principal", c.Principal}, {"cache", c.Cache}, {"credential-source", c.CredentialSource}, {"credentials-file", c.CredentialsFile},
		{"oauth-client-id", c.OAuthClientID}, {"oauth-device-client-id", c.OAuthDeviceClientID}, {"oauth-keyring-service", c.OAuthKeyringService},
		{"device-id", c.DeviceID}, {"key-store", c.KeyStore},
	} {
		if item.value != "" {
			inherited = append(inherited, "--"+item.name, item.value)
		}
	}
	if c.Mode == "cloud" {
		inherited = append(inherited, "--client-config", path)
	}
	if err = install.RunWithIO(append(inherited, args...), stdout, stderr); err != nil {
		return err
	}
	return installHooks(hooks, stderr)
}

// mcpInstallHooks is what mcp install does about Claude Code's hooks: the
// scope (local, user, project-shared or none) and whether it was chosen.
type mcpInstallHooks struct {
	scope, root           string
	explicit              bool
	dryRun, confirmShared bool
}

// installHookFlags takes --hooks and --confirm-shared from mcp install's
// flags; the rest go to the installer. A Claude Code entry gets the hooks
// (the queue's Stop hook, and ctxwatch's) in its project's local settings
// by default, or the user's when the entry is the user's ~/.claude.json.
func installHookFlags(args []string) ([]string, mcpInstallHooks, error) {
	h := mcpInstallHooks{scope: "none"}
	rest := []string{}
	agent, config := "", ".mcp.json"
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		takeValue := func() (string, error) {
			if inline {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("--%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		switch {
		case strings.HasPrefix(args[i], "-") && name == "hooks":
			v, err := takeValue()
			if err != nil {
				return nil, h, err
			}
			h.scope, h.explicit = v, true
			continue
		case strings.HasPrefix(args[i], "-") && name == "confirm-shared":
			h.confirmShared = !inline || value == "true"
			continue
		case strings.HasPrefix(args[i], "-") && (name == "agent" || name == "config"):
			rest = append(rest, args[i])
			v := value
			if !inline && i+1 < len(args) {
				i++
				v = args[i]
				rest = append(rest, v)
			}
			if name == "agent" {
				agent = v
			} else {
				config = v
			}
			continue
		case strings.HasPrefix(args[i], "-") && name == "dry-run":
			h.dryRun = !inline || value == "true"
		}
		rest = append(rest, args[i])
	}
	switch h.scope {
	case "none", ctxwatch.ScopeLocal, ctxwatch.ScopeUser, ctxwatch.ScopeProjectShared:
	default:
		return nil, h, fmt.Errorf("--hooks must be local, user, project-shared or none, not %q", h.scope)
	}
	if agent != ctxwatch.ClaudeCode {
		if h.explicit && h.scope != "none" {
			return nil, h, errors.New("--hooks is for --agent claude-code")
		}
		h.scope = "none"
		return rest, h, nil
	}
	path, err := filepath.Abs(config)
	if err != nil {
		return nil, h, err
	}
	h.root = filepath.Dir(path)
	if !h.explicit {
		h.scope = ctxwatch.ScopeLocal
		if filepath.Base(path) == ".claude.json" {
			h.scope = ctxwatch.ScopeUser
		}
	}
	return rest, h, nil
}

// installHooks installs the hooks after the MCP entry, reporting on
// standard error so the installer's JSON stays alone on standard output.
// A default install that can't add them says so and leaves the entry.
func installHooks(h mcpInstallHooks, stderr io.Writer) error {
	if h.scope == "none" {
		return nil
	}
	if !h.explicit && contextGOOS == "windows" {
		fmt.Fprintln(stderr, "the Claude Code hooks need a POSIX shell and were not installed on Windows")
		return nil
	}
	err := applyHooks(h.scope, h.root, true, h.dryRun, h.confirmShared, stderr, stderr)
	if err != nil && !h.explicit {
		fmt.Fprintf(stderr, "the Claude Code hooks were not installed: %v; install them with clawbridge context hooks install\n", err)
		return nil
	}
	return err
}

func mcpSelection(c Config, path, agent, target, name, session, instance string) doctor.MCPConfig {
	return doctor.MCPConfig{LinearProfile: c.LinearProfile, ClientConfig: func() string {
		if c.Mode == "cloud" {
			return path
		}
		return ""
	}(), Agent: agent, Name: name, Path: target, DB: c.DB, Workspace: c.Workspace, Project: c.Project, Session: session, Instance: instance, Mode: c.Mode, Server: c.Server, Audience: c.Audience, Account: c.Account, ImpersonateServiceAccount: c.ImpersonateServiceAccount, Principal: c.Principal, Cache: c.Cache, CredentialSource: c.CredentialSource, CredentialsFile: c.CredentialsFile, OAuthClientID: c.OAuthClientID, OAuthDeviceClientID: c.OAuthDeviceClientID, OAuthKeyringService: c.OAuthKeyringService, DeviceID: c.DeviceID, KeyStore: c.KeyStore}
}

func runDoctorCommand(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	var agent, target, session, name, instance string
	f, err := commandFlags("doctor", args, stderr, func(f *flag.FlagSet) {
		f.StringVar(&agent, "mcp-agent", "", "codex or claude-code")
		f.StringVar(&target, "mcp-config", "", "agent config path")
		f.StringVar(&session, "mcp-session", "", "independent installed session")
		f.StringVar(&name, "mcp-name", "clawbridge", "entry name")
		f.StringVar(&instance, "mcp-instance", "", "installed session instance")
	})
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	selected := agent != "" || target != "" || session != "" || instance != "" || name != "clawbridge"
	f.Visit(func(*flag.Flag) { selected = true })
	if selected && (agent == "" || target == "" || session == "") {
		return errors.New("doctor requires --mcp-agent, --mcp-config and --mcp-session together")
	}
	if selected && agent != "codex" && agent != "claude-code" {
		return errors.New("unsupported MCP agent")
	}
	if selected {
		target, err = filepath.Abs(target)
		if err != nil {
			return err
		}
	}
	c, path, err := resolveConfig(ctx, flags)
	if err != nil {
		return err
	}
	if selected {
		return runDoctor(ctx, c, path, stdout, mcpSelection(c, path, agent, target, name, session, instance))
	}
	return runDoctor(ctx, c, path, stdout)
}

var checkTUITerminal = tui.CheckTerminal
var runTUIProgram = tui.RunProgram

// waitForProject reports whether the TUI opens on "Waiting for a project":
// a hosted profile with a workspace and no project, given or saved, whose
// project didn't resolve, AND whose signed project list for that workspace
// answers: with none (wait for one) or some (pick one). Any other failure, a
// revoked device, a server down, TLS or DNS, a membership removed, is
// returned as the resolve error it was (R175-M1).
func waitForProject(resolveErr error, mode, workspace, flagProject, project string, listProjects func() error) bool {
	if resolveErr == nil || mode != "cloud" || workspace == "" || strings.TrimSpace(flagProject) != "" || project != "" {
		return false
	}
	return listProjects() == nil
}

// signedProjectList asks the server for this person's projects in the
// workspace; only its own answer counts, never a cached one.
func signedProjectList(ctx context.Context, executor doctor.Executor, workspace string) error {
	r, err := executor.Execute(ctx, bus.Request{Operation: "list_projects", Workspace: workspace, Args: map[string]any{}})
	if err == nil && r.Source == "offline-mirror" {
		// A cached list can't say the server has no project for us.
		return errors.New("the server is offline; a cached project list doesn't count")
	}
	return err
}

// projectSlug is the shape of a project slug (bus namePattern); a slug the
// server hands the TUI is checked before it's written to the profile.
var projectSlug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// saveFoundProject writes the project the waiting screen found to the
// profile, if it is a slug.
func saveFoundProject(path, slug string) error {
	if !projectSlug.MatchString(slug) {
		return errors.New("the server named a project that isn't a slug; set project in " + path + " by hand")
	}
	return setProfileValue(path, "project", slug)
}

func runTUI(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	screen := "overview"
	_, err := commandFlags("tui", args, stderr, func(f *flag.FlagSet) {
		f.StringVar(&screen, "screen", "overview", "overview, projects, sessions, inbox, linear, dispatcher, account, doctor or cloud")
	})
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if screen == "dispatcher" {
		// The tab was called Model before.
		screen = string(tui.DispatcherModel)
	}
	switch tui.Screen(screen) {
	case tui.Overview, tui.Projects, tui.Sessions, tui.Inbox, tui.Cloud, tui.Doctor, tui.Linear, tui.Account, tui.DispatcherModel:
	default:
		return errors.New("invalid TUI screen")
	}
	if err = checkTUITerminal(os.Stdin, stdout); err != nil {
		return err
	}
	c, path, err := resolveConfig(ctx, flags)
	if err != nil {
		return err
	}
	bounded, cancel := context.WithCancel(ctx)
	defer cancel()
	var executor doctor.Executor
	var outboxWaiting func() int
	if c.Mode == "cloud" {
		var client *remote.Client
		client, err = remote.New(remoteConfig(c))
		if err == nil {
			executor = client
			drainBeforeCommand(bounded, client)
			outboxWaiting = func() int { return outboxCount(bounded, client) }
		}
	} else {
		var store *bus.Store
		store, err = bus.Open(c.DB)
		if err == nil {
			defer store.Close()
			executor = store
		}
	}
	if err != nil {
		return err
	}
	workspace := c.Workspace
	c.Workspace, c.Project, err = executor.ResolveProject(bounded, c.Workspace, c.Project, gitRemote(bounded))
	// A hosted profile that joined a workspace but has no project yet opens
	// on "Waiting for a project" (TUI-UX-AUDIT PR D), which saves the
	// project once one is shared and reopens the TUI on it.
	waiting := waitForProject(err, c.Mode, workspace, flags.Project, c.Project, func() error { return signedProjectList(bounded, executor, workspace) })
	if waiting {
		c.Workspace, err = workspace, nil
	}
	if err != nil {
		return err
	}
	found := ""
	info := "Provisioning unavailable. This interface never creates infrastructure."
	if c.Mode == "cloud" {
		info = "Endpoint: " + c.Server + "\n" + info
	} else {
		info = "Local authority.\n" + info
	}
	linearSource, e := newLinearTUISource(c, executor)
	if e != nil {
		return e
	}
	model, err := tui.New(tui.Config{WaitingForProject: waiting, ProjectFound: func(slug string) error {
		if e := saveFoundProject(path, slug); e != nil {
			return e
		}
		found = slug
		return nil
	}, Linear: linearSource, Executor: executor, OutboxWaiting: outboxWaiting, Mode: tui.Mode(c.Mode), Workspace: c.Workspace, Project: c.Project, Session: c.Session, Instance: c.Instance, AgentMarker: agentMarker(closesEnv()), Server: c.Server, Audience: c.Audience, Principal: c.Principal, DeviceID: c.DeviceID, Context: bounded, InitialScreen: tui.Screen(screen), CloudInfo: info,
		Doctor: tui.DoctorFunc(func(ctx context.Context) ([]tui.Check, error) {
			report, e := collectDoctor(ctx, c, path, executor)
			if e != nil {
				return nil, e
			}
			return tuiChecks(report.Checks), nil
		}),
	})
	if err != nil {
		return err
	}
	if err = runTUIProgram(bounded, model, os.Stdin, stdout); err != nil || found == "" {
		return err
	}
	// The project was just saved: open the TUI on it, as if started there.
	// This runs once per save: the saved project resolves, so the next run
	// can't wait again unless the project is taken away in between, and
	// then only after the person has seen it and quit.
	fmt.Fprintf(stderr, "Project %s saved to %s.\n", clirender.Safe(found), path)
	return runTUI(ctx, flags, args, stdout, stderr)
}

// tuiChecks are doctor's checks for the TUI's Doctor tab, each with the
// CLI's answer for its status (U60).
func tuiChecks(checks []doctor.Check) []tui.Check {
	out := make([]tui.Check, 0, len(checks))
	for _, check := range checks {
		out = append(out, tui.Check{ID: check.ID, Title: check.Title, Status: tui.Status(check.Status), Answer: doctorAnswerOf(check), Detail: check.Message, Fix: check.Action})
	}
	return out
}
