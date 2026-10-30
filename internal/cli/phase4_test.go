package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/tui"
)

func TestPhase4AdapterPreflightHasNoSideEffects(t *testing.T) {
	isolated(t)
	for _, args := range [][]string{
		{"--client-config", "relative", "whoami"},
		{"--client-config", "/a", "--client-config", "/b", "whoami"},
		{"doctor", "--mcp-name", "clawbridge"},
		{"doctor", "--mcp-agent", "codex"},
		{"doctor", "--mcp-agent", "codex", "--mcp-config", "c.toml", "--mcp-session", "s", "--mcp-linear-profile", "bad value"},
		{"tui", "--screen", "invalid"},
		{"setup", "join", "--workspace", "w", "--project", "p"},
	} {
		var out, errs bytes.Buffer
		if err := Run(context.Background(), args, &out, &errs); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, args := range [][]string{{"tui", "--help"}, {"mcp", "install", "--help"}} {
		var out, errs bytes.Buffer
		if err := Run(context.Background(), args, &out, &errs); err != nil {
			t.Fatalf("help %v: %v", args, err)
		}
	}
}

func TestPhase4ClientConfigSelectorOverridesEnvironment(t *testing.T) {
	dir := isolated(t)
	selected := filepath.Join(dir, "selected.toml")
	if err := os.WriteFile(selected, []byte("mode='local'\nworkspace='selected'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWBRIDGE_CONFIG", filepath.Join(dir, "missing-other.toml"))
	c, path, err := resolveConfig(context.Background(), Config{ClientConfig: selected})
	if err != nil || path != selected || c.Workspace != "selected" {
		t.Fatalf("selector not pinned: %v", err)
	}
}

func TestPhase4InstallerInheritsCloudRouteAndPinsConfig(t *testing.T) {
	dir := isolated(t)
	path := filepath.Join(dir, "trusted.toml")
	raw := "mode='cloud'\nserver='https://service.example.test'\ncredential_source='device'\ndevice_id='dev_abcdefghjkmnpqrstvwxyz0123'\nkey_store='keyring'\nprincipal='human@example.test'\nworkspace='ws_join'\nproject='prj_join'\nsession='operator'\ninstance='operator-instance'\ncache='cache.db'\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	args := []string{"mcp", "install", "--client-config", path, "--session", "agent", "--agent", "claude-code", "--config", filepath.Join(dir, "agent.json"), "--dry-run"}
	if err := Run(context.Background(), args, &out, &errs); err != nil {
		t.Fatal(err)
	}
	value := out.String()
	// Compare decoded arguments: JSON escapes a Windows path's backslashes.
	var preview struct {
		Entry struct {
			Args []string `json:"args"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--client-config", path, "--credential-source", "device", "--session", "agent"} {
		if !slices.Contains(preview.Entry.Args, want) {
			t.Fatalf("missing safe route field %s", want)
		}
	}
	for _, bad := range []string{"operator-instance", "oauth"} {
		if strings.Contains(value, bad) {
			t.Fatal("private/operator data installed")
		}
	}
	out.Reset()
	if err := Run(context.Background(), []string{"mcp", "install", "--client-config", path, "--agent", "claude-code"}, &out, &errs); err == nil {
		t.Fatal("inherited operator actor")
	}
}

type phase4JoinProbe struct {
	resolve func() (string, string, error)
	execute func(bus.Request) (bus.Result, error)
}

func (p phase4JoinProbe) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return p.resolve()
}
func (p phase4JoinProbe) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	return p.execute(r)
}

func TestPhase4TUIUsesAdapterAndSelectedScreenWithoutProvisioning(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	oldCheck, oldRun := checkTUITerminal, runTUIProgram
	t.Cleanup(func() { checkTUITerminal, runTUIProgram = oldCheck, oldRun })
	checkTUITerminal = func(io.Reader, io.Writer) error { return nil }
	called := false
	runTUIProgram = func(ctx context.Context, m tui.Model, _ io.Reader, _ io.Writer) error {
		called = true
		if m.CurrentScreen() != tui.Cloud {
			t.Fatal("initial screen not selected")
		}
		if cmd := m.Init(); cmd == nil {
			t.Fatal("model not connected")
		}
		return nil
	}
	var out, errs bytes.Buffer
	if err := Run(context.Background(), []string{"tui", "--project", "p", "--screen", "cloud"}, &out, &errs); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("TUI adapter not invoked")
	}
}
