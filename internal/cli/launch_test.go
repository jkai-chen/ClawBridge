package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/launchcmd"
	"github.com/pelletier/go-toml/v2"
)

type launchExec struct{ who string }

func (f launchExec) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "whoami":
		return bus.Result{Data: map[string]any{"principal": f.who}}, nil
	case "list_sessions":
		return bus.Result{Data: map[string]any{"sessions": []map[string]any{
			{"name": "mine", "instance": "inst_m", "owner_principal": "mara@example.test", "host": "mara-mbp"},
			{"name": "theirs", "instance": "", "owner_principal": "kofi@example.test"},
		}}}, nil
	}
	return bus.Result{}, errors.New("unexpected " + r.Operation)
}

func fakeLaunchSource(t *testing.T) (launchcmd.Source, *[]string) {
	t.Helper()
	var ensured []string
	return launchcmd.Source{Exec: launchExec{who: "mara@example.test"}, StateDir: "/st",
		Prepare: func(_ context.Context, agent, session, _, _ string) (launchcmd.Prep, error) {
			ensured = append(ensured, agent+"|"+session)
			return launchcmd.Prep{CodexCommand: "/bin/clawbridge", CodexArgs: []string{"mcp", "--session", session}, Warnings: []string{"w1"}}, nil
		}}, &ensured
}

func TestLaunchCommandPrintsOwnAndRefusesOthers(t *testing.T) {
	src, ensured := fakeLaunchSource(t)
	var out, errOut bytes.Buffer
	if err := printLaunch(context.Background(), src, "mine", "claude-code", false, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != "CLAWBRIDGE_SESSION=mine CLAWBRIDGE_INSTANCE=inst_m claude --mcp-config /st/mcp/mine.json\n" {
		t.Fatalf("out %q", out.String())
	}
	out.Reset()
	err := printLaunch(context.Background(), src, "theirs", "claude-code", false, &out, &errOut)
	if err == nil || err.Error() != "only kofi@example.test can start this agent" || out.Len() != 0 {
		t.Fatalf("err %v out %q", err, out.String())
	}
	if len(*ensured) != 1 {
		t.Fatalf("a refused session wrote config: %v", *ensured)
	}
}

func TestLaunchCommandJSON(t *testing.T) {
	src, _ := fakeLaunchSource(t)
	var out bytes.Buffer
	if err := printLaunch(context.Background(), src, "mine", "codex", true, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Source string
		Data   launchcmd.Line
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != "launch-command" || !strings.HasPrefix(got.Data.Line, "CLAWBRIDGE_SESSION=mine CLAWBRIDGE_INSTANCE=inst_m codex -c ") || len(got.Data.Warnings) != 1 || got.Data.Host != "mara-mbp" {
		t.Fatalf("%+v", got)
	}
}

func TestLaunchCommandNeedsASession(t *testing.T) {
	isolated(t)
	err := Run(context.Background(), []string{"launch-command"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--session") {
		t.Fatal(err)
	}
}

func runOut(t *testing.T, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), args, &out, &errOut); err != nil {
		t.Fatalf("%v: %v (%s)", args, err, errOut.String())
	}
	return out.String()
}

func launchLine(t *testing.T, args ...string) launchcmd.Line {
	t.Helper()
	var got struct{ Data launchcmd.Line }
	if err := json.Unmarshal([]byte(runOut(t, append([]string{"launch-command", "--json"}, args...)...)), &got); err != nil {
		t.Fatal(err)
	}
	return got.Data
}

// A real local store: the session is the OS user's own, so the line is
// given, its MCP file is written once, and Codex is told to install its entry.
func TestLaunchCommandLocalWritesThePerSessionFileOnce(t *testing.T) {
	localUIProject(t)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	t.Setenv("CLAUDECODE", "")
	reg := runOut(t, "--session", "api.worker", "--json", "register", "--agent-kind", "claude-code", "--role", "engineer")
	var r struct{ Data struct{ Instance string } }
	if err := json.Unmarshal([]byte(reg), &r); err != nil || r.Data.Instance == "" {
		t.Fatalf("register: %v %s", err, reg)
	}
	home, _ := stateHome()
	path := launchcmd.ConfigPath(home, "api.worker")
	if strings.Contains(path, "api.worker") {
		t.Fatal(path)
	}

	l := launchLine(t, "--session", "api.worker")
	want := "CLAWBRIDGE_SESSION=api.worker CLAWBRIDGE_INSTANCE=" + r.Data.Instance + " claude --mcp-config " + launchcmd.Quote(path)
	if l.Line != want {
		t.Fatalf("got  %s\nwant %s", l.Line, want)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err = json.Unmarshal(first, &cfg); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cfg.MCPServers["clawbridge"].Args, " ")
	if !strings.Contains(args, "--session api.worker") || !strings.Contains(args, "--instance "+r.Data.Instance) {
		t.Fatalf("entry args %q", args)
	}
	for _, secret := range []string{"CLAWBRIDGE_DEVICE_KEY", "TOKEN", "SECRET"} {
		if strings.Contains(string(first), secret) {
			t.Fatalf("the file names %s", secret)
		}
	}
	// No hooks beside the per-session file.
	if _, err = os.Stat(filepath.Join(filepath.Dir(path), ".claude")); err == nil {
		t.Fatal("hooks were installed")
	}
	launchLine(t, "--session", "api.worker")
	if again, _ := os.ReadFile(path); !bytes.Equal(again, first) {
		t.Fatal("a repeat rewrote the file")
	}

	// A stale file (another instance) is written again.
	stale := strings.ReplaceAll(string(first), r.Data.Instance, "inst_old")
	if err = os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	launchLine(t, "--session", "api.worker")
	if fixed, _ := os.ReadFile(path); !bytes.Equal(fixed, first) {
		t.Fatalf("stale file not rewritten: %s", fixed)
	}

	// Codex: nothing is written to its config; the server is on the line.
	codexHome := os.Getenv("CODEX_HOME")
	c := launchLine(t, "--session", "api.worker", "--agent", "codex")
	if _, err = os.Stat(filepath.Join(codexHome, "config.toml")); err == nil {
		t.Fatal("the Codex config was written")
	}
	if !strings.HasPrefix(c.Line, "CLAWBRIDGE_SESSION=api.worker CLAWBRIDGE_INSTANCE="+r.Data.Instance+" codex -c 'mcp_servers.clawbridge.command=") || len(c.Warnings) != 0 {
		t.Fatalf("%+v", c)
	}
	// The override is what `mcp install --agent codex` writes (same args).
	if !strings.Contains(c.Line, `"--session", "api.worker"`) || !strings.Contains(c.Line, `"--instance", "`+r.Data.Instance+`"`) {
		t.Fatalf("args differ from mcp install's: %s", c.Line)
	}
}

// B's line yields exactly one clawbridge server, as B, even when the Codex
// config already has an entry for A under the same name, and other
// clawbridge entries are reported.
func TestLaunchCommandCodexLineReplacesTheConfigsEntryAndWarnsAboutOthers(t *testing.T) {
	localUIProject(t)
	codexHome := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "[mcp_servers.clawbridge]\ncommand = \"/old/clawbridge\"\nargs = [\"mcp\", \"--session\", \"sess-a\"]\n" +
		"[mcp_servers.sess-a]\ncommand = \"/old/bin/clawbridge\"\nargs = [\"mcp\", \"--session\", \"sess-a\"]\n" +
		"[mcp_servers.unrelated]\ncommand = \"/bin/true\"\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	runOut(t, "--session", "sess-b", "--json", "register", "--agent-kind", "codex", "--role", "engineer")
	l := launchLine(t, "--session", "sess-b", "--agent", "codex")
	if len(l.Warnings) != 1 || l.Warnings[0] != "Your Codex config also starts ClawBridge as sess-a; remove it so this agent acts only as sess-b." {
		t.Fatalf("warnings %v", l.Warnings)
	}
	// Apply the line's -c overrides to the config the way Codex does (the
	// same dotted key replaces that entry's command and args) and count.
	if runtime.GOOS == "windows" {
		return
	}
	script := "codex() { for a in \"$@\"; do printf '%s\\0' \"$a\"; done; }; " + l.Line
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var over struct {
		Mcp_servers map[string]struct {
			Command string
			Args    []string
		}
	}
	if err = toml.Unmarshal([]byte(args[1]+"\n"+args[3]), &over); err != nil {
		t.Fatal(err)
	}
	var base struct {
		Mcp_servers map[string]struct {
			Command string
			Args    []string
		}
	}
	if err = toml.Unmarshal([]byte(cfg), &base); err != nil {
		t.Fatal(err)
	}
	base.Mcp_servers["clawbridge"] = over.Mcp_servers["clawbridge"]
	count, asB := 0, 0
	for name, srv := range base.Mcp_servers {
		if name == "unrelated" {
			continue
		}
		if name == "clawbridge" {
			count++
			if strings.Join(srv.Args, " ") != "" && strings.Contains(strings.Join(srv.Args, " "), "--session sess-b") {
				asB++
			}
		}
	}
	if count != 1 || asB != 1 {
		t.Fatalf("clawbridge servers %d, as sess-b %d: %+v", count, asB, base.Mcp_servers)
	}
}
