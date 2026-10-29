// Package doctor inspects trusted routing, authority health and local client
// integration without modifying configuration or printing credentials.
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/mirror"
	"clawbridge/internal/playbook"
	"clawbridge/internal/remote"
	"clawbridge/internal/skill"
	"github.com/pelletier/go-toml/v2"
)

const (
	Pass = "pass"
	Warn = "warn"
	Fail = "fail"
)

// Executor is the shared application boundary. Doctor only invokes nonmutating
// operations through it.
type Executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
	ResolveProject(context.Context, string, string, string) (string, string, error)
}

// TokenSource validates the selected hosted credential route by minting an
// ephemeral token. Doctor never retains or renders the returned token.
type TokenSource interface {
	Token(context.Context) (string, error)
}

// Config is a plain, importable view of trusted routing. Doctor treats these as
// already resolved values and labels their source; it does not implement CLI
// config precedence.
type Config struct {
	Path                      string `json:"path"`
	Present                   bool   `json:"present"`
	Valid                     bool   `json:"valid"`
	Mode                      string `json:"mode"`
	DB                        string `json:"db"`
	Workspace                 string `json:"workspace"`
	Project                   string `json:"project"`
	Session                   string `json:"session"`
	Instance                  string `json:"instance"`
	Server                    string `json:"server"`
	Audience                  string `json:"audience"`
	Account                   string `json:"account"`
	ImpersonateServiceAccount string `json:"impersonate_service_account"`
	Principal                 string `json:"principal"`
	Cache                     string `json:"cache"`
	CredentialSource          string `json:"credential_source"`
	CredentialsFile           string `json:"credentials_file,omitempty"`
	OAuthClientID             string `json:"oauth_client_id,omitempty"`
	OAuthDeviceClientID       string `json:"oauth_device_client_id,omitempty"`
	OAuthKeyringService       string `json:"oauth_keyring_service,omitempty"`
	DeviceID                  string `json:"device_id,omitempty"`
	KeyStore                  string `json:"key_store,omitempty"`
}

// Device is what the client found of a device route's key: Err is why it
// could not be read (a file key's mode included), and the clock correction
// in use.
type Device struct {
	Err         error
	ClockOffset time.Duration
	OffsetAt    time.Time
}

// MCPConfig identifies one installed-client configuration and the expected
// ClawBridge entry. Doctor reads these paths and never repairs them.
type MCPConfig struct {
	LinearProfile             string `json:"linear_profile,omitempty"`
	ClientConfig              string `json:"client_config,omitempty"`
	Agent                     string `json:"agent"`
	Name                      string `json:"name"`
	Path                      string `json:"path"`
	DB                        string `json:"db"`
	Workspace                 string `json:"workspace"`
	Project                   string `json:"project"`
	Session                   string `json:"session"`
	Instance                  string `json:"instance"`
	Mode                      string `json:"mode"`
	Server                    string `json:"server"`
	Audience                  string `json:"audience"`
	Account                   string `json:"account"`
	ImpersonateServiceAccount string `json:"impersonate_service_account"`
	Principal                 string `json:"principal"`
	Cache                     string `json:"cache"`
	CredentialSource          string `json:"credential_source"`
	CredentialsFile           string `json:"credentials_file,omitempty"`
	OAuthClientID             string `json:"oauth_client_id,omitempty"`
	OAuthDeviceClientID       string `json:"oauth_device_client_id,omitempty"`
	OAuthKeyringService       string `json:"oauth_keyring_service,omitempty"`
	DeviceID                  string `json:"device_id,omitempty"`
	KeyStore                  string `json:"key_store,omitempty"`
}

// Input controls deterministic diagnostics. Now and MirrorStaleAfter are
// injected so freshness tests do not sleep or depend on wall-clock timing.
type Input struct {
	Config           Config
	GitRemote        string
	Now              time.Time
	MirrorStaleAfter time.Duration
	TokenSource      TokenSource
	// Device describes a device route's key; nil for any other route.
	Device     *Device
	MCPConfigs []MCPConfig
	// LinearCapabilities, when a personal Linear profile is configured, reads
	// the provider's negotiated capabilities from its advertised tool list.
	// It performs no Linear write. Nil skips the Linear section.
	LinearCapabilities func(context.Context) (linearprovider.Capabilities, error)
	// LinearAPIKey says whether the profile has a Linear API key saved, and
	// in which store, from its marker: doctor never reads the key. Nil skips
	// the check.
	LinearAPIKey func() (store string, saved bool)
	// Skills reports the agent skill installations. Nil skips the check.
	Skills func() ([]skill.State, error)
	// Context reads this session's context watching state, locally. Nil
	// skips the check.
	Context func() ContextState
	// Queue reads this session's message queue state, locally; the counts
	// are the server's. Nil skips the check.
	Queue func() QueueState
	// Playbook reads the project playbook's state in this checkout, for the
	// coordinator session. Nil skips the check.
	Playbook func() playbook.State
	// WorkspaceUsers lists the Linear workspace's users through this
	// person's own connection, for the team roles check. Nil skips that
	// part of it.
	WorkspaceUsers func(ctx context.Context, wid, pid string) ([]linearprovider.WorkspaceUser, error)
	// TeamRolesDoc reads the generated "Team roles" document's state
	// through this person's own connection (the project owner's only). Nil
	// skips the check.
	TeamRolesDoc func(ctx context.Context, wid, pid string) (RolesDocState, error)
	// LabelSets reads the bound teams' label sets through the personal
	// Linear profile; reads only. Nil skips the check.
	LabelSets func(context.Context) ([]LabelSet, error)
	// TeamsWithoutProject reads the binding's teams listed without a
	// project, each as Linear shows it now, through the personal Linear
	// profile; reads only. Nil skips the check, and so does an empty list.
	TeamsWithoutProject func(ctx context.Context, wid, pid string) ([]TeamWithoutProject, error)
}

// TeamWithoutProject is a team the binding lists without a project, by
// its label in doctor's words ("T: no project yet (inert)"), and whether
// it is still inert.
type TeamWithoutProject struct {
	Label string
	Inert bool
}

// RolesDocState is the generated team roles document as doctor reports
// it: published or not, the revisions, and what Linear shows.
type RolesDocState struct {
	Published, Pending, Edited, Archived, Unreadable bool
	Revision, PublishedRevision                      int
	Replaced                                         string
}

// Check is one stable machine-readable diagnostic.
type Check struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
	Action  string `json:"action,omitempty"`
	// Aliases are earlier IDs of the same check, kept for one release so
	// a script reading the old ID still finds it.
	Aliases []string `json:"aliases,omitempty"`
}

// Report has an aggregate status derived from the most severe check.
type Report struct {
	Status      string  `json:"status"`
	GeneratedAt string  `json:"generated_at"`
	Checks      []Check `json:"checks"`
}

// OK means there are no required failures. Warnings do not make doctor fail.
func (r Report) OK() bool { return r.Status != Fail }

// ExitCode returns zero for a passing or warning-only report.
func (r Report) ExitCode() int {
	if r.OK() {
		return 0
	}
	return 1
}

// Run performs read-only diagnostics. It does not open a database, change agent
// configuration, sync a mirror or perform any hosted write.
func Run(ctx context.Context, in Input, executor Executor) Report {
	if ctx == nil {
		ctx = context.Background()
	}
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	if in.MirrorStaleAfter <= 0 {
		in.MirrorStaleAfter = 24 * time.Hour
	}
	report := Report{GeneratedAt: in.Now.UTC().Format(time.RFC3339)}
	add := func(check Check) { report.Checks = append(report.Checks, check) }

	configOK := checkConfig(in.Config, add)
	authorityOK, projectsOK := checkAuthority(ctx, in.Config, executor, add)
	if authorityOK && executor != nil {
		checkServerMaintenance(executor, add)
	}
	checkClientOutbox(ctx, in.Config, add)
	checkCredentials(ctx, in, configOK, add)
	wid, pid, resolutionOK := checkResolution(ctx, in.Config, in.GitRemote, executor, authorityOK, projectsOK, add)
	checkMirror(ctx, in, wid, pid, resolutionOK, add)
	// The binding is read only for a Linear user (a selected Linear
	// profile); otherwise doctor stays list-and-resolve only.
	if in.LinearCapabilities != nil && resolutionOK && executor != nil {
		checkTaskboardBinding(ctx, executor, wid, pid, add)
		checkCommentWatch(ctx, executor, wid, pid, add)
		checkLabelPolicy(ctx, executor, wid, pid, add)
		checkTeamRoles(ctx, executor, wid, pid, in.WorkspaceUsers, add)
		checkTeamRolesDoc(ctx, wid, pid, in.TeamRolesDoc, add)
		checkLinearWrites(ctx, executor, wid, pid, add)
		checkProjectTeamAdds(ctx, executor, wid, pid, add)
		if in.TeamsWithoutProject != nil {
			checkTeamsWithoutProject(ctx, in.TeamsWithoutProject, wid, pid, add)
		}
		checkProvenGates(ctx, executor, wid, pid, add)
		if in.LabelSets != nil {
			checkLabelSets(ctx, in.LabelSets, add)
		}
		if in.Config.Mode == "cloud" {
			checkLinearApp(ctx, executor, wid, pid, add)
			checkIssueSubscribers(ctx, executor, wid, pid, add)
		}
		// The dispatcher needs the task board, so its owner is a Linear
		// user; its model mode is shown to a project owner only.
		checkDispatcherModel(ctx, executor, wid, pid, add)
	}
	for _, mcp := range in.MCPConfigs {
		if mcp.Agent == "" {
			mcp.Agent = "codex"
		}
		if mcp.Name == "" {
			mcp.Name = "clawbridge"
		}
		checkMCP(mcp, add)
	}
	if in.LinearCapabilities != nil {
		checkLinearCapabilities(ctx, in.LinearCapabilities, add)
	}
	if in.LinearAPIKey != nil {
		checkLinearAPIKey(in.LinearAPIKey, add)
	}
	if in.Skills != nil {
		checkSkills(in.Skills, add)
	}
	if in.Context != nil {
		checkContext(in.Context(), add)
	}
	if in.Queue != nil {
		var queueExecutor Executor
		if authorityOK {
			queueExecutor = executor
		}
		checkQueue(ctx, in.Queue(), in.Config, queueExecutor, in.Now, add)
	}
	if in.Playbook != nil && authorityOK {
		checkPlaybook(ctx, in.Playbook, in.Config, executor, add)
	}
	if len(in.MCPConfigs) == 0 {
		add(Check{
			ID: "mcp.entries", Title: "Installed MCP entries", Status: Warn,
			Message: "No agent MCP configuration paths were selected for inspection.",
			Action:  "Pass explicit temporary or installed paths when an MCP entry should be checked.",
		})
	}
	report.Status = aggregate(report.Checks)
	return report
}

// checkLinearCapabilities makes a silent capability switch-off visible. The
// capabilities are optional, so an off state is a warning, never a failure.
func checkLinearCapabilities(ctx context.Context, read func(context.Context) (linearprovider.Capabilities, error), add func(Check)) {
	check := Check{ID: "linear.capabilities", Title: "Linear capabilities"}
	caps, err := read(ctx)
	if err != nil {
		check.Status = Warn
		check.Message = "Linear capabilities could not be read (" + safeCode(err) + ")."
		check.Action = "Check the personal Linear profile with `clawbridge linear status --profile ID`."
		add(check)
		return
	}
	flags := capabilityFlags(caps)
	parts, off := []string{}, []string{}
	for _, flag := range flags {
		state := "on"
		if !flag.on {
			state = "off"
		}
		parts = append(parts, flag.key+"="+state)
		if reason := caps.Reasons[flag.key]; !flag.on && reason != "" {
			off = append(off, flag.key+": "+reason)
		}
	}
	check.Status = Pass
	check.Message = strings.Join(parts, ", ") + "."
	if slices.ContainsFunc(flags, func(f capabilityFlag) bool { return !f.on }) {
		check.Status = Warn
		if len(off) > 0 {
			check.Message += " Off because " + strings.Join(off, "; ") + "."
		}
		check.Action = "Commands that need an off capability are refused until it returns; report a provider schema change to the coordinator."
	}
	add(check)
}

// checkLinearAPIKey reports the person's Linear API key from its marker
// only: the key is read just by linear key and linear view (K1).
func checkLinearAPIKey(saved func() (string, bool), add func(Check)) {
	check := Check{ID: "linear.api_key", Title: "Linear API key", Status: Pass}
	store, ok := saved()
	if !ok {
		check.Message = "No Linear API key is saved; one is needed only to write Linear views."
		add(check)
		return
	}
	check.Message = "A Linear API key is saved (" + store + " store). It acts as you everywhere in Linear, and any program running as you can read it; doctor doesn't read it."
	check.Action = "Check it with `clawbridge linear key status --profile ID`; revoke it in Linear and run `clawbridge linear key remove` when you don't need it."
	add(check)
}

// checkSkills warns when no agent skill is installed, or when an installed
// one is not the skill this binary carries.
func checkSkills(read func() ([]skill.State, error), add func(Check)) {
	check := Check{ID: "skill.version", Title: "Agent skill", Status: Pass}
	states, err := read()
	if err != nil {
		check.Status = Warn
		check.Message = "Agent skill installations could not be read."
		check.Action = "Run `clawbridge skill status` for details."
		add(check)
		return
	}
	parts, force := []string{}, false
	for _, s := range states {
		switch {
		case !s.Installed:
			parts = append(parts, s.Agent+": not installed")
		case len(s.Modified) > 0:
			check.Status, force = Warn, true
			parts = append(parts, s.Agent+": "+strings.Join(s.Modified, ", ")+" changed since installation")
		case !s.Current:
			check.Status = Warn
			parts = append(parts, s.Agent+": installed "+s.InstalledVersion+", binary "+s.BinaryVersion)
		default:
			parts = append(parts, s.Agent+": current ("+s.InstalledVersion+")")
		}
	}
	if !slices.ContainsFunc(states, func(s skill.State) bool { return s.Installed }) {
		check.Status = Warn
		check.Message = "No ClawBridge agent skill is installed."
		if len(parts) > 0 {
			check.Message += " " + strings.Join(parts, "; ") + "."
		}
		check.Action = "Run `clawbridge skill install` so agents get this version's workflow."
		add(check)
		return
	}
	check.Message = strings.Join(parts, "; ") + "."
	if check.Status == Warn {
		check.Action = "Run `clawbridge skill install` to install this binary's skill."
		if force {
			check.Action = "Review the changed files, then run `clawbridge skill install --force` to replace them."
		}
	}
	add(check)
}

type capabilityFlag struct {
	key string
	on  bool
}

// capabilityFlags lists every boolean capability by its JSON key, in
// declaration order, so a new capability shows up without a doctor change.
func capabilityFlags(caps linearprovider.Capabilities) []capabilityFlag {
	v := reflect.ValueOf(caps)
	flags := []capabilityFlag{}
	for n := 0; n < v.NumField(); n++ {
		field := v.Type().Field(n)
		key, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if field.Type.Kind() == reflect.Bool && key != "" && key != "-" {
			flags = append(flags, capabilityFlag{key, v.Field(n).Bool()})
		}
	}
	return flags
}
func checkConfig(config Config, add func(Check)) bool {
	check := Check{ID: "config.trusted", Title: "Trusted configuration", Status: Fail, Path: config.Path}
	if !config.Present {
		check.Message = "No trusted ClawBridge configuration is present."
		check.Action = "Create local or cloud setup routing first; doctor will not invent a default project or authority."
		add(check)
		return false
	}
	if !config.Valid {
		check.Message = "Trusted ClawBridge configuration could not be parsed."
		check.Action = "Correct the configuration file; doctor will not rewrite it."
		add(check)
		return false
	}
	if config.Path == "" {
		check.Message = "Trusted configuration has no explicit source path."
		check.Action = "Select the exact home, explicit or repository configuration source."
		add(check)
		return false
	}
	switch config.Mode {
	case "local":
		if config.DB == "" || config.Workspace == "" {
			check.Message = "Local routing is incomplete: explicit database and workspace are required."
			check.Action = "Run local setup or select the missing trusted route."
			add(check)
			return false
		}
	case "cloud":
		if _, err := remote.ValidateRoute(remote.Config{
			URL: config.Server, Audience: config.Audience, Account: config.Account,
			ImpersonateServiceAccount: config.ImpersonateServiceAccount,
			Principal:                 config.Principal, CachePath: config.Cache,
			CredentialSource: config.CredentialSource, CredentialsFile: config.CredentialsFile,
			OAuthClientID: config.OAuthClientID, OAuthDeviceClientID: config.OAuthDeviceClientID,
			OAuthKeyringService: config.OAuthKeyringService,
			DeviceID:            config.DeviceID, KeyStore: config.KeyStore,
		}); err != nil {
			check.Message = "Cloud credential routing is incomplete or inconsistent."
			check.Action = "Rerun authenticated cloud join or select one complete explicit oauth, gcloud, ADC or device route."
			add(check)
			return false
		}
	default:
		check.Message = "Trusted storage mode is absent or unsupported."
		check.Action = "Select explicit local or cloud mode."
		add(check)
		return false
	}
	check.Status = Pass
	check.Message = "Trusted configuration is present, parseable and complete for " + config.Mode + " mode."
	add(check)
	return true
}

func checkAuthority(ctx context.Context, config Config, executor Executor, add func(Check)) (bool, bool) {
	check := Check{ID: "authority.health", Title: "Authority health", Status: Fail, Path: config.Path}
	if config.Mode != "local" && config.Mode != "cloud" {
		check.Message = "Authority cannot be checked without a supported storage mode."
		add(check)
		return false, false
	}
	if executor == nil {
		check.Message = "No authority executor is configured."
		check.Action = "Select the configured local store or authenticated cloud client."
		add(check)
		return false, false
	}
	if config.Workspace == "" {
		check.Message = "The configured workspace is absent."
		check.Action = "Select an explicit workspace in trusted configuration."
		add(check)
		return false, false
	}
	result, err := executor.Execute(ctx, bus.Request{
		Operation: "list_projects",
		Workspace: config.Workspace,
		Args:      map[string]any{},
	})
	if err != nil {
		check.Message = "The configured authority did not return its visible projects (" + safeCode(err) + ")."
		check.Action = "Verify the selected authority, network, login and workspace; no repair or fallback was attempted."
		add(check)
		return false, false
	}
	expectedSource := "local-authority"
	if config.Mode == "cloud" {
		expectedSource = "server"
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		check.Message = "The authority returned an unexpected result type or nonauthoritative source."
		check.Action = "Verify config mode and server address; offline mirrors cannot establish authority health."
		add(check)
		return false, false
	}
	var count int
	switch projects := data["projects"].(type) {
	case []any:
		count = len(projects)
	case []map[string]any:
		count = len(projects)
	default:
		check.Message = "The authority project list has an unexpected type."
		check.Action = "Verify the selected authority and binary versions; no repair was attempted."
		add(check)
		return false, false
	}
	if result.Source != expectedSource {
		check.Message = "The authority returned an unexpected result type or nonauthoritative source."
		check.Action = "Verify config mode and server address; offline mirrors cannot establish authority health."
		add(check)
		return false, false
	}
	check.Status = Pass
	check.Message = fmt.Sprintf("The %s authority is reachable and returned %d visible project(s).", config.Mode, count)
	add(check)
	return true, true
}

func checkCredentials(ctx context.Context, in Input, configOK bool, add func(Check)) {
	if in.Config.Mode == "cloud" && in.Config.CredentialSource == "device" {
		checkDevice(in, add)
		return
	}
	check := Check{ID: "auth.token-selection", Title: "Google token selection", Status: Fail}
	if in.Config.Mode == "local" {
		check.Status = Pass
		check.Message = "Local mode does not use Google credentials."
		add(check)
		return
	}
	if in.Config.Mode != "cloud" || !configOK {
		check.Message = "Google token routing cannot be selected from incomplete cloud configuration."
		check.Action = "Complete trusted cloud routing before authenticating."
		add(check)
		return
	}
	if in.TokenSource == nil {
		check.Message = "No explicit Google token source is configured."
		check.Action = "Select the initiating account, audience and impersonation service account."
		add(check)
		return
	}
	token, err := in.TokenSource.Token(ctx)
	if err != nil {
		check.Message = "The selected Google credential route could not mint an audience-bound token."
		check.Action = "Reauthenticate the explicit account and verify token-creation IAM; credentials were not printed."
		add(check)
		return
	}
	if token == "" || len(token) > 16384 || strings.ContainsAny(token, " \r\n\t") {
		check.Message = "The credential source returned invalid token material."
		check.Action = "Reauthenticate the explicit account; credentials were not printed."
		add(check)
		return
	}
	check.Status = Pass
	check.Message = "The explicit " + in.Config.CredentialSource + " credential route produced a token."
	add(check)
}

// checkDevice reports a device route's key and its clock correction (§11).
// Whether the server still takes the device shows in authority.health.
func checkDevice(in Input, add func(Check)) {
	check := Check{ID: "auth.device", Title: "Device key", Status: Fail}
	store := in.Config.KeyStore
	switch {
	case in.Device == nil:
		check.Message = "The device key was not checked."
		check.Action = "Join with an invite, or enrol this device."
	case in.Device.Err != nil:
		check.Message = "The device key " + in.Config.DeviceID + " (" + store + ") can't be used: " + in.Device.Err.Error()
		check.Action = "Unlock the key store or fix the key file's mode; if the key is gone, ask an owner for an invite."
	default:
		check.Status = Pass
		check.Message = "The device key " + in.Config.DeviceID + " is readable from its " + store + " store."
		if store == "file" {
			check.Message += " It is a plain file, 0600: anyone who can read it as you can act as you."
		}
	}
	add(check)
	clock := Check{ID: "auth.clock", Title: "Clock offset", Status: Pass, Message: "No clock correction is in use."}
	if in.Device != nil && !in.Device.OffsetAt.IsZero() {
		clock.Message = fmt.Sprintf("Requests are corrected by %s to the server's clock, measured %s; it lapses after 24 h.", in.Device.ClockOffset, in.Device.OffsetAt.UTC().Format(time.RFC3339))
		if in.Device.ClockOffset > 2*time.Minute || in.Device.ClockOffset < -2*time.Minute {
			clock.Status = Warn
			clock.Action = "Fix this machine's clock (enable network time)."
		}
	}
	add(clock)
}

func checkResolution(ctx context.Context, config Config, remote string, executor Executor, authorityOK, projectsOK bool, add func(Check)) (string, string, bool) {
	check := Check{ID: "project.resolution", Title: "Project resolution", Status: Fail}
	if !authorityOK || !projectsOK || executor == nil {
		check.Message = "Project resolution was not attempted because the authority check failed."
		check.Action = "Restore authority health first."
		add(check)
		return "", "", false
	}
	if config.Project == "" && remote == "" {
		check.Message = "No explicit project or registered git remote is selected."
		check.Action = "Select a project or configure a known repository remote; doctor will not infer a slug."
		add(check)
		return "", "", false
	}
	wid, pid, err := executor.ResolveProject(ctx, config.Workspace, config.Project, remote)
	if err != nil {
		check.Message = "The selected project did not resolve uniquely and authoritatively (" + safeCode(err) + ")."
		check.Action = "Select an existing visible project or register the repository remote in that scope."
		add(check)
		return "", "", false
	}
	if wid == "" || pid == "" {
		check.Message = "Project resolution returned incomplete immutable identity."
		check.Action = "Verify authority health; no project was created."
		add(check)
		return "", "", false
	}
	check.Status = Pass
	check.Message = "The configured project resolved to immutable workspace and project identifiers."
	add(check)
	return wid, pid, true
}

// bindingRevision finds the stored revision a binding_unreadable refusal names.
var bindingRevision = regexp.MustCompile(`\(revision (\d+)\)`)

// checkTaskboardBinding reports a project task board binding the authority
// cannot read. Such a binding is refused for every use rather than guessed at,
// so the board is down until a project owner repairs it; doctor names the
// refusal's repair path. It reads only.
func checkTaskboardBinding(ctx context.Context, executor Executor, wid, pid string, add func(Check)) {
	check := Check{ID: "linear.binding", Title: "Task board binding"}
	_, err := executor.Execute(ctx, bus.Request{Operation: "taskboard_get_v2", Workspace: wid, Project: pid, Args: map[string]any{}})
	unreadable, reason := false, ""
	var coded interface{ Code() string }
	var rejected *remote.HTTPError
	switch {
	case errors.As(err, &rejected):
		unreadable, reason = rejected.Code == "binding_unreadable", rejected.Message
	case errors.As(err, &coded):
		unreadable, reason = coded.Code() == "binding_unreadable", err.Error()
	}
	switch {
	case err == nil:
		check.Status = Pass
		check.Message = "The project's task board binding is readable."
	case unreadable:
		revision := "N"
		if m := bindingRevision.FindStringSubmatch(reason); m != nil {
			revision = m[1]
		}
		check.Status = Fail
		check.Message = "The project's task board binding is unreadable; every board call is refused and nothing is guessed from it."
		check.Action = "If ClawBridge was recently downgraded, upgrade instead: the binding may be a newer version's. Otherwise a project owner clears it with `clawbridge linear unlink --expected-revision " + revision + " --confirm --repair`, then links the project again."
	default:
		check.Status = Warn
		check.Message = "The task board binding could not be read (" + safeCode(err) + ")."
		check.Action = "Check project access; `clawbridge linear binding` shows the binding."
	}
	add(check)
}

// checkTeamsWithoutProject reports the teams a binding lists without a
// project (FR10 bootstrap). An inert one is as linked; a stale one is
// harmless, and the next link drops it. A binding that lists none adds
// no check.
func checkTeamsWithoutProject(ctx context.Context, read func(context.Context, string, string) ([]TeamWithoutProject, error), wid, pid string, add func(Check)) {
	teams, err := read(ctx, wid, pid)
	if err == nil && len(teams) == 0 {
		return
	}
	check := Check{ID: "linear.teams_without_project", Title: "Teams without a project"}
	if err != nil {
		check.Status = Warn
		check.Message = "The teams listed without a project could not be read (" + safeCode(err) + ")."
		check.Action = "`clawbridge linear binding` shows the binding; `clawbridge linear issues` shows the refusal."
		add(check)
		return
	}
	labels, stale := []string{}, false
	for _, t := range teams {
		labels = append(labels, t.Label)
		stale = stale || !t.Inert
	}
	check.Status = Pass
	check.Message = strings.Join(labels, "; ") + "."
	if stale {
		check.Action = "Nothing is wrong: a team a bound project carries works as an ordinary team. The next `clawbridge linear link` drops its entry."
	}
	add(check)
}

// checkCommentWatch reports the project's comment watcher and claim
// markers. Neither ever fails doctor: the bus claim is authoritative, and a
// marker or notice only lags.
func checkCommentWatch(ctx context.Context, executor Executor, wid, pid string, add func(Check)) {
	watch := Check{ID: "linear.watch", Title: "Linear comment watch"}
	markers := Check{ID: "linear.markers", Title: "Linear claim markers"}
	result, err := executor.Execute(ctx, bus.Request{Operation: "comment_watch_status", Workspace: wid, Project: pid, Args: map[string]any{}})
	status, _ := result.Data.(map[string]any)
	if err != nil || status == nil {
		watch.Status, markers.Status = Warn, Warn
		watch.Message = "The comment watch status could not be read (" + safeCode(err) + ")."
		watch.Action = "A server older than this client does not have it: upgrade the server first."
		markers.Message = "The claim marker status could not be read."
		add(watch)
		add(markers)
		return
	}
	n := func(key string) int {
		switch v := status[key].(type) {
		case int:
			return v
		case float64:
			return int(v)
		case interface{ Int64() (int64, error) }:
			i, _ := v.Int64()
			return int(i)
		}
		return 0
	}
	live, _ := status["watcher_live"].(bool)
	watch.Status = Pass
	watch.Message = fmt.Sprintf("%d assigned issues are watched for new Linear comments.", n("watched_issues"))
	switch {
	case n("watched_issues") > 0 && !live:
		watch.Status = Warn
		watch.Message = fmt.Sprintf("%d assigned issues are watched, but no comment watcher is running: no one is told of new comments.", n("watched_issues"))
		watch.Action = "Run `clawbridge linear watch` on one host of this project, normally the coordinator's."
	case n("no_recipient") > 0:
		watch.Status = Warn
		watch.Message = fmt.Sprintf("%d watched issues have new comments but no active assignee or coordinator session to notify; they are held, not dropped.", n("no_recipient"))
		watch.Action = "Designate an active coordinator session with `clawbridge project coordinator-set SESSION`."
	case n("held_issues") > 0:
		watch.Status = Warn
		watch.Message = fmt.Sprintf("%d watched issues could not be polled last time; their comments are held and retried.", n("held_issues"))
	}
	switch {
	case status["budget_source"] == "project":
		watch.Message += fmt.Sprintf(" This hour %d of the project's own budget of %d Linear calls are used.", n("budget_used"), n("watch_budget"))
	case status["budget_limit"] != nil:
		watch.Message += fmt.Sprintf(" This hour %d of %d Linear calls are used.", n("budget_used"), n("budget_limit"))
	}
	markers.Status = Pass
	markers.Message = "Claim markers are applied. Session names are shown in Linear claim comments: name sessions by role, never with personal data."
	problems := []string{}
	if k := n("markers_pending"); k > 0 {
		oldest, _ := status["markers_oldest_pending"].(string)
		problems = append(problems, fmt.Sprintf("%d markers pending for over 10 minutes (oldest from %s)", k, oldest))
	}
	if k := n("markers_no_label"); k > 0 {
		problems = append(problems, fmt.Sprintf("%d markers cannot apply the agent-working label: it is missing or ambiguous in the issue's team", k))
	}
	if k := n("markers_out_of_binding"); k > 0 {
		problems = append(problems, fmt.Sprintf("%d markers stopped because their issue left the task board", k))
	}
	if k := n("close_label_drift"); k > 0 {
		problems = append(problems, fmt.Sprintf("%d released issues may still show agent-working on Linear", k))
	}
	if len(problems) > 0 {
		markers.Status = Warn
		markers.Message = strings.Join(problems, "; ") + "."
		markers.Action = "Run `clawbridge linear watch` with a write-access Linear profile; a Linear admin creates exactly one agent-working label per team (or one workspace label)."
	}
	add(watch)
	add(markers)
}

// checkLinearApp shows who makes a hosted project's agent writes in Linear:
// the ClawBridge app, with every bound team it must see, or each member's
// own profile when the server has no app. It reads only.
func checkLinearApp(ctx context.Context, executor Executor, wid, pid string, add func(Check)) {
	check := Check{ID: "linear.app", Title: "ClawBridge Linear app", Status: Pass}
	r, err := executor.Execute(ctx, bus.Request{Operation: "linear_app_status", Workspace: wid, Project: pid, Args: map[string]any{}})
	m, _ := r.Data.(map[string]any)
	var refused *remote.HTTPError
	switch {
	case errors.As(err, &refused) && refused.Status == 400:
		check.Message = "This server has no ClawBridge Linear app: agent writes use each member's own Linear profile."
	case err != nil || m == nil || m["configured"] != true:
		code := "invalid"
		if err != nil {
			code = safeCode(err)
		}
		check.Status = Warn
		check.Message = "The server's Linear app status could not be read (" + code + "), so agent writes are refused until it can."
		check.Action = "Retry; if it persists, the server operator checks the app's settings and its startup log."
	case m["binding"] != nil:
		check.Status = Warn
		check.Message = fmt.Sprintf("Agent writes go to Linear as the ClawBridge app, but the task-board binding could not be read: %v", m["binding"])
		check.Action = "Run `clawbridge linear binding`; a binding the app writes to must be pinned with `clawbridge linear link`."
	default:
		teams, _ := m["teams_visible"].(map[string]any)
		hidden := []string{}
		for team, seen := range teams {
			if seen != true {
				hidden = append(hidden, team)
			}
		}
		sort.Strings(hidden)
		check.Message = fmt.Sprintf("Agent writes go to Linear as the ClawBridge app (Linear user %v), and it sees all %d bound teams.", m["app_user_id"], len(teams))
		if len(hidden) > 0 {
			check.Status = Fail
			check.Message = fmt.Sprintf("Agent writes go to Linear as the ClawBridge app, but it cannot see bound teams %s, so writes there fail.", strings.Join(hidden, ", "))
			check.Action = "A Linear admin adds the ClawBridge app to each of those teams (private teams need it as a member)."
		}
		// Intake credits a trusted person's write through the app only by
		// the server's record of it; a write it failed to record leaves
		// that window to whatever else was recorded.
		if n, _ := m["attribution_record_failures"].(float64); n > 0 && check.Status == Pass {
			check.Status = Warn
			check.Message += fmt.Sprintf(" The server failed to record who asked for %d of its app writes since it started, so intake may have missed or misjudged whose label or mention those were.", int64(n))
			check.Action = "The server operator checks the server log for \"intake attribution was not recorded\" and the database's health."
		}
	}
	add(check)
	if err == nil && m != nil && m["outbox"] != nil {
		checkLinearOutbox(m, add)
	}
	if err == nil && m != nil && m["server_instances"] != nil {
		checkServerInstances(m, add)
	}
}

// checkServerInstances says how many servers share the database: each
// paces Linear on its own, so more than one may together pass Linear's
// limits and a project's budget (LINEAR-QUEUE-DESIGN §3).
func checkServerInstances(status map[string]any, add func(Check)) {
	check := Check{ID: "linear.instances", Title: "Servers pacing Linear", Status: Pass}
	var n bus.ServerInstances
	raw, _ := json.Marshal(status["server_instances"])
	json.Unmarshal(raw, &n)
	switch {
	case n.Pooler:
		check.Message = "The number of servers can't be told: a connection pooler (PgBouncer) sits in front of the database."
		check.Action = "Run one server instance, or see docs/operations/deployment.md, \"More than one server\"."
	case !n.Known:
		check.Message = "The number of servers can't be told from the database's connections."
	case n.Live > 1:
		check.Status = Warn
		check.Message = fmt.Sprintf("%d servers share this database. Each paces its Linear calls on its own, so together they can pass Linear's rate limits and a project's hourly budget.", n.Live)
		check.Action = "Run one server instance; see docs/operations/deployment.md, \"More than one server\"."
	default:
		check.Message = "One server paces this database's Linear calls."
	}
	add(check)
}

// Queue-age thresholds for linear.outbox (LINEAR-QUEUE-DESIGN §8).
const (
	outboxWarnAge = 15 * time.Minute
	outboxFailAge = 60 * time.Minute
)

// checkLinearOutbox reports a hosted project's queued Linear writes from
// the app status: how many, the oldest's age and what most wait for. It
// warns when the oldest waited 15 minutes and fails at an hour, or when a
// write died or expired in the last day. Counts and times only.
func checkLinearOutbox(status map[string]any, add func(Check)) {
	check := Check{ID: "linear.outbox", Title: "Queued Linear writes", Status: Pass}
	var st bus.OutboxStatus
	var budget bus.LinearBudget
	raw, _ := json.Marshal(status["outbox"])
	json.Unmarshal(raw, &st)
	raw, _ = json.Marshal(status["budget"])
	json.Unmarshal(raw, &budget)
	used := ""
	if budget.Limit > 0 {
		used = fmt.Sprintf(" This hour %d of the project's %d Linear calls are used.", budget.Used, budget.Limit)
	}
	oldest := time.Duration(st.OldestQueued) * time.Second
	check.Message = "No Linear writes are queued." + used
	switch {
	case st.Queued == 0 && st.Sending > 0:
		check.Message = fmt.Sprintf("%d Linear writes are being sent now.", st.Sending) + used
	case st.Queued > 0:
		check.Message = fmt.Sprintf("%d Linear writes are queued, the oldest for %s, most %s.", st.Queued+st.Sending, waitedFor(oldest), waitingFor(st.Waiting)) + used
	}
	switch {
	case oldest >= outboxFailAge:
		check.Status = Fail
	case oldest >= outboxWarnAge:
		check.Status = Warn
	}
	if check.Status != Pass {
		switch st.Waiting {
		case "project_hourly_cap":
			check.Action = "They go when the hour resets"
			if !budget.ResetsAt.IsZero() {
				check.Action += " (" + budget.ResetsAt.UTC().Format("15:04") + " UTC)"
			}
			check.Action += "; or the project owner raises the budget with `clawbridge project linear-budget set N`."
		case "linear_rate_limit":
			check.Action = "Linear's rate limit holds them and they go when it allows; if it lasts, look for another client spending the app's limit."
		default:
			check.Action = "They wait behind earlier writes on the same issue; `clawbridge linear write-status` lists them."
		}
	}
	if lost := st.Dead24h + st.Expired24h; lost > 0 {
		check.Status = Fail
		check.Message += fmt.Sprintf(" %d writes were given up in the last 24 hours (%d failed every try, %d waited past 3 hours); nothing was sent for them.", lost, st.Dead24h, st.Expired24h)
		check.Action = "Each session was told; `clawbridge linear write-status` lists them. Redo the writes still wanted."
	}
	if st.Uncertain24h > 0 {
		if check.Status == Pass {
			check.Status = Warn
		}
		check.Message += fmt.Sprintf(" %d writes in the last 24 hours may or may not have reached Linear.", st.Uncertain24h)
		if check.Action == "" {
			check.Action = "Check those issues in Linear before writing again; `clawbridge linear write-status` lists them."
		}
	}
	add(check)
}

// waitedFor is a queue age in whole minutes.
func waitedFor(d time.Duration) string {
	if m := int(d / time.Minute); m != 1 {
		return fmt.Sprintf("%d minutes", m)
	}
	return "1 minute"
}

// waitingFor is what most queued writes wait for, in words.
func waitingFor(reason string) string {
	switch reason {
	case "project_hourly_cap":
		return "for the project's hourly Linear budget"
	case "linear_rate_limit":
		return "for Linear's rate limit"
	case "ordered_behind":
		return "behind earlier writes on the same issue"
	}
	return "being sent"
}

// checkLabelPolicy shows the project owner's approved label list: its
// revision and who set it when. It is an audit gate set by a person, not a
// boundary against the owner's own sessions. It reads only.
func checkLabelPolicy(ctx context.Context, executor Executor, wid, pid string, add func(Check)) {
	check := Check{ID: "linear.label_policy", Title: "Approved Linear labels", Status: Pass}
	r, err := executor.Execute(ctx, bus.Request{Operation: "label_policy_get", Workspace: wid, Project: pid, Args: map[string]any{}})
	m, _ := r.Data.(map[string]any)
	switch {
	case err != nil || m == nil:
		check.Status = Warn
		check.Message = "The approved label list could not be read (" + safeCode(err) + ")."
		check.Action = "Check project access; `clawbridge linear label-policy show` shows the list."
	case fmt.Sprint(m["revision"]) == "0":
		check.Message = "No approved label list is set, so `clawbridge linear label create` is refused."
		check.Action = "The project owner sets one with `clawbridge linear label-policy set --file FILE --expected-revision 0` when labels are needed."
	default:
		// A local authority returns typed values; a remote one, decoded JSON.
		size := func(v any) int {
			if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map {
				return rv.Len()
			}
			return 0
		}
		check.Message = fmt.Sprintf("Approved label list revision %v, set by %v at %v: %d labels, %d renames.", m["revision"], m["updated_by"], m["updated_at"], size(m["labels"]), size(m["renames"]))
	}
	add(check)
}

// checkIssueSubscribers says who hears of the issues agents create or claim
// as the ClawBridge app (live/ISSUE-SUBSCRIBERS-PLAN.md): the hint for a
// board linked before the setting existed, and Linear refusing the owner.
// It reads only, and never shows an email (B4).
func checkIssueSubscribers(ctx context.Context, executor Executor, wid, pid string, add func(Check)) {
	check := Check{ID: "linear.issue_subscribers", Title: "Who hears of agents' issues", Status: Pass}
	r, err := executor.Execute(ctx, bus.Request{Operation: "issue_subscribers_get", Workspace: wid, Project: pid, Args: map[string]any{}})
	m, _ := r.Data.(map[string]any)
	rows, _ := m["rows"].(map[string]any)
	failed := 0
	if rows != nil {
		failed, _ = strconv.Atoi(fmt.Sprint(rows["failed"]))
	}
	switch {
	case err != nil || m == nil:
		// A server older than the setting has nothing to say.
		check.Message = "This server doesn't subscribe anyone to agents' issues."
	case m["proven"] != true:
		check.Message = "This server doesn't subscribe anyone to agents' issues yet; its operator turns it on after a live check."
	case m["enabled"] != true:
		check.Status = Warn
		check.Message = "Issues agents create or claim notify nobody: their Linear author is the ClawBridge app."
		check.Action = "The project owner or a lead turns it on with `clawbridge linear subscribers on`."
	case m["owner_not_in_linear_until"] != nil:
		check.Status = Warn
		check.Message = "Linear knows no user by the project owner's email, so the owner isn't subscribed to agents' issues."
		check.Action = "Add the owner's Linear user with `clawbridge linear subscribers add LINEAR_USER_UUID`."
	case failed > 0:
		check.Status = Warn
		check.Message = fmt.Sprintf("%d agent issues couldn't be subscribed after several tries.", failed)
		check.Action = "`clawbridge linear subscribers show` lists the counts; the ClawBridge Linear app's status may say why."
	default:
		check.Message = "Agents' issues notify the project's people."
	}
	add(check)
}

// checkLinearWrites warns of a recorded Linear write of any kind (a
// project create, an issue move, a team added to a project) that may have
// happened unconfirmed (FR9): reserved more than ten minutes ago and never
// settled, or settled uncertain, and not acknowledged. It reads the
// overview, which never sends the notice. The check was
// linear.project_creates until v0.3.9; that ID stays an alias for one
// release.
func checkLinearWrites(ctx context.Context, executor Executor, wid, pid string, add func(Check)) {
	check := Check{ID: "linear.writes", Title: "Linear writes", Status: Pass, Aliases: []string{"linear.project_creates"}}
	r, err := executor.Execute(ctx, bus.Request{Operation: "project_overview", Workspace: wid, Project: pid, Args: map[string]any{}})
	m, _ := r.Data.(map[string]any)
	writes, _ := m["linear_writes"].(map[string]any)
	if writes == nil {
		// A server before v0.3.9 lists them under project_creates.
		writes, _ = m["project_creates"].(map[string]any)
	}
	if err != nil || writes == nil {
		// An older server keeps no record of them.
		return
	}
	// Every kind of Linear write the server records, each named by its
	// kind (an older server's rows have none: a project create).
	var ops []string
	named := func(row map[string]any) string {
		write := fmt.Sprint(row["write"])
		if row["write"] == nil {
			write = "project_create"
		}
		named := fmt.Sprintf("%s %v", write, row["operation_id"])
		if row["state"] == "uncertain" {
			named += " (uncertain)"
		}
		return named
	}
	switch rows := writes["unconfirmed"].(type) {
	case []map[string]any:
		for _, row := range rows {
			ops = append(ops, named(row))
		}
	case []any:
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			ops = append(ops, named(row))
		}
	}
	if len(ops) == 0 {
		check.Message = "No Linear write is unconfirmed."
		add(check)
		return
	}
	check.Status = Warn
	check.Message = fmt.Sprintf("%d Linear write(s) may have happened, unconfirmed: %s.", len(ops), strings.Join(ops, ", "))
	check.Action = "Check each in Linear (the team's projects for a create, the issue for a move, the project for a team add), then the project owner runs `clawbridge linear writes --ack OPERATION_ID` for each."
	add(check)
}

func checkMirror(ctx context.Context, in Input, wid, pid string, resolved bool, add func(Check)) {
	check := Check{ID: "mirror.freshness", Title: "Offline mirror freshness", Status: Warn, Path: in.Config.Cache}
	if in.Config.Mode == "local" {
		check.Status = Pass
		check.Message = "Local authority mode does not use a cloud offline mirror."
		add(check)
		return
	}
	if in.Config.Cache == "" {
		check.Message = "Cloud routing has no offline mirror path."
		check.Action = "Select a trusted cache path; writes will still never be queued."
		add(check)
		return
	}
	if !resolved {
		check.Message = "Mirror freshness cannot be bound because project resolution failed."
		check.Action = "Restore unique project resolution and sync an authorized snapshot."
		add(check)
		return
	}
	snapshot, err := mirror.Load(ctx, in.Config.Cache, mirror.Binding{
		Server: in.Config.Server, Audience: remote.MirrorAudience(in.Config.Audience, in.Config.CredentialSource, in.Config.DeviceID), Principal: in.Config.Principal,
		Workspace: wid, Project: pid, Session: in.Config.Session, Instance: in.Config.Instance,
	})
	if err != nil {
		check.Message = "No complete mirror matches this trusted authority, principal and scope."
		check.Action = "Run an authorized sync while online; doctor did not create or modify the cache."
		add(check)
		return
	}
	updated, err := time.Parse(time.RFC3339Nano, snapshot.UpdatedAt)
	if err != nil {
		check.Message = "The mirror has an invalid snapshot timestamp."
		check.Action = "Replace it with a successful online sync; existing cache bytes were not changed."
		add(check)
		return
	}
	age := in.Now.Sub(updated)
	if age < 0 {
		age = 0
	}
	check.Message = "The matching mirror is " + age.Round(time.Second).String() + " old."
	if age > in.MirrorStaleAfter {
		check.Status = Warn
		check.Message += " It exceeds the " + in.MirrorStaleAfter.String() + " freshness target."
		check.Action = "Run an authorized sync when online; retain read-only and instance-bound semantics."
		add(check)
		return
	}
	check.Status = Pass
	add(check)
}

func checkMCP(mcp MCPConfig, add func(Check)) {
	id := "mcp." + mcp.Agent + "." + mcp.Name
	check := Check{ID: id, Title: "Installed MCP entry", Status: Fail, Path: mcp.Path}
	if mcp.Agent != "codex" && mcp.Agent != "claude-code" {
		check.Message = "The selected MCP agent kind is unsupported."
		check.Action = "Inspect Codex or Claude Code entries only."
		add(check)
		return
	}
	if mcp.Name == "" || mcp.Path == "" || !filepath.IsAbs(mcp.Path) {
		check.Message = "The MCP inspection requires an entry name and absolute exact path."
		check.Action = "Select the installed configuration path explicitly."
		add(check)
		return
	}
	info, err := os.Lstat(mcp.Path)
	if errors.Is(err, os.ErrNotExist) {
		check.Status = Warn
		check.Message = "No MCP configuration file exists at the selected path."
		check.Action = "Install an explicitly scoped MCP entry if this client should use ClawBridge."
		add(check)
		return
	}
	if err != nil || !info.Mode().IsRegular() {
		check.Message = "The MCP configuration cannot be read as a regular file."
		check.Action = "Select the exact regular client configuration path; no file was changed."
		add(check)
		return
	}
	if info.Size() > 4<<20 {
		check.Message = "The MCP configuration exceeds the 4 MiB inspection limit."
		check.Action = "Select the intended client configuration; no file was changed."
		add(check)
		return
	}
	raw, err := os.ReadFile(mcp.Path)
	if err != nil {
		check.Message = "The MCP configuration could not be read."
		check.Action = "Correct file permissions; no file was changed."
		add(check)
		return
	}
	entry, found, err := parseMCPEntry(mcp.Agent, mcp.Name, raw)
	if err != nil {
		check.Message = "The MCP configuration is malformed or has an unsupported server shape."
		check.Action = "Correct the configuration explicitly; doctor will not rewrite or repair it."
		add(check)
		return
	}
	if !found {
		check.Status = Warn
		check.Message = "The expected ClawBridge MCP entry is absent from this client configuration."
		check.Action = "Install the explicitly scoped entry when this client should use ClawBridge."
		add(check)
		return
	}
	expected, err := expectedMCPEntry(mcp)
	if err != nil {
		check.Message = "The expected MCP scope or credential route is incomplete."
		check.Action = "Select the complete trusted route before inspecting installation."
		add(check)
		return
	}
	if !entryMatches(entry, expected) {
		check.Message = "The installed MCP entry conflicts with the expected project, session, mode or credential route."
		check.Action = "Inspect the entry and reinstall with the intended exact scope and a distinct entry name if needed; doctor made no change."
		add(check)
		return
	}
	command, _ := entry["command"].(string)
	if !filepath.IsAbs(command) {
		check.Message = "The installed MCP command is not an absolute executable path."
		check.Action = "Reinstall with the intended absolute ClawBridge binary path; doctor made no change."
		add(check)
		return
	}
	if info, err := os.Stat(command); err != nil || info.IsDir() {
		check.Status = Warn
		check.Message = "The installed MCP command does not currently resolve to an executable file."
		check.Action = "Restore or reinstall the intended ClawBridge binary at that path."
		add(check)
		return
	}
	check.Status = Pass
	check.Message = "The installed MCP entry matches the expected authority, scope and credential route."
	add(check)
}

func parseMCPEntry(agent, name string, raw []byte) (map[string]any, bool, error) {
	root := map[string]any{}
	key := "mcpServers"
	if agent == "codex" {
		key = "mcp_servers"
		if len(bytes.TrimSpace(raw)) == 0 {
			return nil, false, nil
		}
		if toml.Unmarshal(raw, &root) != nil {
			return nil, false, errors.New("invalid TOML")
		}
	} else {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&root) != nil || root == nil {
			return nil, false, errors.New("invalid JSON object")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return nil, false, errors.New("trailing JSON")
		}
	}
	rawServers, exists := root[key]
	if !exists {
		return nil, false, nil
	}
	servers, ok := rawServers.(map[string]any)
	if !ok {
		return nil, false, errors.New("server setting is not an object")
	}
	rawEntry, exists := servers[name]
	if !exists {
		return nil, false, nil
	}
	entry, ok := rawEntry.(map[string]any)
	if !ok {
		return nil, false, errors.New("entry is not an object")
	}
	return entry, true, nil
}

func expectedMCPEntry(mcp MCPConfig) (map[string]any, error) {
	if mcp.LinearProfile != "" && !regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString(mcp.LinearProfile) {
		return nil, errors.New("invalid personal Linear profile ID")
	}
	if mcp.Workspace == "" || mcp.Project == "" || mcp.Session == "" {
		return nil, errors.New("scope required")
	}
	mode := mcp.Mode
	if mode == "" {
		mode = "local"
		if mcp.Server != "" {
			mode = "cloud"
		}
	}
	args := []any{"mcp", "--mode", mode, "--workspace", mcp.Workspace, "--project", mcp.Project, "--session", mcp.Session}
	switch mode {
	case "local":
		if mcp.DB == "" {
			return nil, errors.New("database required")
		}
		args = append(args, "--db", mcp.DB)
	case "cloud":
		if !filepath.IsAbs(mcp.Cache) {
			return nil, errors.New("absolute cache path required")
		}
		route, err := remote.ValidateRoute(remote.Config{
			URL: mcp.Server, Audience: mcp.Audience, Account: mcp.Account,
			ImpersonateServiceAccount: mcp.ImpersonateServiceAccount,
			Principal:                 mcp.Principal, CachePath: mcp.Cache,
			CredentialSource: mcp.CredentialSource, CredentialsFile: mcp.CredentialsFile,
			OAuthClientID: mcp.OAuthClientID, OAuthDeviceClientID: mcp.OAuthDeviceClientID,
			OAuthKeyringService: mcp.OAuthKeyringService,
			DeviceID:            mcp.DeviceID, KeyStore: mcp.KeyStore,
		})
		if err != nil {
			return nil, err
		}
		if route.CredentialSource == remote.CredentialSourceDevice {
			args = append(args, "--server", route.URL, "--credential-source", route.CredentialSource)
		} else {
			args = append(args, "--server", route.URL, "--audience", route.Audience, "--credential-source", route.CredentialSource)
		}
		switch route.CredentialSource {
		case remote.CredentialSourceDevice:
			args = append(args, "--device-id", route.DeviceID, "--key-store", route.KeyStore)
		case remote.CredentialSourceGcloud:
			args = append(args, "--account", route.Account, "--impersonate-service-account", route.ImpersonateServiceAccount)
		case remote.CredentialSourceADC:
			args = append(args, "--credentials-file", route.CredentialsFile, "--account", route.Account, "--impersonate-service-account", route.ImpersonateServiceAccount)
		case remote.CredentialSourceOAuth:
			args = append(args, "--oauth-client-id", route.OAuthClientID)
			if route.OAuthDeviceClientID != "" {
				args = append(args, "--oauth-device-client-id", route.OAuthDeviceClientID)
			}
			if route.OAuthKeyringService != "" {
				args = append(args, "--oauth-keyring-service", route.OAuthKeyringService)
			}
		}
		args = append(args, "--principal", route.Principal, "--cache", route.CachePath)
	default:
		return nil, errors.New("mode required")
	}
	if mcp.LinearProfile != "" {
		args = append(args, "--linear-profile", mcp.LinearProfile)
	}
	if mcp.Instance != "" {
		args = append(args, "--instance", mcp.Instance)
	}
	if mcp.ClientConfig != "" {
		if !filepath.IsAbs(mcp.ClientConfig) {
			return nil, errors.New("absolute client config required")
		}
		args = append(args, "--client-config", mcp.ClientConfig)
	}
	return map[string]any{"command": nil, "args": args}, nil
}

func entryMatches(actual, expected map[string]any) bool {
	// Installed entries may add unrelated environment values, but must not
	// carry credentials or override the pinned route through environment.
	if value, exists := actual["env"]; exists {
		env, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for name, value := range env {
			if _, ok := value.(string); !ok || !safeMCPEnvironmentName(name) {
				return false
			}
		}
	}
	if value, exists := actual["env_vars"]; exists {
		names, ok := value.([]any)
		if !ok {
			return false
		}
		for _, value := range names {
			name, ok := value.(string)
			if !ok || !safeMCPEnvironmentName(name) {
				return false
			}
		}
	}
	actualCommand, actualHasCommand := actual["command"].(string)
	expectedCommand, expectedHasCommand := expected["command"].(string)
	if expectedHasCommand && (!actualHasCommand || actualCommand != expectedCommand) {
		return false
	}
	actualArgs, ok := actual["args"].([]any)
	if !ok {
		return false
	}
	expectedArgs, _ := expected["args"].([]any)
	if len(actualArgs) != len(expectedArgs) {
		return false
	}
	for i := range expectedArgs {
		if fmt.Sprint(actualArgs[i]) != fmt.Sprint(expectedArgs[i]) {
			return false
		}
	}
	if _, ok := actual["type"]; ok {
		if actual["type"] != "stdio" {
			return false
		}
	}
	return true
}

func safeMCPEnvironmentName(name string) bool {
	name = strings.ToUpper(name)
	if strings.HasPrefix(name, "CLAWBRIDGE_") || strings.HasPrefix(name, "CLOUDSDK_") {
		return false
	}
	for _, marker := range []string{"SECRET", "TOKEN", "PASSWORD", "CREDENTIAL", "PRIVATE_KEY"} {
		if strings.Contains(name, marker) {
			return false
		}
	}
	return true
}

func safeCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		switch coded.Code() {
		case "forbidden", "not_found", "conflict", "invalid", "rate_limited", "unavailable":
			return coded.Code()
		}
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "permission") || strings.Contains(text, "membership") || strings.Contains(text, "authorized"):
		return "forbidden"
	case strings.Contains(text, "not found") || strings.Contains(text, "ambiguous"):
		return "not_found"
	case strings.Contains(text, "context") || strings.Contains(text, "timeout") || strings.Contains(text, "unavailable") || strings.Contains(text, "connection"):
		return "unavailable"
	}
	return "rejected"
}

func aggregate(checks []Check) string {
	status := Pass
	for _, check := range checks {
		if check.Status == Fail {
			return Fail
		}
		if check.Status == Warn {
			status = Warn
		}
	}
	return status
}

// SortedChecks returns a name-sorted copy for stable external rendering while
// leaving the diagnostic execution order intact.
func SortedChecks(checks []Check) []Check {
	out := append([]Check{}, checks...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// LabelSet is one bound team's label set for a name ClawBridge finds labels
// by (needs-agent, agent-working): where each label of it is listed.
type LabelSet struct {
	Team   string   `json:"team"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	Apply  bool     `json:"apply"`
	Err    string   `json:"error,omitempty"`
}

// checkLabelSets shows each bound team's needs-agent and agent-working
// sets (CROSS-TEAM-BINDING §3.2), read now through the person's profile. A
// team with no needs-agent is never polled for it; one with no agent-working
// to apply gets no marker label.
func checkLabelSets(ctx context.Context, read func(context.Context) ([]LabelSet, error), add func(Check)) {
	check := Check{ID: "linear.labels", Title: "Linear label sets", Status: Pass}
	sets, err := read(ctx)
	if err != nil {
		check.Status = Warn
		check.Message = "The label sets could not be read (" + safeCode(err) + ")."
		check.Action = "Check the personal Linear profile with `clawbridge linear status --profile ID`."
		add(check)
		return
	}
	parts, gaps := []string{}, []string{}
	for _, s := range sets {
		team := s.Team
		if len(team) > 8 {
			team = team[:8]
		}
		switch {
		case s.Err != "":
			gaps = append(gaps, fmt.Sprintf("%s %s: %s", team, s.Name, s.Err))
		case len(s.Scopes) == 0 || s.Name != "needs-agent" && !s.Apply:
			gaps = append(gaps, fmt.Sprintf("%s %s: none", team, s.Name))
		default:
			parts = append(parts, fmt.Sprintf("%s %s: %s", team, s.Name, strings.Join(s.Scopes, "+")))
		}
	}
	check.Message = "Read now; the server's watcher re-reads its copy at least hourly."
	if len(parts) > 0 {
		check.Message = strings.Join(parts, "; ") + ". " + check.Message
	}
	if len(gaps) > 0 {
		check.Status = Warn
		check.Message = strings.Join(gaps, "; ") + ". " + check.Message
		check.Action = "A team with no needs-agent is never polled for it, and one with no agent-working gets no marker label. Create the label in the team or its bound parent, or remove the duplicate a team lists twice."
	}
	add(check)
}

// checkProvenGates names the server's PROVEN gates that are on and those
// that are off: a Linear write behind an off gate is refused as unproven.
// Only the project owner's overview lists them; for anyone else, or an
// older server, the check is left out.
func checkProvenGates(ctx context.Context, executor Executor, wid, pid string, add func(Check)) {
	r, err := executor.Execute(ctx, bus.Request{Operation: "project_overview", Workspace: wid, Project: pid, Args: map[string]any{}})
	m, _ := r.Data.(map[string]any)
	on, off, ok := bus.ProvenGatesOnOff(m["proven_gates"])
	if err != nil || !ok {
		return
	}
	check := Check{ID: "linear.proven_gates", Title: "Proven Linear write gates", Status: Pass}
	switch {
	case len(on) == 0:
		check.Message = fmt.Sprintf("Proven gates: none on (off: %s).", strings.Join(off, ", "))
	case len(off) == 0:
		check.Message = fmt.Sprintf("Proven gates: %s.", strings.Join(on, ", "))
	default:
		check.Message = fmt.Sprintf("Proven gates: %s (off: %s).", strings.Join(on, ", "), strings.Join(off, ", "))
	}
	add(check)
}

// checkProjectTeamAdds counts the teams added to Linear projects (FR10)
// and warns of those Linear answered with other fields changed, naming
// the operation and the fields, until the owner acknowledges each:
// ClawBridge never undoes them, so a person looks. It reads the overview.
func checkProjectTeamAdds(ctx context.Context, executor Executor, wid, pid string, add func(Check)) {
	r, err := executor.Execute(ctx, bus.Request{Operation: "project_overview", Workspace: wid, Project: pid, Args: map[string]any{}})
	m, _ := r.Data.(map[string]any)
	adds, _ := m["project_team_adds"].(map[string]any)
	if err != nil || adds == nil {
		// An older server keeps no record of them.
		return
	}
	count := func(v any) int {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		}
		return 0
	}
	check := Check{ID: "linear.project_team_adds", Title: "Teams added to Linear projects", Status: Pass}
	added, changed := count(adds["added"]), count(adds["with_changes"])
	check.Message = fmt.Sprintf("%d team(s) added to Linear projects; none changed anything else.", added)
	if changed == 0 {
		add(check)
		return
	}
	var named []string
	each := func(row map[string]any) {
		if row["state"] != "accepted_with_changes" || row["acknowledged_at"] != nil {
			return
		}
		var fields []string
		switch list := row["changed"].(type) {
		case []string:
			fields = list
		case []any:
			for _, x := range list {
				fields = append(fields, fmt.Sprint(x))
			}
		}
		named = append(named, fmt.Sprintf("%v (%s)", row["operation_id"], strings.Join(fields, ", ")))
	}
	switch rows := adds["recent"].(type) {
	case []map[string]any:
		for _, row := range rows {
			each(row)
		}
	case []any:
		for _, raw := range rows {
			row, _ := raw.(map[string]any)
			each(row)
		}
	}
	check.Status = Warn
	check.Message = fmt.Sprintf("%d team(s) added to Linear projects; Linear changed other fields with %d: %s.", added, changed, strings.Join(named, ", "))
	check.Action = "Look at each project in Linear (ClawBridge sent only the team and never undoes what Linear changed), then the project owner runs `clawbridge linear writes --ack OPERATION_ID` for each."
	add(check)
}
