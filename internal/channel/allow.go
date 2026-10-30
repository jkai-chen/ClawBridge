package channel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"clawbridge/internal/ctxwatch"
)

// Claude Code takes a channel push only from a plugin on its allowlist:
// Anthropic's default, or an organisation's allowedChannelPlugins from
// managed settings, which replaces the default (FR26 D1). `clawbridge
// channel allow` is the one-time admin step that lists ours on a machine.

// AllowFileName is our managed-settings drop-in.
const AllowFileName = "50-clawbridge-channels.json"

// AllowJSON is the drop-in, byte for byte. Anthropic's four official
// channel plugins are listed too: a managed list replaces the default one.
const AllowJSON = `{
  "channelsEnabled": true,
  "allowedChannelPlugins": [
    {
      "marketplace": "clawbridge",
      "plugin": "clawbridge"
    },
    {
      "marketplace": "claude-plugins-official",
      "plugin": "discord"
    },
    {
      "marketplace": "claude-plugins-official",
      "plugin": "telegram"
    },
    {
      "marketplace": "claude-plugins-official",
      "plugin": "fakechat"
    },
    {
      "marketplace": "claude-plugins-official",
      "plugin": "imessage"
    }
  ]
}
`

// ManagedSettingsDir is where Claude Code reads managed settings on goos.
func ManagedSettingsDir(goos string) string {
	switch goos {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "windows":
		return `C:\Program Files\ClaudeCode`
	}
	return "/etc/claude-code"
}

// AllowPath is the drop-in's path on goos.
func AllowPath(goos string) string {
	sep := "/"
	if goos == "windows" {
		sep = `\`
	}
	return ManagedSettingsDir(goos) + sep + "managed-settings.d" + sep + AllowFileName
}

// ChannelAllowlisted says whether this machine's managed settings allowlist
// our plugin as a channel, and in which file. It only reads.
func ChannelAllowlisted() (bool, string, error) {
	return AllowlistedIn(ManagedSettingsDir(runtime.GOOS))
}

// AllowlistedIn is ChannelAllowlisted for the managed settings under root:
// managed-settings.json, then managed-settings.d/*.json in name order,
// merged as Claude Code merges them (lists add up, the last value of
// channelsEnabled wins). ok needs our plugin listed and channelsEnabled
// true; where is the file listing it, if any does. MDM and server-managed
// settings are not seen.
func AllowlistedIn(root string) (ok bool, where string, err error) {
	docs := []string{filepath.Join(root, "managed-settings.json")}
	entries, err := os.ReadDir(filepath.Join(root, "managed-settings.d"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, "", fmt.Errorf("the managed settings in %s are not readable", root)
	}
	var names []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") && strings.HasSuffix(e.Name(), ".json") && !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		docs = append(docs, filepath.Join(root, "managed-settings.d", n))
	}
	enabled := false
	for _, path := range docs {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var doc struct {
			ChannelsEnabled       *bool `json:"channelsEnabled"`
			AllowedChannelPlugins []any `json:"allowedChannelPlugins"`
		}
		if err != nil || json.Unmarshal(data, &doc) != nil {
			return false, "", fmt.Errorf("the managed settings file %s is not readable JSON", path)
		}
		if doc.ChannelsEnabled != nil {
			enabled = *doc.ChannelsEnabled
		}
		for _, p := range doc.AllowedChannelPlugins {
			if m, _ := p.(map[string]any); m["marketplace"] == "clawbridge" && m["plugin"] == "clawbridge" && where == "" {
				where = path
			}
		}
	}
	return enabled && where != "", where, nil
}

var (
	// ErrAllowDiffers is a drop-in already there with other content.
	ErrAllowDiffers = errors.New("a different drop-in is already there")
	// ErrAllowNotPermitted is a managed-settings folder this process may
	// not write: the person didn't run it as an administrator.
	ErrAllowNotPermitted = errors.New("not permitted to write the managed settings")
)

// AllowWrite is what WriteAllow did: the drop-in's path, whether it wrote
// it, and, for a different drop-in already there, the change.
type AllowWrite struct {
	Path    string
	Written bool
	Diff    string
}

// WriteAllow writes the drop-in under root (ManagedSettingsDir in use),
// atomically, 0644, creating missing folders 0755. A drop-in already there
// with the same bytes is left as it is; with others, only force replaces
// it. It writes nothing else, and never raises its own privileges.
func WriteAllow(root string, force bool) (AllowWrite, error) {
	dir := filepath.Join(root, "managed-settings.d")
	w := AllowWrite{Path: filepath.Join(dir, AllowFileName)}
	info, err := os.Lstat(w.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return w, notPermitted(w.Path, err)
	case !info.Mode().IsRegular():
		return w, fmt.Errorf("%s is a link or not a regular file; remove it by hand first", w.Path)
	default:
		before, err := os.ReadFile(w.Path)
		if err != nil {
			return w, notPermitted(w.Path, err)
		}
		if bytes.Equal(before, []byte(AllowJSON)) {
			return w, nil
		}
		w.Diff = ctxwatch.HookChange{Before: before, After: []byte(AllowJSON)}.Diff()
		if !force {
			return w, ErrAllowDiffers
		}
	}
	for _, d := range []string{root, dir} {
		if err := mkdirPublic(d); err != nil {
			return w, notPermitted(d, err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".clawbridge-channels-*.tmp")
	if err != nil {
		return w, notPermitted(dir, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	err = tmp.Chmod(0o644)
	if err == nil {
		_, err = tmp.WriteString(AllowJSON)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(name, w.Path)
	}
	if err != nil {
		return w, notPermitted(w.Path, err)
	}
	w.Written = true
	return w, nil
}

// mkdirPublic makes dir 0755 when it is missing, whatever the umask: every
// Claude Code user on the machine reads it.
func mkdirPublic(dir string) error {
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a folder", dir)
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}

func notPermitted(path string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %s", ErrAllowNotPermitted, path)
	}
	return fmt.Errorf("%s could not be written: %v", path, err)
}
