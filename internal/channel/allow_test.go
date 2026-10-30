package channel

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const wantAllow = `{
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

// The drop-in is the owner's exact document: ours, then Anthropic's four
// official channel plugins, which the list would otherwise replace.
func TestAllowDocument(t *testing.T) {
	if AllowJSON != wantAllow {
		t.Fatalf("%s", AllowJSON)
	}
	var doc struct {
		ChannelsEnabled       bool                `json:"channelsEnabled"`
		AllowedChannelPlugins []map[string]string `json:"allowedChannelPlugins"`
	}
	if err := json.Unmarshal([]byte(AllowJSON), &doc); err != nil || !doc.ChannelsEnabled || len(doc.AllowedChannelPlugins) != 5 {
		t.Fatal(err, doc)
	}
	for goos, want := range map[string]string{
		"darwin":  "/Library/Application Support/ClaudeCode/managed-settings.d/50-clawbridge-channels.json",
		"linux":   "/etc/claude-code/managed-settings.d/50-clawbridge-channels.json",
		"windows": `C:\Program Files\ClaudeCode\managed-settings.d\50-clawbridge-channels.json`,
	} {
		if got := AllowPath(goos); got != want {
			t.Fatalf("%s: %q", goos, got)
		}
	}
}

func writeDoc(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ChannelAllowlisted reads managed-settings.json and then the drop-ins in
// name order, as Claude Code merges them: lists add up, and the last
// channelsEnabled wins.
func TestAllowlistedMergesTheDocuments(t *testing.T) {
	ours := `{"allowedChannelPlugins":[{"marketplace":"clawbridge","plugin":"clawbridge"}]}`
	other := `{"allowedChannelPlugins":[{"marketplace":"claude-plugins-official","plugin":"telegram"}]}`
	for name, c := range map[string]struct {
		docs  map[string]string
		ok    bool
		where string
		err   bool
	}{
		"none":            {nil, false, "", false},
		"drop-in":         {map[string]string{"managed-settings.d/50-clawbridge-channels.json": AllowJSON}, true, "managed-settings.d/50-clawbridge-channels.json", false},
		"split":           {map[string]string{"managed-settings.json": `{"channelsEnabled": true}`, "managed-settings.d/10-a.json": other, "managed-settings.d/20-b.json": ours}, true, "managed-settings.d/20-b.json", false},
		"base lists":      {map[string]string{"managed-settings.json": `{"channelsEnabled": true, "allowedChannelPlugins":[{"marketplace":"clawbridge","plugin":"clawbridge"}]}`, "managed-settings.d/10-a.json": other}, true, "managed-settings.json", false},
		"not enabled":     {map[string]string{"managed-settings.d/20-b.json": ours}, false, "managed-settings.d/20-b.json", false},
		"later disables":  {map[string]string{"managed-settings.d/50-clawbridge-channels.json": AllowJSON, "managed-settings.d/90-off.json": `{"channelsEnabled": false}`}, false, "managed-settings.d/50-clawbridge-channels.json", false},
		"earlier enables": {map[string]string{"managed-settings.json": `{"channelsEnabled": false}`, "managed-settings.d/10-on.json": `{"channelsEnabled": true}`, "managed-settings.d/20-b.json": ours}, true, "managed-settings.d/20-b.json", false},
		"others only":     {map[string]string{"managed-settings.json": `{"channelsEnabled": true}`, "managed-settings.d/10-a.json": other}, false, "", false},
		"lookalike":       {map[string]string{"managed-settings.json": `{"channelsEnabled": true, "allowedChannelPlugins":[{"marketplace":"clawbridge-evil","plugin":"clawbridge"},{"marketplace":"clawbridge"},"clawbridge@clawbridge"]}`}, false, "", false},
		"not json files":  {map[string]string{"managed-settings.d/50-x.json.bak": AllowJSON, "managed-settings.d/.50-x.json": AllowJSON}, false, "", false},
		"broken":          {map[string]string{"managed-settings.d/10-a.json": `{`}, false, "", true},
	} {
		root := t.TempDir()
		for rel, content := range c.docs {
			writeDoc(t, filepath.Join(root, filepath.FromSlash(rel)), content)
		}
		ok, where, err := AllowlistedIn(root)
		want := ""
		if c.where != "" {
			want = filepath.Join(root, filepath.FromSlash(c.where))
		}
		if ok != c.ok || where != want || (err != nil) != c.err {
			t.Fatalf("%s: %v %q %v", name, ok, where, err)
		}
	}
}

// --write writes the exact bytes, 0644, creating the folders 0755; an
// equal file is left alone, a different one only replaced with force; a
// folder it can't write is refused.
func TestWriteAllow(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ClaudeCode")
	path := filepath.Join(root, "managed-settings.d", AllowFileName)
	res, err := WriteAllow(root, false)
	if err != nil || res.Path != path || !res.Written {
		t.Fatal(res, err)
	}
	if data, _ := os.ReadFile(path); string(data) != wantAllow {
		t.Fatalf("%s", data)
	}
	if runtime.GOOS != "windows" {
		for p, mode := range map[string]os.FileMode{path: 0o644, root: 0o755, filepath.Dir(path): 0o755} {
			if info, _ := os.Stat(p); info.Mode().Perm() != mode {
				t.Fatalf("%s: %v", p, info.Mode())
			}
		}
	}
	if res, err = WriteAllow(root, false); err != nil || res.Written {
		t.Fatal("an equal file was rewritten", res, err)
	}
	writeDoc(t, path, `{"channelsEnabled": true, "allowedChannelPlugins": []}`+"\n")
	res, err = WriteAllow(root, false)
	if !errors.Is(err, ErrAllowDiffers) || res.Written || !strings.Contains(res.Diff, `+   "allowedChannelPlugins": [`) || !strings.Contains(res.Diff, `- {"channelsEnabled": true, "allowedChannelPlugins": []}`) {
		t.Fatal(res, err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "clawbridge") {
		t.Fatal("replaced without force")
	}
	if res, err = WriteAllow(root, true); err != nil || !res.Written {
		t.Fatal(res, err)
	}
	if data, _ := os.ReadFile(path); string(data) != wantAllow {
		t.Fatalf("%s", data)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*")); len(left) != 0 {
		t.Fatal("temporary files left", left)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	locked := t.TempDir()
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if _, err = WriteAllow(filepath.Join(locked, "ClaudeCode"), false); !errors.Is(err, ErrAllowNotPermitted) {
		t.Fatal(err)
	}
	// A link where the drop-in goes is refused, not followed.
	linked := filepath.Join(t.TempDir(), "ClaudeCode")
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	writeDoc(t, target, "{}")
	if err = os.MkdirAll(filepath.Join(linked, "managed-settings.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, filepath.Join(linked, "managed-settings.d", AllowFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err = WriteAllow(linked, true); err == nil {
		t.Fatal("wrote through a link")
	}
	if data, _ := os.ReadFile(target); string(data) != "{}" {
		t.Fatalf("%s", data)
	}
}

// The plugin's setup state is read from Claude Code's settings only: a
// marketplace it knows, and the plugin enabled at the user, project or
// local scope, the narrower scope winning.
func TestDetectPlugin(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		want  PluginState
	}{
		"nothing":       {nil, PluginState{}},
		"known file":    {map[string]string{"config/plugins/known_marketplaces.json": `{"clawbridge":{"source":{"source":"github","repo":"jkai-chen/ClawBridge"}}}`}, PluginState{Marketplace: true}},
		"extra known":   {map[string]string{"config/settings.json": `{"extraKnownMarketplaces":{"clawbridge":{"source":{"source":"github","repo":"jkai-chen/ClawBridge"}}}}`}, PluginState{Marketplace: true}},
		"user enabled":  {map[string]string{"config/settings.json": `{"enabledPlugins":{"clawbridge@clawbridge":true}}`, "config/plugins/known_marketplaces.json": `{"clawbridge":{}}`}, PluginState{Marketplace: true, Enabled: true}},
		"local off":     {map[string]string{"config/settings.json": `{"enabledPlugins":{"clawbridge@clawbridge":true}}`, "project/.claude/settings.local.json": `{"enabledPlugins":{"clawbridge@clawbridge":false}}`}, PluginState{}},
		"project on":    {map[string]string{"project/.claude/settings.json": `{"enabledPlugins":{"clawbridge@clawbridge":true}}`}, PluginState{Enabled: true}},
		"other plugin":  {map[string]string{"config/settings.json": `{"enabledPlugins":{"clawbridge@other":true,"other@clawbridge":true}}`, "config/plugins/known_marketplaces.json": `{"other":{}}`}, PluginState{}},
		"not a boolean": {map[string]string{"config/settings.json": `{"enabledPlugins":{"clawbridge@clawbridge":"yes"}}`}, PluginState{}},
		"broken":        {map[string]string{"config/settings.json": `{`}, PluginState{}},
	} {
		root := t.TempDir()
		for rel, content := range c.files {
			writeDoc(t, filepath.Join(root, filepath.FromSlash(rel)), content)
		}
		if got := DetectPlugin(filepath.Join(root, "config"), filepath.Join(root, "project")); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	if LaunchLine("agent-1", "inst-1") != "CLAWBRIDGE_SESSION=agent-1 CLAWBRIDGE_INSTANCE=inst-1 claude --channels plugin:clawbridge@clawbridge" {
		t.Fatal(LaunchLine("agent-1", "inst-1"))
	}
}
