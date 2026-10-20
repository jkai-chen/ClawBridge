package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	DB                        string `toml:"db"`
	Workspace                 string `toml:"workspace"`
	Project                   string `toml:"project"`
	Session                   string `toml:"session"`
	Instance                  string `toml:"instance"`
	Mode                      string `toml:"mode"`
	Server                    string `toml:"server"`
	Audience                  string `toml:"audience"`
	Account                   string `toml:"account"`
	ImpersonateServiceAccount string `toml:"impersonate_service_account"`
	Principal                 string `toml:"principal"`
	Cache                     string `toml:"cache"`
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
	if top.Audience != "" {
		base.Audience = top.Audience
	}
	if top.Account != "" {
		base.Account = top.Account
	}
	if top.ImpersonateServiceAccount != "" {
		base.ImpersonateServiceAccount = top.ImpersonateServiceAccount
	}
	if top.Principal != "" {
		base.Principal = top.Principal
	}
	if top.Cache != "" {
		base.Cache = top.Cache
	}
	return base
}
func resolveConfig(ctx context.Context, flags Config) (Config, string, error) {
	home, err := stateHome()
	if err != nil {
		return Config{}, "", err
	}
	path := filepath.Join(home, "config.toml")
	base := Config{}
	if explicit := os.Getenv("CLAWBRIDGE_CONFIG"); explicit != "" {
		path = explicit
		c, e := readConfig(path, false)
		if e != nil {
			return Config{}, "", e
		}
		base = overlay(base, c)
	} else {
		base, err = readConfig(path, false)
		if err != nil {
			return Config{}, "", err
		}
		if root := checkoutRoot(ctx); root != "" {
			repo, e := readConfig(filepath.Join(root, ".clawbridge.toml"), false)
			if e != nil {
				return Config{}, "", e
			}
			// Repository files select scope; they cannot route credentials or cache.
			repo.Server = ""
			repo.Audience = ""
			repo.Account = ""
			repo.ImpersonateServiceAccount = ""
			repo.Principal = ""
			repo.Cache = ""
			base = overlay(base, repo)
		}
	}
	env := Config{DB: os.Getenv("CLAWBRIDGE_DB"), Workspace: os.Getenv("CLAWBRIDGE_WORKSPACE"), Project: os.Getenv("CLAWBRIDGE_PROJECT"), Session: os.Getenv("CLAWBRIDGE_SESSION"), Instance: os.Getenv("CLAWBRIDGE_INSTANCE"), Mode: os.Getenv("CLAWBRIDGE_MODE"), Server: os.Getenv("CLAWBRIDGE_SERVER"), Audience: os.Getenv("CLAWBRIDGE_AUDIENCE"), Account: os.Getenv("CLAWBRIDGE_ACCOUNT"), ImpersonateServiceAccount: os.Getenv("CLAWBRIDGE_IMPERSONATE_SERVICE_ACCOUNT"), Principal: os.Getenv("CLAWBRIDGE_PRINCIPAL"), Cache: os.Getenv("CLAWBRIDGE_CACHE")}
	base = overlay(overlay(base, env), flags)
	if base.DB == "" {
		base.DB = filepath.Join(home, "local.db")
	}
	abs, err := filepath.Abs(base.DB)
	if err != nil {
		return Config{}, "", err
	}
	base.DB = abs
	if base.Mode != "" && base.Mode != "local" && base.Mode != "cloud" {
		return Config{}, "", errors.New("unsupported storage mode; no write queued")
	}
	if base.Mode == "" {
		if base.Server != "" || base.Audience != "" || base.Principal != "" || base.ImpersonateServiceAccount != "" || base.Cache != "" {
			return Config{}, "", errors.New("cloud routing requires explicit mode=cloud; no local fallback")
		}
		base.Mode = "local"
	}
	if base.Mode == "cloud" && (base.Server == "" || base.Audience == "") {
		return Config{}, "", errors.New("cloud mode requires trusted server and audience configuration")
	}
	if base.Cache != "" && !filepath.IsAbs(base.Cache) {
		base.Cache = filepath.Join(home, base.Cache)
	}
	return base, path, nil
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
		for key, value := range map[string]string{"server": c.Server, "audience": c.Audience, "account": c.Account, "impersonate_service_account": c.ImpersonateServiceAccount, "principal": c.Principal, "cache": c.Cache} {
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
