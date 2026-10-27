package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"

	"clawbridge/internal/bus"
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
	return install.RunWithIO(append(inherited, args...), stdout, stderr)
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

func runTUI(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	screen := "overview"
	_, err := commandFlags("tui", args, stderr, func(f *flag.FlagSet) {
		f.StringVar(&screen, "screen", "overview", "overview, projects, sessions, inbox, cloud, doctor, linear or account")
	})
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	switch tui.Screen(screen) {
	case tui.Overview, tui.Projects, tui.Sessions, tui.Inbox, tui.Cloud, tui.Doctor, tui.Linear, tui.Account:
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
	if c.Mode == "cloud" {
		executor, err = remote.New(remoteConfig(c))
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
	c.Workspace, c.Project, err = executor.ResolveProject(bounded, c.Workspace, c.Project, gitRemote(bounded))
	if err != nil {
		return err
	}
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
	model, err := tui.New(tui.Config{Linear: linearSource, Executor: executor, Mode: tui.Mode(c.Mode), Workspace: c.Workspace, Project: c.Project, Session: c.Session, Instance: c.Instance, Server: c.Server, Audience: c.Audience, Principal: c.Principal, DeviceID: c.DeviceID, Context: bounded, InitialScreen: tui.Screen(screen), CloudInfo: info,
		Doctor: tui.DoctorFunc(func(ctx context.Context) ([]tui.Check, error) {
			report, e := collectDoctor(ctx, c, path, executor)
			if e != nil {
				return nil, e
			}
			out := make([]tui.Check, 0, len(report.Checks))
			for _, check := range report.Checks {
				out = append(out, tui.Check{ID: check.ID, Title: check.Title, Status: tui.Status(check.Status), Detail: check.Message, Fix: check.Action})
			}
			return out, nil
		}),
	})
	if err != nil {
		return err
	}
	return runTUIProgram(bounded, model, os.Stdin, stdout)
}
