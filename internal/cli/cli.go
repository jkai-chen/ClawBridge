// Package cli translates command arguments into the shared scoped bus API.
package cli

import (
	"clawbridge/internal/linearkey"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/auth"
	"clawbridge/internal/buildinfo"
	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/dispatchermodel"
	"clawbridge/internal/doctor"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/mcpserver"
	"clawbridge/internal/playbook"
	"clawbridge/internal/remote"
	"clawbridge/internal/server"
	"clawbridge/internal/skill"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Usage = `clawbridge: local and authenticated hosted agent messaging
Global flags: --db PATH --workspace NAME_OR_ID --project SLUG_OR_ID --session NAME_OR_ID --instance ID --json
Commands: setup --local, project create SLUG, register [NAME], sessions, send TO BODY --operation-id ID,
          inbox, inbox-status [--after N], mark-read ID..., reply ID BODY --operation-id ID, update,
          retire [--target-session S] [--successor S --status moved] [--outcome done | --confirm], history, whoami, status ID,
          notification-status [--after N], notification-policy --policy accept|hold|refuse,
          deadletters list [--target-session NAME|@all], deadletters requeue|dismiss MESSAGE_ID [--target-session NAME],
          assignment propose [--issue-uuid UUID --issue-identifier ID] [--task-role ROLE] [--scope-repo REMOTE] [--scope-path PATTERN]...,
          assignment confirm TARGET_SESSION --revision REV [--allow-overlap], assignment release [TARGET_SESSION] --revision REV,
          project coordinator-set SESSION, project coordinator-clear,
          sessions [--issue ID] [--scope-path PATH] [--assignment-state proposed|confirmed],
          host-status|host-wait --adapter codex --endpoint unix:///absolute/socket --thread UUID,
          notify --adapter json|codex --state ABSOLUTE_PATH [--thread UUID] [--interval 5s] [--once],
          linear COMMAND --help (native connection, scoped task board and task actions),
          runner launch ISSUE_UUID --profile ID [--role ROLE] [--dry-run], runner gc [--confirm] (Claude in an Orca terminal),
          mcp [--linear-profile ID], mcp install ... [--linear-profile ID] (opt-in native Linear tools),
          skill install|status|uninstall [--agent claude-code|codex|all] [--dir SKILLS_DIR] [--force],
          serve, operator principal-unbind|workspace-clear-domains ... --confirm (on the server host),
          sync, purge-expired, principal-unbind EMAIL --confirm [--allow-previous],
          workspace-domains DOMAIN...|--clear, doctor, tui [--screen SCREEN], version,
          cloud check [infra|server|version|workspace|linear-app|backups|all], cloud secret set KEY (own-server onboarding),
          project context-watch show|set --file FILE --expected-revision N,
          project watch-budget show|set CALLS_PER_HOUR|clear,
          context levels set [--warn-tokens N] [--handover-tokens N] | --clear,
          context show [--session NAME | --agent claude-code|codex --transcript PATH] [--json],
          context bind [--session NAME] --codex-thread UUID|--agent K --transcript PATH, context hooks install|uninstall [--scope local|user|project-shared]
Interactive: bare clawbridge opens the selected profile, or Start without one; onboard reopens Start
(local, join a team server, or your own server); setup wizard starts guided join.
Onboarding: setup join --server URL --workspace TEAM --project CODEBASE [--login|--device]
setup --cloud initializes an existing service only; no infrastructure is created.
Global: --client-config /absolute/path/config.toml (trusted route; not agent --config)
Commands: login [--device|--use-gcloud], logout,
          join --email EMAIL (with --server URL: join a hosted workspace with an invite; asks for the code),
          invite, device, user, owner, workspace (accounts on a hosted workspace; each takes --help)
Cloud flags: --mode cloud --server URL --audience AUDIENCE [--credential-source oauth|gcloud|adc|device] [--oauth-client-id ID --oauth-client-secret CREDENTIAL --oauth-device-client-id ID --oauth-device-client-secret CREDENTIAL] [--principal EMAIL] [--cache PATH]
Results are laid out for people at a terminal and are JSON with source otherwise: on a pipe, in an agent's shell, with --json
or CLAWBRIDGE_OUTPUT=json (--json=false or CLAWBRIDGE_OUTPUT=human asks for the layout). Scripts and agents: pass --json.
See docs/reference/api.md for command flags and isolated test environment variables.
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
		"device-id":             &c.DeviceID, "key-store": &c.KeyStore,
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
			c.JSONFlag = "true"
			if has {
				b, err := strconv.ParseBool(val)
				if err != nil {
					return c, nil, errors.New("--json expects a boolean")
				}
				c.JSONFlag = strconv.FormatBool(b)
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
	// K1: only linear key and linear view may read the person's Linear API
	// key; any other entry (mcp, runner, serve, notify, ...) can never.
	if !linearKeyEntry(args) {
		linearkey.Forbid()
	}
	// The output format, once, for every command (live/CLI-OUTPUT-PLAN.md
	// §2): --json, CLAWBRIDGE_OUTPUT, an agent's shell, a terminal.
	mode, err := clirender.Choose(jsonFlag(args), os.Environ(), stdout)
	if err != nil {
		return err
	}
	ctx = clirender.WithMode(ctx, mode)
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		if len(args) != 1 {
			return errors.New("version accepts no arguments")
		}
		return emit(ctx, stdout, "version", struct {
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
	// Context watching reads local files only.
	// `context levels` is the server's (the session's own levels); the rest
	// reads local files.
	if len(args) > 0 && args[0] == "context" && (len(args) < 2 || args[1] != "levels") {
		return runContext(ctx, args[1:], stdout, stderr)
	}
	// The queue's hooks find their route in the MCP server's record.
	if len(args) > 0 && args[0] == "hook" {
		return runHook(ctx, args[1:], stdout, stderr)
	}
	// The onboarding probes take cloud flags (--project is a cloud project
	// here) and resolve trusted configuration only for the workspace check.
	if len(args) > 0 && args[0] == "cloud" {
		return runCloud(ctx, args[1:], stdout, stderr)
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
	if rest[0] == "onboard" {
		return runOnboard(ctx, flags, rest[1:], stdout, stderr)
	}
	if rest[0] == "tui" {
		return runTUI(ctx, flags, rest[1:], stdout, stderr)
	}
	// A device's unacknowledged device events print on stderr after any
	// command it signs (§17.3); the TUI shows them on its Account screen.
	if rest[0] != "mcp" {
		remote.SetDeviceEventsNotice(printDeviceEvents(stderr))
		defer remote.SetDeviceEventsNotice(nil)
	}
	if rest[0] == "linear" {
		return runLinear(ctx, flags, rest[1:], stdout, stderr)
	}
	if rest[0] == "runner" {
		return runRunner(ctx, flags, rest[1:], stdout, stderr)
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
	if rest[0] == "join" {
		return runDeviceJoin(ctx, flags, rest[1:], os.Stdin, stdout, stderr)
	}
	switch rest[0] {
	case "invite", "device", "user", "owner", "workspace":
		return runAccount(ctx, flags, rest[0], rest[1:], stdout, stderr)
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
					return emit(ctx, stdout, "login", map[string]any{
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
			return emit(ctx, stdout, "logout", map[string]any{"logged_out": true, "client_id": config.OAuthClientID})
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
		return emit(ctx, stdout, "login", map[string]any{
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
	if command == "setup" && setupWantsCloud(rest) && flags.Mode == "" && envMode() == "" {
		// On a clean HOME, setup --cloud says which mode it initializes:
		// the default is local only until something says otherwise. A mode
		// set anywhere else still conflicts, as before.
		if path, e := selectedClientPath(flags); e == nil {
			if c, e := readConfig(path, false); e == nil && c.Mode == "" {
				flags.Mode = "cloud"
			}
		}
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
		f.Usage = func() {
			fmt.Fprintln(f.Output(), `usage:
  clawbridge setup --local [--name WORKSPACE] [--project PROJECT]
      Starts a local workspace on this machine. --project saves PROJECT as the
      default; create it with clawbridge project create PROJECT. Without it,
      the first project you create becomes the default.
  clawbridge --server URL --audience AUDIENCE [--oauth-client-id ID --oauth-client-secret SECRET
             --oauth-device-client-id ID --oauth-device-client-secret SECRET] setup --cloud --name WORKSPACE
      Initializes a workspace on your own hosted server, as its bootstrap owner.
      The routing flags come before "setup"; they are saved for later commands.
flags:`)
			f.PrintDefaults()
		}
	case "project":
		if len(rest) == 0 {
			return errors.New("usage: project create SLUG, project overview, project threads, project thread THREAD_ID, project role-set EMAIL ROLE, or project role-revoke EMAIL")
		}
		switch rest[0] {
		case "create":
			rest = rest[1:]
			op = "project_create"
			arityMin, arityMax = 1, 1
			stringArg("name", "name")
			stringArg("git-remote", "git_remote")
		case "overview":
			// Any member: the project, its sessions and its Linear board.
			rest = rest[1:]
			op = "project_overview"
		case "threads":
			// The project owner, without a session: the project's threads,
			// newest activity first. Every read is in the project's history.
			rest = rest[1:]
			op = "project_threads"
			f.Int("limit", 50, "threads per page (1–100)")
			stringArg("before", "the next cursor from the previous page")
		case "thread":
			rest = rest[1:]
			op = "project_thread"
			arityMin, arityMax = 1, 1
			f.Int("limit", 100, "messages per page (1–200)")
			f.Int64("after", 0, "exclusive sequence cursor")
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
		case "context-watch":
			// Context watching: any member reads it; the project owner sets it.
			switch {
			case len(rest) >= 2 && rest[1] == "show":
				op = "context_watch_get"
			case len(rest) >= 2 && rest[1] == "set":
				op = "context_watch_set"
				f.String("file", "", "the setting, JSON: {\"enabled\", \"levels\", \"extra_recipients\", \"handover_hint\"}")
				f.Int64("expected-revision", -1, "the setting's revision you last read (0 for none)")
			default:
				return errors.New("usage: project context-watch show; project context-watch set --file FILE --expected-revision N")
			}
			rest = rest[2:]
		case "watch-budget":
			// The comment watcher's hourly Linear budget for this project:
			// any member reads it; the project owner sets or clears it.
			switch {
			case len(rest) >= 2 && rest[1] == "show":
				op = "comment_watch_status"
			case len(rest) >= 2 && rest[1] == "set":
				op = "comment_watch_budget_set"
				arityMin, arityMax = 1, 1
			case len(rest) >= 2 && rest[1] == "clear":
				op = "comment_watch_budget_set"
			default:
				return errors.New("usage: project watch-budget show; project watch-budget set CALLS_PER_HOUR; project watch-budget clear")
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
		usage := errors.New("usage: intake policy [set --intake-mode off|notify --signals S1 ...] | trust add|remove LINEAR_USER_ID|NAME|EMAIL [--confirm] | trust list | token create [--expires-days N] | token list | token revoke TOKEN_ID | list [--state S] | dismiss|rearm ISSUE_UUID")
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
			// A name or email is looked up among the board's members
			// (--linear-profile) and shown before it changes anything.
			f.Bool("confirm", false, "with a name or email: make the change after it's resolved")
			if action == "add" {
				stringArg("principal", "link them to this ClawBridge project member (email), so their own writes through ClawBridge count")
			}
		case "trust list":
			op = "intake_trust_list"
		case "token create":
			op = "intake_token_create"
			f.Int("expires-days", 0, "days until the token expires (default 90, at most 365)")
			f.String("out", "", "write the token to this new file (0600, never over an existing one) instead of this terminal")
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
	case "dispatcher":
		// The dispatcher's policy: any member reads it; the owner sets the
		// domains; a domain's owner edits its trust list and coordinator.
		usage := errors.New("usage: dispatcher show; dispatcher enable [--engine rules|model] --expected-revision N [--confirm]; dispatcher disable --expected-revision N; dispatcher domains set --file FILE --expected-revision N; dispatcher domain trust|untrust|coordinator DOMAIN SESSION --expected-revision N; dispatcher decisions [--since-hours N] [--limit N]; dispatcher chat \"QUESTION\"; dispatcher chat show [CASE]; dispatcher chat leads on|off --expected-revision N; dispatcher model advise on|off --expected-revision N [--confirm]; dispatcher model tools set [route_notice] [ask_session] --expected-revision N [--confirm]; dispatcher model setup [--provider ID --model M [--base-url URL] --key-stdin --daily-tokens N [--caps K=V,…] --confirm-egress]; dispatcher model status; dispatcher model key delete --confirm; dispatcher escalations [--all] [--limit N]")
		switch {
		case len(rest) >= 1 && rest[0] == "show":
			op, rest = "dispatcher_get", rest[1:]
		case len(rest) >= 1 && rest[0] == "enable":
			// The project owner turns it on, in their own terminal: a preview
			// first, then --confirm.
			op, rest = "dispatcher_enable", rest[1:]
			stringArg("engine", "rules (default) or model; model needs your model setup to have passed its test")
			f.Bool("confirm", false, "turn it on; without it, show what would change")
			f.Int64("expected-revision", -1, "the dispatcher policy revision you last read (0 for none)")
		case len(rest) >= 1 && rest[0] == "disable":
			op, rest = "dispatcher_disable", rest[1:]
			f.Int64("expected-revision", -1, "the dispatcher policy revision you last read")
		case len(rest) >= 1 && rest[0] == "decisions":
			// The decision log, as the model reads it in a chat (D6-L4).
			op, rest = "dispatcher_decisions_list", rest[1:]
			f.Int("since-hours", 24, "how far back to read, 1 to 168 hours")
			f.Int("limit", 50, "at most this many decisions, 1 to 50")
		case len(rest) >= 2 && rest[0] == "chat" && rest[1] == "show":
			op, rest = "dispatcher_chat_show", rest[2:]
			arityMax = 1
		case len(rest) >= 2 && rest[0] == "chat" && rest[1] == "leads":
			op, rest = "dispatcher_chat_leads", rest[2:]
			f.Int64("expected-revision", -1, "the dispatcher policy revision you last read")
			arityMin, arityMax = 1, 1
		case len(rest) >= 1 && rest[0] == "chat":
			// A question to the model, from a person's terminal only [DM-M1].
			op, rest = "dispatcher_chat_ask", rest[1:]
			arityMin, arityMax = 1, 1
		case len(rest) >= 2 && rest[0] == "model" && rest[1] == "advise":
			// The model as the advisor in rules mode (d7): the owner, in
			// their own terminal, a preview first.
			op, rest = "dispatcher_model_advise", rest[2:]
			f.Bool("confirm", false, "make the change; without it, show what it means")
			f.Int64("expected-revision", -1, "the dispatcher policy revision you last read")
			arityMin, arityMax = 1, 1
		case len(rest) >= 2 && rest[0] == "model" && rest[1] == "setup":
			// The owner's model setup (§11.1). A key is never an argument:
			// one that looks like it stops everything first.
			for _, arg := range rest[2:] {
				if dispatchermodel.LooksLikeCredential(arg) {
					return errKeyInArgs
				}
			}
			op, rest = "dispatcher_model_setup", rest[2:]
			stringArg("provider", "the provider's ID (dispatcher model setup lists them)")
			stringArg("model", "the model ID, as the provider names it")
			stringArg("base-url", "for openai-compatible only: the endpoint's base URL")
			f.Bool("key-stdin", false, "read the key from stdin (then every other answer comes from flags); never a flag or env variable")
			stringArg("caps", "caps as key=number pairs: runs_per_hour, runs_per_day, max_input_tokens_per_run, max_output_tokens_per_run, daily_tokens")
			f.Int64("daily-tokens", 0, "your daily token ceiling, across your projects (required)")
			f.Bool("confirm-egress", false, "confirm the egress disclosure setup prints")
		case len(rest) >= 2 && rest[0] == "model" && rest[1] == "status":
			op, rest = "dispatcher_model_doctor", rest[2:]
		case len(rest) >= 3 && rest[0] == "model" && rest[1] == "key" && rest[2] == "delete":
			op, rest = "dispatcher_model_key_delete", rest[3:]
			f.Bool("confirm", false, "delete your key; your agent stops and the model goes off in your projects")
		case len(rest) >= 3 && rest[0] == "model" && rest[1] == "tools" && rest[2] == "set":
			op, rest = "dispatcher_model_tools", rest[3:]
			f.Bool("confirm", false, "make the change; without it, show what it means")
			f.Int64("expected-revision", -1, "the dispatcher policy revision you last read")
			arityMax = len(bus.OptInTools)
		case len(rest) >= 1 && rest[0] == "escalations":
			// Escalations with the model's recommendations and notes: model
			// output, so a person's terminal only [DM-M1].
			op, rest = "dispatcher_escalations_list", rest[1:]
			f.Bool("all", false, "include escalations whose claim was already decided")
			f.Int("limit", 50, "at most this many, 1 to 50")
		case len(rest) >= 2 && rest[0] == "domains" && rest[1] == "set":
			op, rest = "dispatcher_domains_set", rest[2:]
			f.String("file", "", "the domains document, JSON: {\"domains\": [...]}")
			f.Int64("expected-revision", -1, "the dispatcher policy revision you last read (0 for none)")
		case len(rest) >= 2 && rest[0] == "domain" && (rest[1] == "trust" || rest[1] == "untrust" || rest[1] == "coordinator"):
			op = "dispatcher_domain_edit"
			a["action"], rest = rest[1], rest[2:]
			f.Int64("expected-revision", -1, "the dispatcher policy revision you last read")
			arityMin, arityMax = 2, 2
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
		stringArg("outcome", "outcome")
		f.Bool("confirm", false, "release a confirmed assignment as it is, with no outcome")
	case "deadletters":
		// A session's dead letters: the session itself, the project's
		// coordinator or a project owner; --target-session @all lists the
		// project's. Rows carry no body.
		usage := "usage: deadletters list [--target-session NAME|@all], or deadletters requeue|dismiss MESSAGE_ID [--target-session NAME]"
		if len(rest) == 0 {
			return errors.New(usage)
		}
		switch rest[0] {
		case "list":
			op = "deadletters_list"
			f.Int("limit", 100, "page size (1–1000)")
		case "requeue", "dismiss":
			op = "deadletters_" + rest[0]
			arityMin, arityMax = 1, 1
		default:
			return errors.New(usage)
		}
		rest = rest[1:]
		stringArg("target-session", "target_session")
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
	case "context":
		// Only `context levels set` reaches here; see above.
		if len(rest) < 2 || rest[1] != "set" {
			return errors.New("usage: context levels set [--warn-tokens N] [--handover-tokens N] | --clear")
		}
		op, rest = "context_levels_set", rest[2:]
		f.Int64("warn-tokens", 0, "this session's own warn level, below the project's")
		f.Int64("handover-tokens", 0, "this session's own handover level, below the project's")
		f.Bool("clear", false, "go back to the project's levels")
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
		case "local", "cloud", "name", "retention-days", "confirm", "clear", "out":
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
	if op == "context_levels_set" && f.Lookup("clear").Value.(flag.Getter).Get().(bool) {
		a["clear"] = true
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
		} else if op == "comment_watch_budget_set" {
			a["watch_budget"] = nil
			if len(pos) == 1 {
				n, err := strconv.Atoi(pos[0])
				if err != nil || n < 1 || n > 600 {
					return errors.New("project watch-budget set takes 1–600 Linear calls per hour")
				}
				a["watch_budget"] = n
			}
		} else if op == "context_watch_set" {
			if v, _ := a["expected_revision"].(int64); v < 0 || a["expected_revision"] == nil {
				return errors.New("project context-watch set requires --expected-revision")
			}
			path, _ := a["file"].(string)
			delete(a, "file")
			if path == "" {
				return errors.New("project context-watch set requires --file")
			}
			raw, err := readLinearFile(path)
			if err != nil {
				return err
			}
			var doc any
			if err = json.Unmarshal([]byte(raw), &doc); err != nil {
				return errors.New("the context-watch file must hold one JSON object")
			}
			a["context_watch"] = doc
		} else if op == "project_thread" {
			a["thread_id"] = pos[0]
		} else if op == "context_watch_get" || op == "comment_watch_status" || op == "project_overview" || op == "project_threads" {
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
	case "retire":
		if f.Lookup("confirm").Value.(flag.Getter).Get().(bool) {
			a["confirm"] = true
		}
	case "send":
		a["to"], a["body"] = pos[0], pos[1]
	case "reply":
		a["message_id"], a["body"] = pos[0], pos[1]
	case "deadletters":
		if len(pos) == 1 {
			a["message_id"] = pos[0]
		}
	case "mark-read":
		a["ids"] = pos
	case "dispatcher":
		switch op {
		case "dispatcher_chat_ask":
			a["text"] = pos[0]
		case "dispatcher_chat_show":
			if len(pos) == 1 {
				a["case_id"] = pos[0]
			}
		case "dispatcher_chat_leads":
			if pos[0] != "on" && pos[0] != "off" {
				return errors.New("usage: dispatcher chat leads on|off --expected-revision N")
			}
			a["allow"] = pos[0] == "on"
		case "dispatcher_model_advise":
			if pos[0] != "on" && pos[0] != "off" {
				return errors.New("usage: dispatcher model advise on|off --expected-revision N [--confirm]")
			}
			a["on"] = pos[0] == "on"
		case "dispatcher_model_tools":
			tools := []any{}
			for _, t := range pos {
				tools = append(tools, t)
			}
			a["tools"] = tools
		case "dispatcher_decisions_list":
			for k, v := range map[string]int{"since_hours": 24, "limit": 50} {
				if a[k] == nil {
					a[k] = v
				}
			}
		}
		if op == "dispatcher_model_key_delete" && !f.Lookup("confirm").Value.(flag.Getter).Get().(bool) {
			return errors.New("dispatcher model key delete deletes your model key: your agent stops, and the model goes off in every project of yours. Add --confirm to do it")
		}
		switch op {
		case "dispatcher_get", "dispatcher_decisions_list", "dispatcher_chat_ask", "dispatcher_chat_show", "dispatcher_escalations_list",
			"dispatcher_model_setup", "dispatcher_model_doctor", "dispatcher_model_key_delete":
		default:
			if v, _ := a["expected_revision"].(int64); v < 0 || a["expected_revision"] == nil {
				return errors.New(op + " requires --expected-revision")
			}
		}
		if op == "dispatcher_domains_set" {
			path, _ := a["file"].(string)
			delete(a, "file")
			if path == "" {
				return errors.New("dispatcher domains set requires --file")
			}
			raw, err := readLinearFile(path)
			if err != nil {
				return err
			}
			var doc any
			if err = json.Unmarshal([]byte(raw), &doc); err != nil {
				return errors.New("the domains file must hold one JSON object")
			}
			a["dispatcher_domains"] = doc
		}
		if op == "dispatcher_domain_edit" {
			a["domain"], a["target_session"] = pos[0], pos[1]
		}
		if v, ok := a["engine"]; ok {
			a["mode"] = v
			delete(a, "engine")
		}
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
		cloudClient, err = remote.New(remoteConfig(config))
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
		switch {
		case e == nil:
			config.Workspace, config.Project = workspace, project
		case op == "whoami" && config.Project == "" && config.Session == "" && config.Workspace != "" && noProjects(ctx, executor, config.Workspace):
			// A member with no project yet: the server answers whoami for
			// the workspace alone. With any project, an unresolved or
			// ambiguous one is still the error.
		default:
			return e
		}
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
			native := mcpserver.NewServerWithOptions(executor, config.Workspace, config.Project, config.Session, config.Instance, mcpserver.Options{Context: ctx, Taskboard: board, ContextDir: contextDirOrEmpty(), Server: queueServer(config), DB: queueDB(config)})
			return native.Run(ctx, &mcp.StdioTransport{})
		}
		if cloudClient != nil {
			return mcpserver.RunExecutor(ctx, cloudClient, config.Workspace, config.Project, config.Session, config.Instance, mcpserver.Options{ContextDir: contextDirOrEmpty(), Server: config.Server})
		}
		if err = localStore.Close(); err != nil {
			return err
		}
		return mcpserver.RunLocal(ctx, config.DB, config.Workspace, config.Project, config.Session, config.Instance, mcpserver.Options{ContextDir: contextDirOrEmpty()})
	}
	if command == "sync" {
		result, e := cloudClient.Sync(ctx, config.Workspace, config.Project, config.Session, config.Instance)
		if e != nil {
			return e
		}
		return emit(ctx, stdout, "sync", result)
	}
	if config.Instance != "" {
		switch op {
		case "register_session", "list_sessions", "history", "send", "reply", "inbox", "inbox_status", "notification_status", "set_notification_policy", "mark_read", "update_session", "retire_session", "whoami", "message_status", "project_role_set", "project_role_revoke", "purge_expired", "assignment_propose", "assignment_confirm", "assignment_release", "coordinator_set", "intake_policy_get", "intake_policy_set", "intake_trust_add", "intake_trust_remove", "intake_trust_list", "intake_token_create", "intake_token_list", "intake_token_revoke", "intake_list", "intake_dismiss", "intake_rearm", "context_levels_set", "deadletters_list", "deadletters_requeue", "deadletters_dismiss":
			a["instance"] = config.Instance
		}
	}
	if op == "project_threads" || op == "project_thread" {
		// A thread read is a person's act. The server refuses any session;
		// the agent check here is best effort, as an agent can clear its
		// environment.
		if flags.Session != "" {
			return errors.New("the project's threads are read without --session; the project owner runs this in their own terminal")
		}
		if marker := agentMarker(closesEnv()); marker != "" {
			return fmt.Errorf("reading the project's threads is the project owner's, and %s says an agent runs this command; run it yourself in your own terminal", marker)
		}
		config.Session = ""
	}
	if op == "dispatcher_chat_ask" || op == "dispatcher_chat_show" {
		return runDispatcherChat(ctx, executor, config, op, a, stdout)
	}
	if op == "dispatcher_model_setup" {
		return runModelSetup(ctx, executor, config, a, stdout)
	}
	if (op == "intake_trust_add" || op == "intake_trust_remove") && !linearprovider.ValidID(a["linear_user_id"].(string)) {
		who := strings.TrimSpace(a["linear_user_id"].(string))
		member, e := resolveTrustee(ctx, executor, config, who)
		if e != nil {
			return e
		}
		if e = confirmTrustee(stderr, op, member, f.Lookup("confirm").Value.(flag.Getter).Get().(bool)); e != nil {
			return e
		}
		a["linear_user_id"] = member.ID
		// Q2: found by their email, they link to the project member with
		// that email, when there is one; --principal overrides it.
		if op == "intake_trust_add" && a["principal"] == nil && member.Email != "" && strings.EqualFold(who, member.Email) {
			a["link_email"] = who
		}
	}
	// G9 F2: an intake token goes to this terminal or a new file, decided
	// before it exists.
	var secret *secretOut
	if op == "intake_token_create" {
		if secret, err = openSecretOut("clawbridge intake token create", f.Lookup("out").Value.String(), stdout); err != nil {
			return err
		}
	}
	result, err := executor.Execute(ctx, bus.Request{Operation: op, Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: a})
	if err != nil {
		secret.discard()
		if op == "retire_session" && strings.Contains(err.Error(), "holds the confirmed assignment") {
			return retireAgain(err, config.Session, config.Instance, rest)
		}
		return err
	}
	if secret.toFile() {
		data, _ := result.Data.(map[string]any)
		token, _ := data["token"].(string)
		if token == "" {
			secret.discard()
			return errors.New("the server's answer had no intake token; revoke it and create another")
		}
		if err = secret.write(token + "\n"); err != nil {
			_, _ = executor.Execute(ctx, bus.Request{Operation: "intake_token_revoke", Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: map[string]any{"instance": config.Instance, "token_id": data["id"]}})
			return fmt.Errorf("%w; the new intake token was revoked", err)
		}
		delete(data, "token")
		data["token_file"] = secret.path
		data["note"] = "Wrote the token to " + secret.path + " (0600): put it in Linear's MCP server header Authorization: Bearer <token>, then delete the file."
	}
	if op == "intake_trust_add" {
		trustLinkNote(stderr, a, result)
	}
	if op == "dispatcher_decisions_list" || op == "dispatcher_escalations_list" || op == "project_overview" || op == "project_threads" || op == "project_thread" {
		// The log and the escalations carry the model's explanations: model
		// output (D6-L1). The overview carries session and Linear names,
		// which other people chose; threads, other people's and agents' text.
		result.Data = terminalSafeAll(result.Data)
	}
	if op == "whoami" {
		result.Data = playbook.Annotate(ctx, result.Data, ".")
	}
	if op == "assignment_confirm" || op == "assignment_release" {
		applyLinearMarker(ctx, cmp.Or(flags.LinearProfile, config.LinearProfile), executor, config, op, result)
	}
	if command == "setup" {
		if err = writeSetupConfig(configPath, config); err != nil {
			return fmt.Errorf("workspace initialized but config write failed: %w", err)
		}
		// The global --project, given to setup --local, is saved as the
		// default project; project create makes it.
		if p := flags.Project; p != "" && config.Mode == "local" {
			if err = writeDefaultProject(configPath, p, true); err != nil {
				return fmt.Errorf("workspace initialized but config write failed: %w", err)
			}
		}
	}
	// A local workspace's first project becomes its default, so a new
	// terminal needs no --project. One tied to a git remote resolves by the
	// remote instead, in its checkout.
	if op == "project_create" && config.Mode == "local" && a["git_remote"] == nil {
		if slug, _ := a["slug"].(string); slug != "" {
			if err = writeDefaultProject(configPath, slug, false); err != nil {
				return fmt.Errorf("project created but config write failed: %w", err)
			}
		}
	}
	return emit(ctx, stdout, op, result)
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
			DeviceID:            config.DeviceID, KeyStore: config.KeyStore,
		},
		Skills: doctorSkills,
	}
	if config.Session != "" {
		in.Context = func() doctor.ContextState { return doctorContext(config.Session) }
		in.Queue = func() doctor.QueueState { return doctorQueue(config.Session) }
		in.Playbook = func() playbook.State { return playbook.Find(ctx, ".") }
	}
	var executor doctor.Executor = existing
	if config.Mode == "cloud" && config.CredentialSource == remote.CredentialSourceDevice {
		routeConfig := remoteConfig(config)
		signer, err := remote.NewDeviceSigner(routeConfig)
		in.Device = &doctor.Device{Err: errors.Unwrap(err)}
		if err == nil {
			in.Device.ClockOffset, in.Device.OffsetAt = signer.Offset()
			if executor == nil {
				if client, e := remote.NewWithSigner(routeConfig, signer, &http.Client{Timeout: 20 * time.Second}); e == nil {
					executor = client
				}
			}
		} else if in.Device.Err == nil {
			in.Device.Err = err
		}
	} else if config.Mode == "cloud" {
		routeConfig := remoteConfig(config)
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
			in.LinearAPIKey = func() (string, bool) { return linearKeys(directory).Recorded(config.LinearProfile) }
		}
	}
	return doctor.Run(ctx, in, executor), nil
}

func runDoctor(ctx context.Context, config Config, configPath string, stdout io.Writer, selections ...doctor.MCPConfig) error {
	report, err := collectDoctor(ctx, config, configPath, nil, selections...)
	if err != nil {
		return err
	}
	if err = emit(ctx, stdout, "doctor", report); err != nil {
		return err
	}
	if !report.OK() {
		return errors.New("doctor found required failures")
	}
	return nil
}

// setupWantsCloud reports whether setup's arguments ask for --cloud.
func setupWantsCloud(args []string) bool {
	for _, arg := range args {
		if arg == "--cloud" || arg == "-cloud" || arg == "--cloud=true" {
			return true
		}
	}
	return false
}

// retireAgain adds the two commands that retire a session holding a
// confirmed assignment: done if the work is finished, or as it is.
func retireAgain(err error, session, instance string, rest []string) error {
	again := "clawbridge "
	if session != "" {
		again += "--session " + session + " "
	}
	if instance != "" {
		again += "--instance " + instance + " "
	}
	again += strings.TrimSpace("retire " + strings.Join(rest, " "))
	return fmt.Errorf("%w\nFinished:   %s --outcome done\nAs it is:   %s --confirm", err, again, again)
}

// jsonFlag is --json as given anywhere before "--" (a message body after
// it is never a flag); extractGlobals still checks its value.
func jsonFlag(args []string) clirender.Flag {
	flag := clirender.FlagUnset
	for _, arg := range args {
		if arg == "--" {
			break
		}
		name, val, has := strings.Cut(arg, "=")
		if name != "--json" {
			continue
		}
		flag = clirender.FlagTrue
		if b, err := strconv.ParseBool(val); has && err == nil && !b {
			flag = clirender.FlagFalse
		}
	}
	return flag
}

// noProjects reports whether the caller has no project in workspace: the
// one case where whoami answers without resolving one.
func noProjects(ctx context.Context, executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}, workspace string) bool {
	r, err := executor.Execute(ctx, bus.Request{Operation: "list_projects", Workspace: workspace, Args: map[string]any{}})
	if err != nil {
		return false
	}
	d, ok := r.Data.(map[string]any)
	if !ok {
		return false
	}
	projects, ok := d["projects"].([]any)
	if !ok {
		// A local store answers with typed rows.
		rows, typed := d["projects"].([]map[string]any)
		return typed && len(rows) == 0
	}
	return len(projects) == 0
}
