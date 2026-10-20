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
	DB        string `toml:"db"`
	Workspace string `toml:"workspace"`
	Project   string `toml:"project"`
	Session   string `toml:"session"`
	Instance  string `toml:"instance"`
	Mode      string `toml:"mode"`
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
			base = overlay(base, repo)
		}
	}
	env := Config{DB: os.Getenv("CLAWBRIDGE_DB"), Workspace: os.Getenv("CLAWBRIDGE_WORKSPACE"), Project: os.Getenv("CLAWBRIDGE_PROJECT"), Session: os.Getenv("CLAWBRIDGE_SESSION"), Instance: os.Getenv("CLAWBRIDGE_INSTANCE"), Mode: os.Getenv("CLAWBRIDGE_MODE")}
	base = overlay(overlay(base, env), flags)
	if base.DB == "" {
		base.DB = filepath.Join(home, "local.db")
	}
	abs, err := filepath.Abs(base.DB)
	if err != nil {
		return Config{}, "", err
	}
	base.DB = abs
	if base.Mode != "" && base.Mode != "local" {
		return Config{}, "", errors.New("configured storage mode is not local; cloud/mirror mode is unavailable, no write queued")
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
	if old, ok := values["mode"].(string); ok && old != "" && old != "local" {
		return errors.New("refusing to replace nonlocal configuration")
	}
	values["mode"] = "local"
	values["workspace"] = c.Workspace
	values["db"] = c.DB
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
