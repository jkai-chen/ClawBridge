// Package cli translates command arguments into the shared scoped bus API.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/install"
	"clawbridge/internal/mcpserver"
	"clawbridge/internal/remote"
	"clawbridge/internal/server"
)

const Usage = `clawbridge: local and authenticated hosted agent messaging
Global flags: --db PATH --workspace NAME_OR_ID --project SLUG_OR_ID --session NAME_OR_ID --instance ID --json
Commands: setup --local, project create SLUG, register [NAME], sessions, send TO BODY --operation-id ID,
          inbox, mark-read ID..., reply ID BODY --operation-id ID, update, retire, history, whoami, status ID,
          mcp, mcp install ..., serve, sync, purge-expired
Cloud flags: --mode cloud --server URL --audience AUDIENCE [--account EMAIL] [--impersonate-service-account EMAIL] [--principal EMAIL] [--cache PATH]
All normal results are JSON with source. See CORE-API.md for command flags and isolated test environment variables.
`

// extractGlobals permits global flags before or after commands without swallowing
// flag-looking message bodies: use -- to mark the remainder as positional text.
func extractGlobals(args []string) (Config, []string, error) {
	var c Config
	rest := []string{}
	targets := map[string]*string{"db": &c.DB, "workspace": &c.Workspace, "project": &c.Project, "session": &c.Session, "instance": &c.Instance, "server": &c.Server, "audience": &c.Audience, "account": &c.Account, "impersonate-service-account": &c.ImpersonateServiceAccount, "principal": &c.Principal, "cache": &c.Cache, "mode": &c.Mode}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name, val, has := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !strings.HasPrefix(arg, "--") {
			rest = append(rest, arg)
			continue
		}
		if name == "json" {
			if has {
				if _, err := strconv.ParseBool(val); err != nil {
					return c, nil, errors.New("--json expects a boolean")
				}
			}
			continue
		}
		dest, ok := targets[name]
		if !ok {
			rest = append(rest, arg)
			continue
		}
		if !has {
			i++
			if i >= len(args) {
				return c, nil, fmt.Errorf("--%s requires a value", name)
			}
			val = args[i]
		}
		if val == "" {
			return c, nil, fmt.Errorf("--%s requires a nonempty value", name)
		}
		*dest = val
	}
	return c, rest, nil
}

// interspersedFlags moves declared flags before positional arguments so standard
// flag parsing handles `send recipient body --operation-id retry-key` correctly.
func interspersedFlags(f *flag.FlagSet, args []string) ([]string, error) {
	opts, pos := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			pos = append(pos, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		name, _, has := strings.Cut(name, "=")
		fl := f.Lookup(name)
		if fl == nil {
			return nil, fmt.Errorf("unknown flag --%s", name)
		}
		opts = append(opts, arg)
		if !has {
			boolFlag, ok := fl.Value.(interface{ IsBoolFlag() bool })
			if !ok || !boolFlag.IsBoolFlag() {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("--%s requires a value", name)
				}
				opts = append(opts, args[i])
			}
		}
	}
	opts = append(opts, "--")
	return append(opts, pos...), nil
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "help" || args[0] == "-h")) {
		_, err := io.WriteString(stdout, Usage)
		return err
	}
	// A server invocation has its own operator flags and never opens local state.
	if args[0] == "serve" {
		return server.Run(ctx, args[1:])
	}
	flags, rest, err := extractGlobals(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("command is required")
	}
	// Dispatch only from the command position; recipient names and message bodies
	// may themselves be literal command words such as "mcp install".
	if len(rest) >= 2 && rest[0] == "mcp" && rest[1] == "install" {
		installerArgs := append([]string{}, rest[2:]...)
		for _, entry := range []struct{ name, value string }{{"db", flags.DB}, {"workspace", flags.Workspace}, {"project", flags.Project}, {"session", flags.Session}, {"instance", flags.Instance}, {"mode", flags.Mode}, {"server", flags.Server}, {"audience", flags.Audience}, {"account", flags.Account}, {"impersonate-service-account", flags.ImpersonateServiceAccount}, {"principal", flags.Principal}, {"cache", flags.Cache}} {
			if entry.value != "" {
				installerArgs = append(installerArgs, "--"+entry.name, entry.value)
			}
		}
		return install.Run(installerArgs)
	}
	command := rest[0]
	rest = rest[1:]
	if command == "serve" {
		return errors.New("serve operator flags must follow serve directly")
	}
	config, configPath, err := resolveConfig(ctx, flags)
	if err != nil {
		return err
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(stderr)
	a := map[string]any{}
	op := ""
	arityMin, arityMax := 0, 0
	stringArg := func(flagName, key string) { f.String(flagName, "", key) }
	commonPage := func() { f.Int("limit", 100, "page size (1–1000)"); f.Int64("after", 0, "exclusive sequence cursor") }
	switch command {
	case "setup":
		op = "workspace_init"
		f.Bool("local", false, "explicit local authority")
		f.Bool("cloud", false, "initialize hosted workspace as verified bootstrap owner")
		f.String("name", "", "workspace name")
		f.Int("retention-days", 90, "message retention days (proposed default 90)")
	case "project":
		if len(rest) == 0 {
			return errors.New("usage: project create SLUG, or project role-set EMAIL ROLE")
		}
		switch rest[0] {
		case "create":
			rest = rest[1:]
			op = "project_create"
			arityMin, arityMax = 1, 1
			stringArg("name", "name")
			stringArg("git-remote", "git_remote")
		case "role-set":
			rest = rest[1:]
			op = "project_role_set"
			arityMin, arityMax = 2, 2
		case "role":
			if len(rest) < 2 || rest[1] != "set" {
				return errors.New("usage: project role set EMAIL ROLE")
			}
			rest = rest[2:]
			op = "project_role_set"
			arityMin, arityMax = 2, 2
		default:
			return errors.New("unknown project command")
		}
	case "register":
		f.Bool("resume", false, "rotate a known instance and fence its prior process")
		op = "register_session"
		arityMax = 1
		for _, k := range []string{"role", "agent-kind", "model", "host", "account", "session-account", "notes", "transport"} {
			stringArg(k, strings.ReplaceAll(k, "-", "_"))
		}
	case "sessions":
		op = "list_sessions"
		commonPage()
		stringArg("role", "role")
		stringArg("status", "status")
	case "send":
		op = "send"
		arityMin, arityMax = 2, 2
		stringArg("operation-id", "operation_id")
		stringArg("kind", "kind")
		stringArg("thread-id", "thread_id")
	case "inbox":
		op = "inbox"
		commonPage()
		f.Bool("unread-only", true, "return only unread messages")
	case "mark-read":
		op = "mark_read"
		arityMin, arityMax = 1, 1000
	case "reply":
		op = "reply"
		arityMin, arityMax = 2, 2
		stringArg("operation-id", "operation_id")
		stringArg("kind", "kind")
	case "update":
		stringArg("target-session", "target_session")
		op = "update_session"
		for _, k := range []string{"role", "model", "host", "account", "session-account", "notes", "transport"} {
			stringArg(k, k)
		}
	case "retire":
		stringArg("target-session", "target_session")
		op = "retire_session"
		stringArg("successor", "successor")
		stringArg("status", "status")
	case "history":
		op = "history"
		commonPage()
		stringArg("entity", "entity")
		stringArg("key", "key")
	case "sync":
		if config.Mode != "cloud" {
			return errors.New("sync requires explicit cloud mode")
		}
	case "purge-expired":
		op = "purge_expired"
	case "whoami":
		op = "whoami"
	case "status":
		op = "message_status"
		arityMin, arityMax = 1, 1
	case "mcp":
		if len(rest) > 0 {
			return errors.New("usage: clawbridge mcp [global flags], or clawbridge mcp install ...")
		}
	default:
		return fmt.Errorf("unknown command %q; use --help", command)
	}
	ordered, err := interspersedFlags(f, rest)
	if err != nil {
		return err
	}
	if err = f.Parse(ordered); err != nil {
		return err
	}
	pos := f.Args()
	if len(pos) < arityMin || len(pos) > arityMax {
		return fmt.Errorf("%s expects %d–%d positional arguments", command, arityMin, arityMax)
	}
	f.Visit(func(fl *flag.Flag) {
		key := strings.ReplaceAll(fl.Name, "-", "_")
		switch fl.Name {
		case "local", "cloud", "name", "retention-days":
			return
		}
		if getter, ok := fl.Value.(flag.Getter); ok {
			a[key] = getter.Get()
		}
	})
	if v, ok := a["session_account"]; ok {
		a["account"] = v
		delete(a, "session_account")
	}
	if config.Mode == "local" && (command == "register" || command == "update") && flags.Account != "" {
		a["account"] = flags.Account
	}
	if command == "setup" {
		local := f.Lookup("local").Value.(flag.Getter).Get().(bool)
		cloud := f.Lookup("cloud").Value.(flag.Getter).Get().(bool)
		if local == cloud {
			return errors.New("setup requires exactly one of --local or --cloud")
		}
		if (local && config.Mode != "local") || (cloud && config.Mode != "cloud") {
			return errors.New("setup mode conflicts with configured storage mode; no fallback")
		}
		name := f.Lookup("name").Value.String()
		if name != "" {
			config.Workspace = name
		}
		if config.Workspace == "" {
			if cloud {
				return errors.New("cloud setup requires explicit workspace name")
			}
			config.Workspace = "local"
		}
		a["mode"] = "local"
		if cloud {
			a["mode"] = "server"
		}
		a["retention_days"] = f.Lookup("retention-days").Value.(flag.Getter).Get()
	}
	switch command {
	case "project":
		if op == "project_create" {
			a["slug"] = pos[0]
			if v := f.Lookup("name").Value.String(); v != "" {
				a["name"] = v
			}
		} else {
			a["principal"], a["role"] = pos[0], pos[1]
		}
	case "register":
		if len(pos) > 0 {
			if config.Session != "" && config.Session != pos[0] {
				return errors.New("register name conflicts with configured session; select --session explicitly")
			}
			config.Session = pos[0]
		}
	case "send":
		a["to"], a["body"] = pos[0], pos[1]
	case "reply":
		a["message_id"], a["body"] = pos[0], pos[1]
	case "mark-read":
		a["ids"] = pos
	case "status":
		a["message_id"] = pos[0]
	}
	if raw, ok := a["transport"]; ok {
		var transport map[string]any
		if e := json.Unmarshal([]byte(raw.(string)), &transport); e != nil || transport == nil {
			return errors.New("--transport must be a JSON object")
		}
		a["transport"] = transport
	}
	var executor interface {
		Execute(context.Context, bus.Request) (bus.Result, error)
		ResolveProject(context.Context, string, string, string) (string, string, error)
	}
	var localStore *bus.Store
	var cloudClient *remote.Client
	if config.Mode == "cloud" {
		cloudClient, err = remote.New(remote.Config{URL: config.Server, Audience: config.Audience, Account: config.Account, ImpersonateServiceAccount: config.ImpersonateServiceAccount, Principal: config.Principal, CachePath: config.Cache})
		if err != nil {
			return err
		}
		executor = cloudClient
	} else {
		localStore, err = bus.Open(config.DB)
		if err != nil {
			return err
		}
		defer localStore.Close()
		executor = localStore
	}
	if command != "setup" && op != "project_create" {
		workspace, project, e := executor.ResolveProject(ctx, config.Workspace, config.Project, gitRemote(ctx))
		if e != nil {
			return e
		}
		config.Workspace, config.Project = workspace, project
	}
	if command == "mcp" {
		if config.Session == "" {
			return errors.New("mcp requires an explicit --session or configured session")
		}
		if cloudClient != nil {
			return mcpserver.RunExecutor(ctx, cloudClient, config.Workspace, config.Project, config.Session, config.Instance)
		}
		if err = localStore.Close(); err != nil {
			return err
		}
		return mcpserver.Run(ctx, config.DB, config.Workspace, config.Project, config.Session, config.Instance)
	}
	if command == "sync" {
		result, e := cloudClient.Sync(ctx, config.Workspace, config.Project, config.Session, config.Instance)
		if e != nil {
			return e
		}
		return json.NewEncoder(stdout).Encode(result)
	}
	if config.Instance != "" {
		switch op {
		case "register_session", "list_sessions", "history", "send", "reply", "inbox", "mark_read", "update_session", "retire_session", "whoami", "message_status", "project_role_set", "purge_expired":
			a["instance"] = config.Instance
		}
	}
	result, err := executor.Execute(ctx, bus.Request{Operation: op, Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: a})
	if err != nil {
		return err
	}
	if command == "setup" {
		if err = writeSetupConfig(configPath, config); err != nil {
			return fmt.Errorf("workspace initialized but config write failed: %w", err)
		}
	}
	return json.NewEncoder(stdout).Encode(result)
}
