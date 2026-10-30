package channel

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"clawbridge/internal/ctxwatch"
)

// repoFile reads a file at the repository root's relative path.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// strictJSON decodes data into v, refusing trailing data.
func strictJSON(t *testing.T, name string, data []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if dec.More() {
		t.Fatalf("%s: data after the JSON object", name)
	}
}

// TestPluginFiles pins the marketplace and plugin the docs and the channel
// server rely on (FR26 D2): plugin clawbridge@clawbridge, one stdio server
// wake running `clawbridge channel serve`, and only the session and the
// protocol pin in its env.
func TestPluginFiles(t *testing.T) {
	var market struct {
		Name  string `json:"name"`
		Owner struct {
			Name string `json:"name"`
		} `json:"owner"`
		Plugins []struct {
			Name   string `json:"name"`
			Source any    `json:"source"`
		} `json:"plugins"`
	}
	strictJSON(t, "marketplace.json", repoFile(t, ".claude-plugin/marketplace.json"), &market)
	if market.Name != Marketplace || market.Owner.Name == "" || len(market.Plugins) != 1 {
		t.Fatalf("marketplace: %+v", market)
	}
	p := market.Plugins[0]
	if p.Name+"@"+market.Name != PluginID || "plugin:"+PluginID != PluginChannel {
		t.Fatalf("plugin %q in marketplace %q is not %s", p.Name, market.Name, PluginID)
	}
	source, ok := p.Source.(string)
	if !ok || source != "./plugins/clawbridge" {
		t.Fatalf("plugin source %#v", p.Source)
	}

	var plugin struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	strictJSON(t, "plugin.json", repoFile(t, source[2:]+"/.claude-plugin/plugin.json"), &plugin)
	if plugin.Name != p.Name || plugin.Description == "" {
		t.Fatalf("plugin.json: %+v", plugin)
	}

	var mcp struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	raw := repoFile(t, source[2:]+"/.mcp.json")
	strictJSON(t, ".mcp.json", raw, &mcp)
	var names []string
	for name := range mcp.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"wake"}) {
		t.Fatalf("servers %v, want only wake", names)
	}
	wake := mcp.MCPServers["wake"]
	if wake.Type != "" && wake.Type != "stdio" {
		t.Fatalf("wake type %q", wake.Type)
	}
	if wake.Command != "clawbridge" || !reflect.DeepEqual(wake.Args, []string{"channel", "serve"}) {
		t.Fatalf("wake runs %q %v", wake.Command, wake.Args)
	}
	want := map[string]string{"CLAWBRIDGE_SESSION": "${CLAWBRIDGE_SESSION:-}", "MCP_PROTOCOL_NEGOTIATION": "legacy"}
	if !reflect.DeepEqual(wake.Env, want) {
		t.Fatalf("wake env %v, want exactly %v", wake.Env, want)
	}
	// The session as Claude Code passes it: the value, empty, or (should
	// the default syntax ever go unexpanded) the literal, which is unset.
	for value, session := range map[string]string{"api-worker-2": "api-worker-2", "": "", wake.Env["CLAWBRIDGE_SESSION"]: ""} {
		if got := envValue(func(string) string { return value }, "CLAWBRIDGE_SESSION"); got != session {
			t.Fatalf("session %q read as %q", value, got)
		}
	}
	// The channel turn the agent sees names this server: the turn hook's
	// parser takes it.
	prompt := `<channel source="plugin:` + p.Name + `:wake" kind="wake" count="1" seq="7">`
	if seq, ok := ctxwatch.ChannelTurnSeq(prompt); !ok || seq != 7 {
		t.Fatalf("the turn hook doesn't take %s: %d %v", prompt, seq, ok)
	}
}

// The reason doctor matches for a session not launched with our channel
// names the plugin these files define.
func TestNotLaunchedReasonNamesThePlugin(t *testing.T) {
	if reasonNotLaunched != "not launched with --channels "+PluginChannel {
		t.Fatalf("%q", reasonNotLaunched)
	}
}
