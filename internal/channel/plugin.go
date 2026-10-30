package channel

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// The plugin's setup (FR26 D2): the marketplace is the public repo, and the
// plugin is installed for the user.
const (
	// PluginID is the plugin as Claude Code's settings name it.
	PluginID = "clawbridge@clawbridge"
	// Marketplace is the marketplace's name.
	Marketplace = "clawbridge"
)

// SetupCommands are the two `claude` commands that install the plugin, as
// argv after `claude`.
var SetupCommands = [][]string{
	{"plugin", "marketplace", "add", "jkai-chen/ClawBridge"},
	{"plugin", "install", PluginID, "--scope", "user"},
}

// LaunchLine is how a session starts Claude Code with our channel.
func LaunchLine(session, instance string) string {
	return "CLAWBRIDGE_SESSION=" + session + " CLAWBRIDGE_INSTANCE=" + instance + " claude --channels " + PluginChannel
}

// PluginState is how far the plugin is set up, as Claude Code's settings
// tell it.
type PluginState struct {
	// Marketplace says Claude Code knows our marketplace; Enabled that the
	// plugin is enabled for this project.
	Marketplace, Enabled bool
}

// DetectPlugin reads Claude Code's settings, never writing them: the
// user's in config (CLAUDE_CONFIG_DIR or ~/.claude) and, with a project,
// its .claude/settings.json and settings.local.json, the narrower scope
// winning for enabledPlugins. An unreadable file counts as saying nothing.
func DetectPlugin(config, project string) PluginState {
	var st PluginState
	files := []string{filepath.Join(config, "settings.json")}
	if project != "" {
		files = append(files, filepath.Join(project, ".claude", "settings.json"), filepath.Join(project, ".claude", "settings.local.json"))
	}
	for _, path := range files {
		var s struct {
			EnabledPlugins         map[string]any `json:"enabledPlugins"`
			ExtraKnownMarketplaces map[string]any `json:"extraKnownMarketplaces"`
		}
		if !readJSON(path, &s) {
			continue
		}
		if on, ok := s.EnabledPlugins[PluginID].(bool); ok {
			st.Enabled = on
		}
		if _, ok := s.ExtraKnownMarketplaces[Marketplace]; ok {
			st.Marketplace = true
		}
	}
	var known map[string]any
	if readJSON(filepath.Join(config, "plugins", "known_marketplaces.json"), &known) {
		if _, ok := known[Marketplace]; ok {
			st.Marketplace = true
		}
	}
	return st
}

func readJSON(path string, v any) bool {
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, v) == nil
}
