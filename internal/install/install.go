// Package install writes scoped stdio MCP entries without invoking an agent.
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"clawbridge/internal/remote"
	"github.com/pelletier/go-toml/v2"
)

type Options struct {
	LinearProfile                                                                string
	ClientConfig                                                                 string
	Agent, Config, Command, Name                                                 string
	DB, Workspace, Project, Session, Instance                                    string
	Mode, Server, Audience, Account, ImpersonateServiceAccount, Principal, Cache string
	CredentialSource, CredentialsFile, OAuthClientID                             string
	OAuthDeviceClientID, OAuthKeyringService                                     string
	DryRun                                                                       bool
}

type Result struct {
	Path    string         `json:"path"`
	Changed bool           `json:"changed"`
	DryRun  bool           `json:"dry_run"`
	Backup  string         `json:"backup,omitempty"`
	Entry   map[string]any `json:"entry"`
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Run is the CLI entry point. Tests should use Install with an isolated path.
func Run(args []string) error { return RunWithIO(args, os.Stdout, os.Stderr) }

// RunWithIO preserves CLI stream ownership for embedded callers.
func RunWithIO(args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("mcp install", flag.ContinueOnError)
	f.SetOutput(stderr)
	o := Options{}
	f.StringVar(&o.LinearProfile, "linear-profile", "", "explicit personal native Linear profile ID (optional)")
	f.StringVar(&o.ClientConfig, "client-config", "", "absolute trusted client config path")
	f.StringVar(&o.Agent, "agent", "", "codex or claude-code")
	f.StringVar(&o.Config, "config", "", "explicit target configuration path")
	f.StringVar(&o.Command, "command", "", "absolute clawbridge binary path")
	f.StringVar(&o.Name, "name", "clawbridge", "MCP entry name; use a distinct name per configured session")
	f.StringVar(&o.DB, "db", "", "local authority database path")
	f.StringVar(&o.Workspace, "workspace", "", "workspace name or ID (required)")
	f.StringVar(&o.Project, "project", "", "project slug or ID (required)")
	f.StringVar(&o.Session, "session", "", "unique agent session name (required)")
	f.StringVar(&o.Instance, "instance", "", "explicit instance to resume (optional)")
	f.StringVar(&o.Mode, "mode", "", "local or cloud (inferred from --server)")
	f.StringVar(&o.Server, "server", "", "trusted cloud server URL")
	f.StringVar(&o.Audience, "audience", "", "Google ID-token audience")
	f.StringVar(&o.Account, "account", "", "explicit initiating Google account")
	f.StringVar(&o.ImpersonateServiceAccount, "impersonate-service-account", "", "explicit audience-bound service identity")
	f.StringVar(&o.Principal, "principal", "", "expected verified principal for scoped offline reads")
	f.StringVar(&o.Cache, "cache", "", "cloud mirror path")
	f.StringVar(&o.CredentialSource, "credential-source", "", "explicit oauth, gcloud or adc backend")
	f.StringVar(&o.CredentialsFile, "credentials-file", "", "explicit authorized-user ADC file")
	f.StringVar(&o.OAuthClientID, "oauth-client-id", "", "active OAuth Desktop client ID")
	f.StringVar(&o.OAuthDeviceClientID, "oauth-device-client-id", "", "separate device-flow OAuth client ID")
	f.StringVar(&o.OAuthKeyringService, "oauth-keyring-service", "", "OS keychain service name")
	f.BoolVar(&o.DryRun, "dry-run", false, "preview the entry without writing files")
	f.Bool("json", true, "return JSON (the default output format)")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("mcp install accepts flags only")
	}
	r, err := Install(o)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(r)
}

// Install preserves unrelated settings, rejects a conflicting entry, backs up
// existing bytes, and atomically replaces the target. A repeat is a byte-level no-op.
func Install(o Options) (Result, error) {
	r := Result{DryRun: o.DryRun}
	if o.LinearProfile != "" && !regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString(o.LinearProfile) {
		return r, errors.New("invalid explicit Linear profile ID")
	}
	if o.ClientConfig != "" && !filepath.IsAbs(o.ClientConfig) {
		return r, errors.New("client config must be absolute")
	}
	if o.Agent != "codex" && o.Agent != "claude-code" {
		return r, errors.New("--agent must be codex or claude-code")
	}
	if o.Workspace == "" || o.Project == "" || o.Session == "" {
		return r, errors.New("installation requires a workspace, a project and a distinct --session; in local mode, set a default project with clawbridge setup --local --project NAME or clawbridge project create NAME, or pass --workspace and --project")
	}
	if o.Name == "" {
		o.Name = "clawbridge"
	}
	if !namePattern.MatchString(o.Name) {
		return r, errors.New("--name may contain only letters, digits, hyphens and underscores")
	}
	if o.Command == "" {
		var err error
		o.Command, err = os.Executable()
		if err != nil {
			return r, err
		}
	}
	if !filepath.IsAbs(o.Command) {
		return r, errors.New("--command must be an absolute executable path")
	}
	command, err := filepath.Abs(o.Command)
	if err != nil {
		return r, err
	}
	if o.DB == "" {
		home := os.Getenv("CLAWBRIDGE_HOME")
		if home == "" {
			u, e := os.UserHomeDir()
			if e != nil {
				return r, e
			}
			home = filepath.Join(u, ".clawbridge")
		}
		o.DB = filepath.Join(home, "local.db")
	}
	db, err := filepath.Abs(o.DB)
	if err != nil {
		return r, err
	}
	if o.Config == "" {
		if o.Agent == "codex" {
			home := os.Getenv("CODEX_HOME")
			if home == "" {
				u, e := os.UserHomeDir()
				if e != nil {
					return r, e
				}
				home = filepath.Join(u, ".codex")
			}
			o.Config = filepath.Join(home, "config.toml")
		} else {
			o.Config = ".mcp.json"
		}
	}
	path, err := filepath.Abs(o.Config)
	if err != nil {
		return r, err
	}
	r.Path = path
	if o.Mode == "" {
		o.Mode = "local"
		if o.Server != "" {
			o.Mode = "cloud"
		}
	}
	argv := []string{"mcp", "--mode", o.Mode, "--workspace", o.Workspace, "--project", o.Project, "--session", o.Session}
	switch o.Mode {
	case "local":
		if o.Server != "" || o.Audience != "" || o.Account != "" || o.ImpersonateServiceAccount != "" || o.Principal != "" || o.Cache != "" ||
			o.CredentialSource != "" || o.CredentialsFile != "" || o.OAuthClientID != "" || o.OAuthDeviceClientID != "" || o.OAuthKeyringService != "" {
			return r, errors.New("cloud credential routing cannot be supplied for a local MCP entry")
		}
		argv = append(argv, "--db", db)
	case "cloud":
		if o.CredentialSource == "" {
			return r, errors.New("cloud installation requires an explicit credential source")
		}
		if o.Cache == "" {
			u, e := os.UserHomeDir()
			if e != nil {
				return r, e
			}
			home := os.Getenv("CLAWBRIDGE_HOME")
			if home == "" {
				home = filepath.Join(u, ".clawbridge")
			}
			o.Cache = filepath.Join(home, "cache.db")
		}
		cache, e := filepath.Abs(o.Cache)
		if e != nil {
			return r, e
		}
		route, e := remote.ValidateRoute(remote.Config{
			URL: o.Server, Audience: o.Audience, Account: o.Account,
			ImpersonateServiceAccount: o.ImpersonateServiceAccount,
			Principal:                 o.Principal, CachePath: cache,
			CredentialSource: o.CredentialSource, CredentialsFile: o.CredentialsFile,
			OAuthClientID: o.OAuthClientID, OAuthDeviceClientID: o.OAuthDeviceClientID,
			OAuthKeyringService: o.OAuthKeyringService,
		})
		if e != nil {
			return r, e
		}
		argv = append(argv, "--server", route.URL, "--audience", route.Audience, "--credential-source", route.CredentialSource)
		switch route.CredentialSource {
		case remote.CredentialSourceGcloud:
			argv = append(argv, "--account", route.Account, "--impersonate-service-account", route.ImpersonateServiceAccount)
		case remote.CredentialSourceADC:
			if !filepath.IsAbs(route.CredentialsFile) {
				return r, errors.New("ADC credentials file must be an absolute path")
			}
			argv = append(argv, "--credentials-file", route.CredentialsFile, "--account", route.Account, "--impersonate-service-account", route.ImpersonateServiceAccount)
		case remote.CredentialSourceOAuth:
			argv = append(argv, "--oauth-client-id", route.OAuthClientID)
			if route.OAuthDeviceClientID != "" {
				argv = append(argv, "--oauth-device-client-id", route.OAuthDeviceClientID)
			}
			if route.OAuthKeyringService != "" {
				argv = append(argv, "--oauth-keyring-service", route.OAuthKeyringService)
			}
		}
		argv = append(argv, "--principal", route.Principal, "--cache", route.CachePath)
	default:
		return r, errors.New("MCP installation mode must be local or cloud")
	}
	if o.LinearProfile != "" {
		argv = append(argv, "--linear-profile", o.LinearProfile)
	}
	if o.Instance != "" {
		argv = append(argv, "--instance", o.Instance)
	}
	if o.ClientConfig != "" {
		argv = append(argv, "--client-config", o.ClientConfig)
	}
	entry := map[string]any{"command": command, "args": argv}
	if o.Agent == "claude-code" {
		entry["type"] = "stdio"
	}
	r.Entry = entry
	if !o.DryRun {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return r, err
		}
		lock, e := os.OpenFile(path+".clawbridge.lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return r, errors.New("cannot acquire configuration lock; another installer may be running (inspect .clawbridge.lock)")
		}
		lock.Close()
		defer os.Remove(path + ".clawbridge.lock")
	}
	before, mode, exists, err := readConfig(path)
	if err != nil {
		return r, err
	}
	after, changed, err := merge(o.Agent, o.Name, before, entry)
	if err != nil {
		return r, err
	}
	r.Changed = changed
	if !changed || o.DryRun {
		return r, nil
	}
	if exists {
		backup := path + ".clawbridge-backup-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		f, e := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return r, e
		}
		_, writeErr := f.Write(before)
		closeErr := f.Close()
		if writeErr != nil {
			return r, writeErr
		}
		if closeErr != nil {
			return r, closeErr
		}
		r.Backup = backup
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".clawbridge-config-*")
	if err != nil {
		return r, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return r, err
	}
	if _, err = f.Write(after); err != nil {
		f.Close()
		return r, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return r, err
	}
	if err = f.Close(); err != nil {
		return r, err
	}
	current, _, nowExists, err := readConfig(path)
	if err != nil {
		return r, err
	}
	if exists != nowExists || !bytes.Equal(before, current) {
		return r, errors.New("configuration changed during installation; original file preserved, retry after the other writer finishes")
	}
	if err = os.Rename(tmp, path); err != nil {
		return r, err
	}
	return r, nil
}

func readConfig(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, 0600, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, errors.New("configuration must be a regular file")
	}
	if info.Size() > 4<<20 {
		return nil, 0, false, errors.New("configuration exceeds 4 MiB limit")
	}
	data, err := os.ReadFile(path)
	return data, info.Mode().Perm(), true, err
}

func merge(agent, name string, before []byte, entry map[string]any) ([]byte, bool, error) {
	root := map[string]any{}
	key := "mcpServers"
	if agent == "codex" {
		key = "mcp_servers"
	}
	if len(bytes.TrimSpace(before)) != 0 {
		if agent == "codex" {
			if toml.Unmarshal(before, &root) != nil {
				return nil, false, errors.New("invalid TOML configuration; file left untouched")
			}
		} else {
			dec := json.NewDecoder(bytes.NewReader(before))
			dec.UseNumber()
			if dec.Decode(&root) != nil || root == nil {
				return nil, false, errors.New("invalid JSON configuration object; file left untouched")
			}
			var extra any
			if dec.Decode(&extra) != io.EOF {
				return nil, false, errors.New("invalid trailing JSON configuration; file left untouched")
			}
		}
	}
	servers := map[string]any{}
	if value, ok := root[key]; ok {
		var valid bool
		servers, valid = value.(map[string]any)
		if !valid {
			return nil, false, errors.New("existing MCP servers setting must be an object/table; file left untouched")
		}
	}
	if old, ok := servers[name]; ok {
		if equivalent(old, entry) {
			return before, false, nil
		}
		return nil, false, errors.New("MCP entry already exists with different settings; choose a different --name or edit that entry explicitly")
	}
	if agent == "codex" {
		block, err := toml.Marshal(map[string]any{"mcp_servers": map[string]any{name: entry}})
		if err != nil {
			return nil, false, err
		}
		after := append(append([]byte{}, before...), []byte("\n"+strings.TrimSpace(string(block))+"\n")...)
		var verified map[string]any
		if toml.Unmarshal(after, &verified) != nil {
			return nil, false, errors.New("cannot append MCP table to this TOML layout; file left untouched; use a separate config path")
		}
		return after, true, nil
	}
	servers[name] = entry
	root[key] = servers
	after, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(after, '\n'), true, nil
}

func equivalent(a, b any) bool {
	aa, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	var x, y any
	if json.Unmarshal(aa, &x) != nil || json.Unmarshal(bb, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
