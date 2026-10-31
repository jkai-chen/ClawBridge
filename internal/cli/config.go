package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"clawbridge/internal/remote"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	LinearProfile string `toml:"-"`
	ClientConfig  string `toml:"-"`
	// JSONFlag is --json as given ("", "true" or "false"): the output format
	// (internal/clirender). Never written to a config file.
	JSONFlag  string `toml:"-"`
	DB        string `toml:"db"`
	Workspace string `toml:"workspace"`
	Project   string `toml:"project"`
	Session   string `toml:"session"`
	Instance  string `toml:"instance"`
	Mode      string `toml:"mode"`
	Server    string `toml:"server"`
	Principal string `toml:"principal"`
	Cache     string `toml:"cache"`
	// CredentialSource is device, the only one since v1.0 removed Google
	// sign-in (oauth, gcloud, adc).
	CredentialSource string `toml:"credential_source"`
	// A device route (credential_source = "device"): this device's ID and
	// where its key is kept, and the clock correction measured against the
	// server (live/AUTH-INVITE-DESIGN.md §4, §7).
	DeviceID           string `toml:"device_id"`
	KeyStore           string `toml:"key_store"`
	ClockOffsetSeconds int64  `toml:"clock_offset_seconds"`
	ClockOffsetAt      string `toml:"clock_offset_at"`
	// Sources names, for workspace, project, session and instance, the
	// layer that last set each: "flag", "env CLAWBRIDGE_PROJECT" (the
	// field's own variable), ".clawbridge.toml at PATH" or "config file
	// PATH". A field no layer set has no entry. Never written to a config
	// file, and overlay never copies it.
	Sources map[string]string `toml:"-"`
	// NoProfile is set by resolveConfig when this machine has no profile at
	// all: no config file at the resolved path, and no route from a
	// repository file, the environment or a flag. Doctor then says so once.
	// Never written to a config file.
	NoProfile bool `toml:"-"`
}

// scopeSourceFields are the scope fields whose source Config.Sources
// records, each with its environment variable.
var scopeSourceFields = [...]struct {
	name, env string
	value     func(Config) string
}{
	{"workspace", "CLAWBRIDGE_WORKSPACE", func(c Config) string { return c.Workspace }},
	{"project", "CLAWBRIDGE_PROJECT", func(c Config) string { return c.Project }},
	{"session", "CLAWBRIDGE_SESSION", func(c Config) string { return c.Session }},
	{"instance", "CLAWBRIDGE_INSTANCE", func(c Config) string { return c.Instance }},
}

// noteSources records source for each scope field layer sets: a later
// layer replaces an earlier one's, as overlay does.
func noteSources(sources map[string]string, layer Config, source func(env string) string) {
	for _, f := range scopeSourceFields {
		if f.value(layer) != "" {
			sources[f.name] = source(f.env)
		}
	}
}

func stateHome() (string, error) {
	if dir := os.Getenv("CLAWBRIDGE_HOME"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".clawbridge"), nil
}

// warnIgnoredRepoConfig warns, on stderr, about a .clawbridge.toml in the
// working directory that is not read: one is read only at a git checkout's
// root.
func warnIgnoredRepoConfig(root string) {
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	if _, err = os.Stat(filepath.Join(wd, ".clawbridge.toml")); err != nil {
		return
	}
	switch {
	case root == "":
		fmt.Fprintln(os.Stderr, "warning: ./.clawbridge.toml is ignored: it is read only at the root of a git checkout, and this directory is not in one. Pass --project, or set a default project with clawbridge project create.")
	case filepath.Clean(root) != filepath.Clean(wd):
		if real, e := filepath.EvalSymlinks(wd); e != nil || filepath.Clean(real) != filepath.Clean(root) {
			fmt.Fprintf(os.Stderr, "warning: ./.clawbridge.toml is ignored: it is read only at the checkout root (%s).\n", root)
		}
	}
}

func checkoutRoot(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func gitRemote(ctx context.Context) string {
	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func readConfig(path string, required bool) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err = toml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if c.DB != "" && !filepath.IsAbs(c.DB) {
		c.DB = filepath.Join(filepath.Dir(path), c.DB)
	}
	if c.Cache != "" && !filepath.IsAbs(c.Cache) {
		c.Cache = filepath.Join(filepath.Dir(path), c.Cache)
	}
	return c, nil
}
func overlay(base, top Config) Config {
	if top.LinearProfile != "" {
		base.LinearProfile = top.LinearProfile
	}
	if top.DB != "" {
		base.DB = top.DB
	}
	if top.Workspace != "" {
		base.Workspace = top.Workspace
	}
	if top.Project != "" {
		base.Project = top.Project
	}
	if top.Session != "" {
		base.Session = top.Session
	}
	if top.Instance != "" {
		base.Instance = top.Instance
	}
	if top.Mode != "" {
		base.Mode = top.Mode
	}
	if top.Server != "" {
		base.Server = top.Server
	}
	if top.Principal != "" {
		base.Principal = top.Principal
	}
	if top.Cache != "" {
		base.Cache = top.Cache
	}
	if top.CredentialSource != "" {
		base.CredentialSource = top.CredentialSource
	}
	if top.DeviceID != "" {
		base.DeviceID = top.DeviceID
	}
	if top.KeyStore != "" {
		base.KeyStore = top.KeyStore
	}
	if top.ClockOffsetAt != "" {
		base.ClockOffsetSeconds, base.ClockOffsetAt = top.ClockOffsetSeconds, top.ClockOffsetAt
	}
	return base
}
func resolveConfig(ctx context.Context, flags Config) (Config, string, error) {
	return resolveConfigVariant(ctx, flags, nil)
}

// resolveConfigVariant applies the same trusted precedence everywhere. The
// optional variant sees the merged route before validation so commands which
// deliberately probe one credential backend can isolate it from another without
// mutating the trusted configuration.
func resolveConfigVariant(ctx context.Context, flags Config, variant func(Config) Config) (Config, string, error) {
	home, err := stateHome()
	if err != nil {
		return Config{}, "", err
	}
	path := filepath.Join(home, "config.toml")
	base := Config{}
	sources := map[string]string{}
	fromFile := func(file string) func(string) string {
		if abs, e := filepath.Abs(file); e == nil {
			file = abs
		}
		return func(string) string { return "config file " + file }
	}
	if explicit := firstConfigPath(flags.ClientConfig, os.Getenv("CLAWBRIDGE_CONFIG")); explicit != "" {
		path = explicit
		c, e := readConfig(path, false)
		if e != nil {
			return Config{}, "", e
		}
		noteLegacyGoogle(path)
		base = overlay(base, c)
		noteSources(sources, c, fromFile(path))
	} else {
		base, err = readConfig(path, false)
		if err != nil {
			return Config{}, "", err
		}
		noteLegacyGoogle(path)
		noteSources(sources, base, fromFile(path))
		root := checkoutRoot(ctx)
		warnIgnoredRepoConfig(root)
		if root != "" {
			repo, e := readConfig(filepath.Join(root, ".clawbridge.toml"), false)
			if e != nil {
				return Config{}, "", e
			}
			// Repository files select scope; they cannot route credentials or cache.
			// Authority mode is trusted configuration too: a repository may not
			// switch local to cloud, or cloud to local.
			repo.Mode = ""
			repo.Server = ""
			repo.Principal = ""
			repo.Cache = ""
			repo.CredentialSource = ""
			repo.DeviceID = ""
			repo.KeyStore = ""
			repo.ClockOffsetSeconds, repo.ClockOffsetAt = 0, ""
			base = overlay(base, repo)
			repoPath := filepath.Join(root, ".clawbridge.toml")
			noteSources(sources, repo, func(string) string { return ".clawbridge.toml at " + repoPath })
		}
	}
	env := Config{
		DB: os.Getenv("CLAWBRIDGE_DB"), Workspace: os.Getenv("CLAWBRIDGE_WORKSPACE"),
		Project: os.Getenv("CLAWBRIDGE_PROJECT"), Session: os.Getenv("CLAWBRIDGE_SESSION"),
		Instance: os.Getenv("CLAWBRIDGE_INSTANCE"), Mode: os.Getenv("CLAWBRIDGE_MODE"),
		Server:    os.Getenv("CLAWBRIDGE_SERVER"),
		Principal: os.Getenv("CLAWBRIDGE_PRINCIPAL"), Cache: os.Getenv("CLAWBRIDGE_CACHE"),
		CredentialSource: os.Getenv("CLAWBRIDGE_CREDENTIAL_SOURCE"),
		DeviceID:         os.Getenv("CLAWBRIDGE_DEVICE_ID"),
		KeyStore:         os.Getenv("CLAWBRIDGE_KEY_STORE"),
	}
	if guided, _ := ctx.Value(guidedRouteKey{}).(bool); guided {
		env = Config{}
	}
	base = overlay(overlay(base, env), flags)
	// With no file, whatever is set came from the repository, the environment
	// or a flag: a profile only if any of it is.
	_, statErr := os.Stat(path)
	noProfile := errors.Is(statErr, os.ErrNotExist) && !routeSet(base)
	// Local mode needs no file: a store at the default path is a profile too.
	// Only stat it here; doctor must not open or create it.
	defaultDB := base.DB == ""
	noteSources(sources, env, func(name string) string { return "env " + name })
	noteSources(sources, flags, func(string) string { return "flag" })
	base.Sources = sources
	if variant != nil {
		base = variant(base)
	}
	if base.DB == "" {
		base.DB = filepath.Join(home, "local.db")
	}
	abs, err := filepath.Abs(base.DB)
	if err != nil {
		return Config{}, "", err
	}
	base.DB = abs
	if noProfile && defaultDB {
		if _, e := os.Stat(base.DB); e == nil || !errors.Is(e, os.ErrNotExist) {
			noProfile = false
		}
	}
	if base.Mode != "" && base.Mode != "local" && base.Mode != "cloud" {
		return Config{}, "", errors.New("unsupported storage mode; no write queued")
	}
	if base.Mode == "" {
		if base.Server != "" || base.Principal != "" || base.Cache != "" ||
			base.CredentialSource != "" || base.DeviceID != "" || base.KeyStore != "" {
			return Config{}, "", errors.New("cloud routing requires explicit mode=cloud; no local fallback")
		}
		base.Mode = "local"
	}
	if base.Mode == "cloud" && base.Server == "" {
		return Config{}, "", errors.New("cloud mode requires trusted server configuration")
	}
	if base.Cache != "" && !filepath.IsAbs(base.Cache) {
		base.Cache = filepath.Join(home, base.Cache)
	}
	// A device key is the only sign-in since v1.0: a profile that still
	// names Google (oauth, gcloud, adc, or none in cloud mode) is told how
	// to join.
	if base.Mode == "cloud" && base.CredentialSource != "device" {
		return Config{}, "", errors.New(remote.GoogleRouteRemoved)
	}
	if base.CredentialSource != "" && base.CredentialSource != "device" {
		return Config{}, "", errors.New("unsupported credential source; device is the only one")
	}
	if base.CredentialSource == "device" && (base.DeviceID == "" || base.KeyStore == "") {
		return Config{}, "", errors.New("the device credential source needs device_id and key_store in trusted configuration; join with an invite")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return Config{}, "", err
	}
	base.ClientConfig = path
	base.NoProfile = noProfile
	return base, path, nil
}

// routeSet reports whether c names any part of a profile: a route, a scope
// or a Linear profile.
func routeSet(c Config) bool {
	return c.LinearProfile != "" || c.DB != "" || c.Workspace != "" || c.Project != "" ||
		c.Session != "" || c.Instance != "" || c.Mode != "" || c.Server != "" ||
		c.Principal != "" || c.Cache != "" || c.CredentialSource != "" ||
		c.DeviceID != "" || c.KeyStore != ""
}

// writeDefaultProject saves project as the default of the profile file at
// path, local or cloud: always when replace, otherwise only when it has none.
// It keeps every other key, a cloud profile's device credential included. It
// writes nothing when there is no file (a profile set only by the
// environment, such as a CI device's) or when the file is for another mode
// than mode, the one in effect (a file without one is local), so a hosted
// project never lands in a local profile.
func writeDefaultProject(path, mode, project string, replace bool) error {
	values := map[string]any{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = toml.Unmarshal(b, &values); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	fileMode, _ := values["mode"].(string)
	if fileMode == "" {
		fileMode = "local"
	}
	if fileMode != mode || (mode != "local" && mode != "cloud") {
		return nil
	}
	if current, _ := values["project"].(string); current != "" && !replace {
		return nil
	}
	values["project"] = project
	return writeConfigValues(path, values)
}

// writeConfigValues writes a config file atomically, readable by its owner.
func writeConfigValues(path string, values map[string]any) error {
	b, err := toml.Marshal(values)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".clawbridge-config-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeSetupConfig preserves unrelated TOML keys and uses a same-directory rename.
func writeSetupConfig(path string, c Config) error {
	values := map[string]any{}
	b, err := os.ReadFile(path)
	if err == nil {
		if err = toml.Unmarshal(b, &values); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	mode := c.Mode
	if mode == "" {
		mode = "local"
	}
	if old, ok := values["mode"].(string); ok && old != "" && old != mode {
		return errors.New("refusing to replace nonlocal configuration")
	}
	values["mode"] = mode
	values["workspace"] = c.Workspace
	if mode == "local" {
		values["db"] = c.DB
	} else {
		for key, value := range map[string]string{
			"server": c.Server, "principal": c.Principal, "cache": c.Cache,
			"credential_source": c.CredentialSource,
		} {
			if value != "" {
				values[key] = value
			}
		}
	}
	b, err = toml.Marshal(values)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".clawbridge-config-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func firstConfigPath(explicit, environment string) string {
	if explicit != "" {
		return explicit
	}
	return environment
}

// envMode is the storage mode the environment selects, if any.
func envMode() string { return os.Getenv("CLAWBRIDGE_MODE") }

// saveClockOffset keeps a device route's measured clock correction in its
// profile (§7), changing no other key.
func saveClockOffset(path string, offset time.Duration, at time.Time) error {
	values := map[string]any{}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err = toml.Unmarshal(b, &values); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	if values["credential_source"] != "device" {
		return nil
	}
	values["clock_offset_seconds"] = int64(offset / time.Second)
	values["clock_offset_at"] = at.UTC().Format(time.RFC3339)
	return writeConfigValues(path, values)
}
