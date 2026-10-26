// Package cli translates command arguments into the shared scoped bus API.
package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/auth"
	"clawbridge/internal/buildinfo"
	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/mcpserver"
	"clawbridge/internal/remote"
	"clawbridge/internal/server"
	"clawbridge/internal/skill"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Usage = `clawbridge: local and authenticated hosted agent messaging
Global flags: --db PATH --workspace NAME_OR_ID --project SLUG_OR_ID --session NAME_OR_ID --instance ID --json
Commands: setup --local, project create SLUG, register [NAME], sessions, send TO BODY --operation-id ID,
          inbox, inbox-status [--after N], mark-read ID..., reply ID BODY --operation-id ID, update, retire, history, whoami, status ID,
          notification-status [--after N], notification-policy --policy accept|hold|refuse,
          assignment propose [--issue-uuid UUID --issue-identifier ID] [--task-role ROLE] [--scope-repo REMOTE] [--scope-path PATTERN]...,
          assignment confirm TARGET_SESSION --revision REV [--allow-overlap], assignment release [TARGET_SESSION] --revision REV,
          project coordinator-set SESSION, project coordinator-clear,
          sessions [--issue ID] [--scope-path PATH] [--assignment-state proposed|confirmed],
          host-status|host-wait --adapter codex --endpoint unix:///absolute/socket --thread UUID,
          notify --adapter json|codex --state ABSOLUTE_PATH [--thread UUID] [--interval 5s] [--once],
          linear COMMAND --help (native connection, scoped task board and task actions),
          mcp [--linear-profile ID], mcp install ... [--linear-profile ID] (opt-in native Linear tools),
          skill install|status|uninstall [--agent claude-code|codex|all] [--dir SKILLS_DIR] [--force],
          serve, operator principal-unbind|workspace-clear-domains ... --confirm (on the server host),
          sync, purge-expired, principal-unbind EMAIL --confirm [--allow-previous],
          workspace-domains DOMAIN...|--clear, doctor, tui [--screen SCREEN], version
Interactive: bare clawbridge opens the selected profile or guided join; setup wizard starts guided join.
Onboarding: setup join --server URL --workspace TEAM --project CODEBASE [--login|--device]
setup --cloud initializes an existing service only; no infrastructure is created.
Global: --client-config /absolute/path/config.toml (trusted route; not agent --config)
Commands: login [--device|--use-gcloud], logout
Cloud flags: --mode cloud --server URL --audience AUDIENCE [--credential-source oauth|gcloud|adc] [--oauth-client-id ID --oauth-client-secret CREDENTIAL --oauth-device-client-id ID --oauth-device-client-secret CREDENTIAL] [--principal EMAIL] [--cache PATH]
All normal results are JSON with source. See docs/reference/api.md for command flags and isolated test environment variables.
`

var (
	browserLoginStaged   = auth.BrowserLoginStaged
	deviceLoginStaged    = auth.DeviceLoginStaged
	loginCredentialStore = func(service string) auth.Store {
		return auth.KeyringStore{Service: service}
	}
	writeLoginConfig   = writeCredentialSourceConfig
	discoverLoginOAuth = remote.DiscoverOAuthClient
)

// extractGlobals permits global flags before or after commands without swallowing
// flag-looking message bodies: use -- to mark the remainder as positional text.
func extractGlobals(args []string) (Config, []string, error) {
	var c Config
	rest := []string{}
	targets := map[string]*string{
		"linear-profile": &c.LinearProfile, "client-config": &c.ClientConfig, "db": &c.DB, "workspace": &c.Workspace, "project": &c.Project,
		"session": &c.Session, "instance": &c.Instance, "server": &c.Server,
		"audience": &c.Audience, "account": &c.Account,
		"impersonate-service-account": &c.ImpersonateServiceAccount,
		"principal":                   &c.Principal, "cache": &c.Cache, "mode": &c.Mode,
		"credential-source": &c.CredentialSource, "credentials-file": &c.CredentialsFile,
		"oauth-client-id": &c.OAuthClientID, "oauth-client-secret": &c.OAuthClientSecret,
		"oauth-device-client-id": &c.OAuthDeviceClientID, "oauth-device-client-secret": &c.OAuthDeviceClientSecret,
		"oauth-keyring-service": &c.OAuthKeyringService,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if (arg == "--help" || arg == "-h") && len(rest) > 0 {
			rest = append(rest, arg)
			continue
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
		if name == "linear-profile" && !validLinearProfile(val) {
			return c, nil, errors.New("invalid personal Linear profile ID")
		}
		if name == "client-config" {
			if c.ClientConfig != "" {
				return c, nil, errors.New("--client-config may be supplied only once")
			}
			if !filepath.IsAbs(val) {
				return c, nil, errors.New("--client-config requires an absolute path")
			}
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
		if arg == "--help" || arg == "-h" {
			opts = append(opts, arg)
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
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		if len(args) != 1 {
			return errors.New("version accepts no arguments")
		}
		return json.NewEncoder(stdout).Encode(struct {
			Source string         `json:"source"`
			Data   buildinfo.Info `json:"data"`
		}{Source: "build", Data: buildinfo.Current()})
	}
	// The skill command needs no trusted configuration, so it runs before any
	// route is resolved.
	if len(args) > 0 && args[0] == "skill" {
		return skill.RunWithIO(args[1:], stdout, stderr)
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "help" || args[0] == "-h") {
		_, err := io.WriteString(stdout, Usage)
		return err
	}
	// A server invocation has its own operator flags and never opens local state.
	if len(args) > 0 && args[0] == "serve" {
		return server.Run(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "operator" {
		return server.RunOperator(ctx, args[1:], stdout)
	}
	flags, rest, err := extractGlobals(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return runStartup(ctx, flags, args, stdout, stderr)
	}
	if len(rest) >= 2 && rest[0] == "setup" && rest[1] == "wizard" {
		return runWizard(ctx, flags, rest[2:], stdout, stderr)
	}
	if len(rest) >= 2 && rest[0] == "setup" && rest[1] == "join" {
		return runJoin(ctx, flags, rest[2:], stdout, stderr)
	}
	if rest[0] == "tui" {
		return runTUI(ctx, flags, rest[1:], stdout, stderr)
	}
	if rest[0] == "linear" {
		return runLinear(ctx, flags, rest[1:], stdout, stderr)
	}
	if rest[0] == "host-status" || rest[0] == "host-wait" {
		return runHost(ctx, rest[0], rest[1:], stdout, stderr)
	}
	if rest[0] == "notify" {
		return runNotify(ctx, flags, rest[1:], stdout, stderr)
	}
	if rest[0] == "doctor" {
		return runDoctorCommand(ctx, flags, rest[1:], stdout, stderr)
	}
	if rest[0] == "login" || rest[0] == "logout" {
		command := rest[0]
		commandArgs := rest[1:]
		f := flag.NewFlagSet(command, flag.ContinueOnError)
		f.SetOutput(stderr)
		device := f.Bool("device", false, "sign in with a code on another device")
		useGcloud := f.Bool("use-gcloud", false, "try the complete explicit gcloud route, then browser OAuth")
		ordered, err := interspersedFlags(f, commandArgs)
		if err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if err = f.Parse(ordered); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if f.NArg() != 0 {
			return fmt.Errorf("%s accepts flags only", command)
		}
		var config Config
		var configPath string
		if command == "login" && *useGcloud {
			// A gcloud probe is a separate, explicitly selected route. It may
			// borrow server/audience/scope from trusted config but cannot inherit
			// OAuth client material, and an OAuth fallback cannot inherit the
			// gcloud account/impersonation fields used only for that probe.
			gcloudConfig, _, gcloudErr := resolveConfigVariant(ctx, flags, func(c Config) Config {
				c.CredentialsFile = ""
				c.OAuthClientID = ""
				c.OAuthClientSecret = ""
				c.OAuthDeviceClientID = ""
				c.OAuthDeviceClientSecret = ""
				c.OAuthKeyringService = ""
				return c
			})
			if gcloudErr == nil {
				source := &remote.GcloudTokenSource{
					Account: gcloudConfig.Account, Audience: gcloudConfig.Audience,
					ImpersonateServiceAccount: gcloudConfig.ImpersonateServiceAccount,
				}
				if _, probeErr := source.Token(ctx); probeErr == nil {
					return json.NewEncoder(stdout).Encode(map[string]any{
						"credential_source": "gcloud", "principal": gcloudConfig.ImpersonateServiceAccount,
					})
				}
			}
			config, configPath, err = resolveConfigVariantMode(ctx, flags, func(c Config) Config {
				c.CredentialSource = "oauth"
				c.Account = ""
				c.ImpersonateServiceAccount = ""
				c.CredentialsFile = ""
				return c
			}, true, *device)
			if err != nil {
				return fmt.Errorf("gcloud was unavailable and browser OAuth fallback is not valid: %w", err)
			}
		} else {
			if command == "login" {
				config, configPath, err = resolveLoginConfig(ctx, flags, *device)
			} else {
				config, configPath, err = resolveConfig(ctx, flags)
			}
			if err != nil {
				return err
			}
		}
		if command == "logout" {
			if *device || *useGcloud {
				return errors.New("logout accepts only global configuration flags")
			}
			if config.OAuthClientID == "" {
				return errors.New("no OAuth client is configured; logout cannot identify a keychain entry")
			}
			if err = auth.Logout(ctx, loginCredentialStore(config.OAuthKeyringService), config.OAuthClientID); err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(map[string]any{"logged_out": true, "client_id": config.OAuthClientID})
		}
		if *device && *useGcloud {
			return errors.New("login accepts either --device or --use-gcloud, not both")
		}
		config, transaction, err := acquireOAuth(ctx, config, *device, stderr)
		if err != nil {
			return err
		}
		if err = writeLoginConfig(configPath, config); err != nil {
			rollbackErr := transaction.Rollback(ctx)
			if rollbackErr != nil {
				return fmt.Errorf("login credential staged but trusted config write and keychain rollback both failed; prior configuration remains effective and login must be retried: %w", err)
			}
			return fmt.Errorf("login did not complete because trusted config update failed; prior configuration and keychain state were preserved: %w", err)
		}
		transaction.Commit()
		return json.NewEncoder(stdout).Encode(map[string]any{
			"logged_in": true, "credential_source": "oauth",
			"principal": config.Principal, "oauth_client_id": config.OAuthClientID,
		})
	}
	// Dispatch only from the command position; recipient names and message bodies
	// may themselves be literal command words such as "mcp install".
	if len(rest) >= 2 && rest[0] == "mcp" && rest[1] == "install" {
		return runInstall(ctx, flags, rest[2:], stdout, stderr)
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
	if command == "doctor" {
		return runDoctor(ctx, config, configPath, stdout)
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
			return errors.New("usage: project create SLUG, project role-set EMAIL ROLE, or project role-revoke EMAIL")
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
		case "coordinator-set", "coordinator-clear":
			// Designating the session that confirms assignments grants no other rights.
			op = "coordinator_set"
			if rest[0] == "coordinator-set" {
				arityMin, arityMax = 1, 1
			}
			rest = rest[1:]
		case "role-revoke":
			rest = rest[1:]
			op = "project_role_revoke"
			arityMin, arityMax = 1, 1
		case "role":
			if len(rest) < 2 || (rest[1] != "set" && rest[1] != "revoke") {
				return errors.New("usage: project role set EMAIL ROLE, or project role revoke EMAIL")
			}
			op = "project_role_set"
			arityMin, arityMax = 2, 2
			if rest[1] == "revoke" {
				op = "project_role_revoke"
				arityMin, arityMax = 1, 1
			}
			rest = rest[2:]
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
		stringArg("issue", "Linear issue UUID or display identifier of a live assignment")
		stringArg("scope-path", "repository path covered by a live assignment")
		stringArg("assignment-state", "proposed or confirmed")
	case "intake":
		// The project's issue intake: its policy, trusted Linear users,
		// /mcp/intake tokens and queue (live/INTAKE-DESIGN.md).
		usage := errors.New("usage: intake policy [set --intake-mode off|notify --signals S1 ...] | trust add|remove LINEAR_USER_ID | trust list | token create [--expires-days N] | token list | token revoke TOKEN_ID | list [--state S] | dismiss|rearm ISSUE_UUID")
		if len(rest) == 0 {
			return usage
		}
		sub := rest[0]
		rest = rest[1:]
		action := ""
		if len(rest) > 0 && (sub == "policy" || sub == "trust" || sub == "token") {
			action, rest = rest[0], rest[1:]
		}
		switch sub + " " + action {
		case "policy ":
			op = "intake_policy_get"
		case "policy set":
			op = "intake_policy_set"
			// --mode is the global storage mode, so the policy's is --intake-mode.
			stringArg("intake-mode", "off or notify (offer and auto are not available yet)")
			f.Var(&repeatedIDs{}, "signals", "enabled signal S1–S5, repeatable; replaces the list")
			f.Var(&repeatedIDs{}, "roles", "session role eligible for offers, repeatable; replaces the list")
			f.Int("s5-calls-per-hour", 0, "Linear calls per hour the /mcp/intake proofs may spend")
			f.Int("out-of-binding-limit", 0, "out-of-binding requests per hour that suspend an intake token")
			f.Int("expected-revision", 0, "the policy revision you last read")
		case "trust add", "trust remove":
			op = "intake_trust_" + action
			arityMin, arityMax = 1, 1
		case "trust list":
			op = "intake_trust_list"
		case "token create":
			op = "intake_token_create"
			f.Int("expires-days", 0, "days until the token expires (default 90, at most 365)")
		case "token list":
			op = "intake_token_list"
		case "token revoke":
			op = "intake_token_revoke"
			arityMin, arityMax = 1, 1
		case "list ":
			op = "intake_list"
			stringArg("state", "queued, offered, assigned, parked, dismissed or closed")
		case "dismiss ", "rearm ":
			op = "intake_" + sub
			arityMin, arityMax = 1, 1
		default:
			return usage
		}
	case "assignment":
		if len(rest) == 0 {
			return errors.New("usage: assignment propose|confirm|release ...")
		}
		// The positional argument names a session, never an assignment ID.
		synopsis := map[string]string{
			"propose": "usage: clawbridge assignment propose [flags]\n  Proposes an assignment for your own session.",
			"confirm": "usage: clawbridge assignment confirm TARGET_SESSION --revision REV [--allow-overlap]\n  TARGET_SESSION is the name or ID of the session whose proposed assignment\n  you confirm, not an assignment ID; only the coordinator session confirms.",
			"release": "usage: clawbridge assignment release [TARGET_SESSION] --revision REV [--outcome done]\n  TARGET_SESSION is the name or ID of the session whose assignment you\n  release, not an assignment ID; omit it to release your own. --outcome done\n  says the work is finished: a project status policy may then move the issue on.",
		}
		if text, ok := synopsis[rest[0]]; ok {
			f.Usage = func() {
				fmt.Fprintln(f.Output(), text)
				f.PrintDefaults()
			}
		}
		switch rest[0] {
		case "propose":
			op = "assignment_propose"
			stringArg("issue-uuid", "Linear issue UUID")
			stringArg("issue-identifier", "Linear display identifier, such as ABC-12")
			stringArg("task-role", "task role, such as engineer or reviewer")
			stringArg("scope-repo", "git remote of the owned repository")
			f.Var(&repeatedIDs{}, "scope-path", "owned path pattern, repeatable (internal/bus/**, docs/*.md)")
		case "confirm":
			op = "assignment_confirm"
			arityMin, arityMax = 1, 1
			stringArg("revision", "the assignment revision you reviewed")
			f.Bool("allow-overlap", false, "confirm although a confirmed assignment overlaps")
		case "release":
			op = "assignment_release"
			arityMax = 1
			stringArg("revision", "the assignment's current revision")
			stringArg("outcome", "done when the work is finished; omit otherwise")
		default:
			return errors.New("usage: assignment propose|confirm|release ...")
		}
		rest = rest[1:]
	case "send":
		op = "send"
		arityMin, arityMax = 2, 2
		stringArg("operation-id", "operation_id")
		stringArg("kind", "kind")
		stringArg("thread-id", "thread_id")
		stringArg("coordination", "coordination")
	case "inbox":
		op = "inbox"
		commonPage()
		f.Bool("unread-only", true, "return only unread messages")
	case "notification-status":
		op = "notification_status"
		f.Int64("after", 0, "exclusive sequence cursor")
	case "notification-policy":
		op = "set_notification_policy"
		stringArg("policy", "policy")
	case "inbox-status":
		op = "inbox_status"
		f.Int64("after", 0, "exclusive sequence cursor")
	case "mark-read":
		op = "mark_read"
		arityMin, arityMax = 1, 1000
	case "reply":
		stringArg("coordination", "coordination")
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
	case "workspace-domains":
		// Workspace owner only: accept only accounts managed by these Google
		// Workspace domains, or clear the list to accept any member's account.
		op = "workspace_set_domains"
		arityMax = 20
		f.Bool("clear", false, "clear the list, so any member's Google account is accepted")
	case "principal-unbind":
		// Server owner only: release an address from its Google account so
		// the next sign-in with it binds the account that now holds it.
		op = "principal_unbind"
		arityMin, arityMax = 1, 1
		f.Bool("confirm", false, "confirm releasing this address from its bound Google account")
		f.Bool("allow-previous", false, "let the released account bind again, for an unbind made by mistake")
	case "whoami":
		op = "whoami"
	case "status":
		op = "message_status"
		arityMin, arityMax = 1, 1
	case "doctor":
		// Handled above before executor construction so local diagnostics never
		// cache a listed project.
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
		case "local", "cloud", "name", "retention-days", "confirm", "clear":
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
		} else if op == "project_role_revoke" {
			a["principal"] = pos[0]
		} else if op == "coordinator_set" {
			a["target_session"] = ""
			if len(pos) > 0 {
				a["target_session"] = pos[0]
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
	case "workspace-domains":
		clear := f.Lookup("clear").Value.(flag.Getter).Get().(bool)
		if clear == (len(pos) > 0) {
			return errors.New("workspace-domains takes the allowed domains, or --clear to accept any account")
		}
		a["domains"] = append([]string{}, pos...)
	case "principal-unbind":
		if confirm := f.Lookup("confirm").Value.(flag.Getter).Get().(bool); !confirm {
			return errors.New("principal-unbind releases an address from its Google account; repeat with --confirm")
		}
		a["principal"] = pos[0]
	case "send":
		a["to"], a["body"] = pos[0], pos[1]
	case "reply":
		a["message_id"], a["body"] = pos[0], pos[1]
	case "mark-read":
		a["ids"] = pos
	case "assignment":
		if len(pos) > 0 {
			a["target_session"] = pos[0]
		}
		if v, ok := a["scope_path"]; ok {
			a["scope_paths"] = v
			delete(a, "scope_path")
		}
	case "status":
		a["message_id"] = pos[0]
	case "intake":
		if v, ok := a["intake_mode"]; ok {
			a["mode"] = v
			delete(a, "intake_mode")
		}
		switch op {
		case "intake_trust_add", "intake_trust_remove":
			a["linear_user_id"] = pos[0]
		case "intake_token_revoke":
			a["token_id"] = pos[0]
		case "intake_dismiss", "intake_rearm":
			a["issue_uuid"] = pos[0]
		}
	}
	if raw, ok := a["coordination"]; ok {
		var object any
		if json.Unmarshal([]byte(raw.(string)), &object) != nil {
			return errors.New("--coordination must be a JSON object")
		}
		c, e := bus.ParseCoordination(object)
		if e != nil {
			return e
		}
		a["coordination"] = c
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
		cloudClient, err = remote.New(remote.Config{
			URL: config.Server, Audience: config.Audience, Account: config.Account,
			ImpersonateServiceAccount: config.ImpersonateServiceAccount,
			Principal:                 config.Principal, CachePath: config.Cache,
			CredentialSource: config.CredentialSource, CredentialsFile: config.CredentialsFile,
			OAuthClientID: config.OAuthClientID, OAuthClientSecret: config.OAuthClientSecret,
			OAuthDeviceClientID: config.OAuthDeviceClientID, OAuthDeviceClientSecret: config.OAuthDeviceClientSecret,
			OAuthKeyringService: config.OAuthKeyringService,
		})
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
	// A server-wide unbind needs no project, and the allow-list belongs to
	// the configured workspace: neither resolves a project first.
	if op == "workspace_set_domains" && config.Workspace == "" {
		return errors.New("workspace-domains needs --workspace or a configured workspace")
	}
	if command != "setup" && op != "project_create" && op != "workspace_set_domains" && op != "principal_unbind" {
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
		if flags.LinearProfile != "" {
			if !validLinearProfile(flags.LinearProfile) {
				return errors.New("invalid explicit Linear profile")
			}
			directory, e := linearDirectory()
			if e != nil {
				return e
			}
			manager := newLinearManager(directory)
			board := newLinearService(directory, flags.LinearProfile, manager, executor, linearAuthority(config))
			// The MCP process lives for the whole client session, so it keeps one
			// native connection open instead of reconnecting on every tool call,
			// and read tools reuse a verified binding briefly. Writes do neither.
			board.KeepProviderOpen(linearboard.ProviderTTL)
			board.CacheReadBindingProof(linearboard.BindingProofTTL)
			defer board.Close()
			native := mcpserver.NewServerWithOptions(executor, config.Workspace, config.Project, config.Session, config.Instance, mcpserver.Options{Context: ctx, Taskboard: board})
			return native.Run(ctx, &mcp.StdioTransport{})
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
		case "register_session", "list_sessions", "history", "send", "reply", "inbox", "inbox_status", "notification_status", "set_notification_policy", "mark_read", "update_session", "retire_session", "whoami", "message_status", "project_role_set", "project_role_revoke", "purge_expired", "assignment_propose", "assignment_confirm", "assignment_release", "coordinator_set", "intake_policy_get", "intake_policy_set", "intake_trust_add", "intake_trust_remove", "intake_trust_list", "intake_token_create", "intake_token_list", "intake_token_revoke", "intake_list", "intake_dismiss", "intake_rearm":
			a["instance"] = config.Instance
		}
	}
	result, err := executor.Execute(ctx, bus.Request{Operation: op, Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: a})
	if err != nil {
		return err
	}
	if op == "assignment_confirm" || op == "assignment_release" {
		applyLinearMarker(ctx, cmp.Or(flags.LinearProfile, config.LinearProfile), executor, config, op, result)
	}
	if command == "setup" {
		if err = writeSetupConfig(configPath, config); err != nil {
			return fmt.Errorf("workspace initialized but config write failed: %w", err)
		}
	}
	return json.NewEncoder(stdout).Encode(result)
}

// applyLinearMarker applies the Linear claim marker of a confirm or release
// right away when a Linear profile is selected; otherwise, or when it
// cannot, the comment watcher applies it later. Either way the bus
// transition stands, and the result says where the marker is.
func applyLinearMarker(ctx context.Context, profile string, executor linearboard.Executor, config Config, op string, result bus.Result) {
	data, ok := result.Data.(map[string]any)
	if !ok {
		return
	}
	assignment, phase := "", "close"
	if op == "assignment_confirm" {
		phase = "claim"
		view, _ := data["assignment"].(map[string]any)
		assignment, _ = view["id"].(string)
		if issue, _ := view["issue_uuid"].(string); issue == "" {
			return
		}
	} else {
		assignment, _ = data["released"].(string)
	}
	if assignment == "" {
		return
	}
	if profile == "" || !validLinearProfile(profile) {
		data["linear_marker"] = map[string]any{"state": "pending", "note": "applied by the comment watcher (clawbridge linear watch), or pass --linear-profile to apply it now"}
		return
	}
	directory, err := linearDirectory()
	if err != nil {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	service := newLinearService(directory, profile, newLinearManager(directory), executor, linearAuthority(config))
	scope := bus.Request{Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: map[string]any{}}
	if config.Instance != "" {
		scope.Args["instance"] = config.Instance
	}
	data["linear_marker"] = service.ApplyMarker(bounded, scope, assignment, phase, nil)
}

// doctorSkills reads the skill installations of the agents configured here.
var doctorSkills = func() ([]skill.State, error) {
	targets, err := skill.Targets("", "")
	if err != nil {
		return nil, nil
	}
	return skill.Status(targets)
}

func collectDoctor(ctx context.Context, config Config, configPath string, existing doctor.Executor, selections ...doctor.MCPConfig) (doctor.Report, error) {
	in := doctor.Input{
		Config: doctor.Config{
			Path: configPath, Present: true, Valid: true,
			Mode: config.Mode, DB: config.DB, Workspace: config.Workspace,
			Project: config.Project, Session: config.Session, Instance: config.Instance,
			Server: config.Server, Audience: config.Audience,
			Account: config.Account, ImpersonateServiceAccount: config.ImpersonateServiceAccount,
			Principal: config.Principal, Cache: config.Cache,
			CredentialSource: config.CredentialSource, CredentialsFile: config.CredentialsFile,
			OAuthClientID: config.OAuthClientID, OAuthDeviceClientID: config.OAuthDeviceClientID,
			OAuthKeyringService: config.OAuthKeyringService,
		},
		Skills: doctorSkills,
	}
	var executor doctor.Executor = existing
	if config.Mode == "cloud" {
		routeConfig := remote.Config{
			URL: config.Server, Audience: config.Audience, Account: config.Account,
			ImpersonateServiceAccount: config.ImpersonateServiceAccount,
			Principal:                 config.Principal, CachePath: config.Cache,
			CredentialSource: config.CredentialSource, CredentialsFile: config.CredentialsFile,
			OAuthClientID: config.OAuthClientID, OAuthClientSecret: config.OAuthClientSecret,
			OAuthDeviceClientID: config.OAuthDeviceClientID, OAuthDeviceClientSecret: config.OAuthDeviceClientSecret,
			OAuthKeyringService: config.OAuthKeyringService,
		}
		tokens, err := remote.NewTokenSource(ctx, routeConfig)
		if err == nil {
			in.TokenSource = tokens
			if executor == nil {
				client, e := remote.NewWithTokenSource(routeConfig, tokens)
				if e == nil {
					executor = client
				}
			}
		}
	} else if executor == nil {
		store, err := bus.Open(config.DB)
		if err != nil {
			return doctor.Report{}, err
		}
		defer store.Close()
		executor = store
	}
	in.MCPConfigs = selections
	if config.LinearProfile != "" && validLinearProfile(config.LinearProfile) {
		directory, err := linearDirectory()
		if err == nil {
			manager := newLinearManager(directory)
			in.LinearCapabilities = func(ctx context.Context) (linearprovider.Capabilities, error) {
				bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				return linearCapabilities(bounded, manager, config.LinearProfile)
			}
		}
	}
	return doctor.Run(ctx, in, executor), nil
}

func runDoctor(ctx context.Context, config Config, configPath string, stdout io.Writer, selections ...doctor.MCPConfig) error {
	report, err := collectDoctor(ctx, config, configPath, nil, selections...)
	if err != nil {
		return err
	}
	if err = json.NewEncoder(stdout).Encode(report); err != nil {
		return err
	}
	if !report.OK() {
		return errors.New("doctor found required failures")
	}
	return nil
}
