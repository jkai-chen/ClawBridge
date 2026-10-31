package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Settings is everything the GUI stores about the backend. The WSL distro
// name is the only WSL thing kept (GUI-DESIGN §6.2); no ClawBridge secret
// is ever stored here.
type Settings struct {
	// BackendPath is an absolute path to the clawbridge client, chosen with
	// the native file dialog. Empty means: find `clawbridge` on PATH.
	BackendPath string `json:"backend_path,omitempty"`
	// WSLDistro is the distro picked on Windows. Empty means: the native
	// Windows client if clawbridge.exe is on PATH.
	WSLDistro string `json:"wsl_distro,omitempty"`
}

// SettingsStore reads and writes the settings file.
type SettingsStore struct{ Path string }

// DefaultSettingsStore puts the file in the user's config directory.
func DefaultSettingsStore() (*SettingsStore, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &SettingsStore{Path: filepath.Join(dir, "ClawBridge GUI", "settings.json")}, nil
}

// Load returns the saved settings; a missing file is empty settings.
func (s *SettingsStore) Load() (Settings, error) {
	var out Settings
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Settings{}, fmt.Errorf("settings file %s: %w", s.Path, err)
	}
	return out, nil
}

// Save writes the settings atomically with owner-only permissions.
func (s *SettingsStore) Save(v Settings) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".settings-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, s.Path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
