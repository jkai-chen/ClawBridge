package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"clawbridge/internal/channel"
	"clawbridge/internal/ctxwatch"
)

// channelMachine points the managed settings at a temporary root and
// Claude Code's settings at a fake folder, and fakes `claude`, recording
// what would have run.
func channelMachine(t *testing.T) (managed, config string, ran *[]string) {
	t.Helper()
	dir := isolated(t)
	managed, config = filepath.Join(dir, "managed", "ClaudeCode"), filepath.Join(dir, "claude-config")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	oldDir, oldGOOS, oldClaude := channelManagedDir, channelGOOS, channelClaude
	channelManagedDir = func() string { return managed }
	channelGOOS = "linux"
	ran = &[]string{}
	channelClaude = func(_ context.Context, args []string, _, _ io.Writer) error {
		*ran = append(*ran, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { channelManagedDir, channelGOOS, channelClaude = oldDir, oldGOOS, oldClaude })
	// A person's terminal, whatever runs the tests.
	typing(t, false, "")
	return managed, config, ran
}

func channelRun(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	err := Run(context.Background(), args, &out, &errs)
	return out.String(), errs.String(), err
}

func allowText(path string) string {
	return "The admin step that lets Claude Code take ClawBridge's channel on this machine (FR26).\n" +
		"Drop-in: " + path + "\n\n" + channel.AllowJSON + "\n" +
		"Warnings:\n" +
		"- It is machine-wide: it applies to every Claude Code user on this machine.\n" +
		"- allowedChannelPlugins replaces Anthropic's default channel list when no other managed list exists, so it also lists Anthropic's four official channel plugins (discord, telegram, fakechat, imessage).\n" +
		"- Anthropic may change its default list later; this file won't follow.\n" +
		"- To undo it, delete " + path + "\n"
}

// FR26: `channel allow` prints the drop-in and its warnings; --write writes
// it only where this process may, and replaces a different one only with
// --force.
func TestChannelAllow(t *testing.T) {
	managed, _, _ := channelMachine(t)
	path := filepath.Join(managed, "managed-settings.d", channel.AllowFileName)
	out, _, err := channelRun(t, "channel", "allow")
	want := allowText(path) + "To write it: sudo \"$(command -v clawbridge)\" channel allow --write\n"
	if err != nil || out != want {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err = os.Stat(path); err == nil {
		t.Fatal("printing wrote the file")
	}
	channelGOOS = "windows"
	if out, _, _ = channelRun(t, "channel", "allow"); !strings.HasSuffix(out, "To write it, in a terminal run as administrator: clawbridge channel allow --write\n") {
		t.Fatal(out)
	}
	channelGOOS = "linux"
	// An agent may show the step, never write it.
	typing(t, true, "yes\n", "CLAUDECODE=1")
	if out, _, err = channelRun(t, "channel", "allow"); err != nil || out != want {
		t.Fatal(err, out)
	}
	if _, _, err = channelRun(t, "channel", "allow", "--write"); err == nil || !strings.Contains(err.Error(), "CLAUDECODE") {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err == nil {
		t.Fatal("an agent wrote the drop-in")
	}
	typing(t, false, "")
	if _, _, err = channelRun(t, "channel", "allow", "--force"); err == nil {
		t.Fatal("--force without --write")
	}
	if _, _, err = channelRun(t, "channel", "allow", "--bogus"); err == nil {
		t.Fatal("an unknown flag")
	}
	// sudo drops the agent markers: without a terminal --write is refused
	// all the same, with --force too, and nothing is asked.
	for _, args := range [][]string{{"--write"}, {"--write", "--force"}} {
		typing(t, false, "yes\n")
		out, _, err = channelRun(t, append([]string{"channel", "allow"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "not a terminal") || strings.Contains(out, "Type yes") {
			t.Fatal(args, err, out)
		}
		if _, err = os.Stat(path); err == nil {
			t.Fatal("written without a terminal")
		}
	}
	// At a terminal, only yes writes.
	for _, answer := range []string{"", "y\n", "no\n", "Yes please\n"} {
		typing(t, true, answer)
		out, _, err = channelRun(t, "channel", "allow", "--write")
		if err == nil || !strings.Contains(err.Error(), "nothing written") || !strings.Contains(out, "machine-wide: every Claude Code user") {
			t.Fatalf("%q: %v\n%s", answer, err, out)
		}
		if _, err = os.Stat(path); err == nil {
			t.Fatalf("written on %q", answer)
		}
	}
	confirm := "This writes " + path + ", machine-wide: every Claude Code user on this machine gets it.\nType yes to write it: "
	typing(t, true, "yes\n")
	out, _, err = channelRun(t, "channel", "allow", "--write")
	if err != nil || out != allowText(path)+confirm+"Written: "+path+"\n" {
		t.Fatalf("%v\n%s", err, out)
	}
	if data, _ := os.ReadFile(path); string(data) != channel.AllowJSON {
		t.Fatalf("%s", data)
	}
	typing(t, true, "yes\n")
	if out, _, err = channelRun(t, "channel", "allow", "--write"); err != nil || !strings.HasSuffix(out, path+" is already in place; nothing written.\n") {
		t.Fatal(err, out)
	}
	if err = os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	typing(t, true, "yes\n")
	out, _, err = channelRun(t, "channel", "allow", "--write")
	if err == nil || !strings.Contains(err.Error(), "--force") || !strings.Contains(out, "- {}") || !strings.Contains(out, "+ {") {
		t.Fatal(err, out)
	}
	if data, _ := os.ReadFile(path); string(data) != "{}\n" {
		t.Fatal("replaced without --force")
	}
	// --force doesn't skip the question.
	typing(t, true, "no\n")
	if _, _, err = channelRun(t, "channel", "allow", "--write", "--force"); err == nil || !strings.Contains(err.Error(), "nothing written") {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "{}\n" {
		t.Fatal("--force replaced it without a yes")
	}
	typing(t, true, "yes\n")
	if out, _, err = channelRun(t, "channel", "allow", "--write", "--force"); err != nil || !strings.HasSuffix(out, "Written: "+path+"\n") {
		t.Fatal(err, out)
	}
	if data, _ := os.ReadFile(path); string(data) != channel.AllowJSON {
		t.Fatalf("%s", data)
	}
	if ok, where, _ := channel.AllowlistedIn(managed); !ok || where != path {
		t.Fatal(ok, where)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err = os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	channelManagedDir = func() string { return filepath.Join(locked, "ClaudeCode") }
	typing(t, true, "yes\n")
	if _, _, err = channelRun(t, "channel", "allow", "--write"); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatal(err)
	}
}

// `channel setup` is a person's: refused from an agent's shell or with
// --session; without a terminal it only prints the commands; at one it
// asks, and runs them only on yes.
func TestChannelSetup(t *testing.T) {
	_, config, ran := channelMachine(t)
	launch := "CLAWBRIDGE_SESSION=<session> CLAWBRIDGE_INSTANCE=<instance> claude --channels plugin:clawbridge@clawbridge"
	typing(t, false, "", "CLAWBRIDGE_SESSION=agent")
	if _, _, err := channelRun(t, "channel", "setup"); err == nil || !strings.Contains(err.Error(), "CLAWBRIDGE_SESSION") {
		t.Fatal(err)
	}
	typing(t, true, "y\n")
	for _, args := range [][]string{{"channel", "setup", "--session", "agent"}, {"channel", "setup", "--session=agent"}, {"--session", "agent", "channel", "setup"}} {
		if _, _, err := channelRun(t, args...); err == nil || !strings.Contains(err.Error(), "--session") {
			t.Fatal(args, err)
		}
	}
	if len(*ran) != 0 {
		t.Fatal(*ran)
	}
	// No terminal: printed only.
	typing(t, false, "y\n")
	out, _, err := channelRun(t, "channel", "setup")
	if err != nil || len(*ran) != 0 || !strings.Contains(out, "  claude plugin marketplace add jkai-chen/ClawBridge\n  claude plugin install clawbridge@clawbridge --scope user\n") ||
		!strings.Contains(out, "not a terminal") || !strings.Contains(out, "clawbridge channel allow") || !strings.HasSuffix(out, launch+"\n") {
		t.Fatal(err, out)
	}
	// A terminal, answered no.
	typing(t, true, "\n")
	if out, _, err = channelRun(t, "channel", "setup"); err != nil || len(*ran) != 0 || !strings.Contains(out, "[y/N]") {
		t.Fatal(err, out, *ran)
	}
	typing(t, true, "y\n")
	if out, _, err = channelRun(t, "channel", "setup"); err != nil || strings.Join(*ran, "|") != "plugin marketplace add jkai-chen/ClawBridge|plugin install clawbridge@clawbridge --scope user" {
		t.Fatal(err, out, *ran)
	}
	// A failing command stops the rest.
	*ran = nil
	channelClaude = func(_ context.Context, args []string, _, _ io.Writer) error {
		*ran = append(*ran, strings.Join(args, " "))
		return errors.New("exit status 1")
	}
	typing(t, true, "yes\n")
	if _, _, err = channelRun(t, "channel", "setup"); err == nil || len(*ran) != 1 {
		t.Fatal(err, *ran)
	}
	// Set up already: nothing to run, nothing asked.
	writeSettings(t, filepath.Join(config, "settings.json"), `{"enabledPlugins":{"clawbridge@clawbridge":true}}`)
	writeSettings(t, filepath.Join(config, "plugins", "known_marketplaces.json"), `{"clawbridge":{}}`)
	*ran = nil
	typing(t, true, "y\n")
	if out, _, err = channelRun(t, "channel", "setup"); err != nil || len(*ran) != 0 || strings.Contains(out, "[y/N]") || !strings.Contains(out, "installed and enabled") {
		t.Fatal(err, out, *ran)
	}
}

func writeSettings(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mcp install --channel adds the turn hook beside the others, leaves the
// MCP entry exactly as without it, and lists the setup steps still to do,
// read only; Codex is refused.
func TestMCPInstallChannel(t *testing.T) {
	managed, config, ran := channelMachine(t)
	dir := filepath.Dir(filepath.Dir(managed))
	install := func(project string, extra ...string) (string, string, error) {
		args := append([]string{"--session", "agent", "--instance", "inst-1", "--workspace", "w", "--project", "p", "mcp", "install", "--agent", "claude-code", "--config", filepath.Join(dir, project, ".mcp.json")}, extra...)
		return channelRun(t, args...)
	}
	_, errs, err := install("plain")
	if err != nil || strings.Contains(errs, "channel") {
		t.Fatal(err, errs)
	}
	_, errs, err = install("chan", "--channel")
	if err != nil {
		t.Fatal(err, errs)
	}
	plain, _ := os.ReadFile(filepath.Join(dir, "plain", ".mcp.json"))
	chan_, _ := os.ReadFile(filepath.Join(dir, "chan", ".mcp.json"))
	if len(plain) == 0 || !bytes.Equal(plain, chan_) {
		t.Fatalf("the entry changed:\n%s\n%s", plain, chan_)
	}
	settings, _ := os.ReadFile(filepath.Join(dir, "chan", ".claude", "settings.local.json"))
	plainSettings, _ := os.ReadFile(filepath.Join(dir, "plain", ".claude", "settings.local.json"))
	turn, marker := "hook turn --from-hook", ctxwatch.HookMarker
	if runtime.GOOS == "windows" {
		turn, marker = `"turn"`, ctxwatch.HookTag
	}
	if strings.Count(string(settings), marker) != 4 || !strings.Contains(string(settings), turn) || strings.Contains(string(plainSettings), turn) {
		t.Fatalf("%s\n%s", settings, plainSettings)
	}
	want := "Channel setup still to do (FR26):\n" +
		"  claude plugin marketplace add jkai-chen/ClawBridge\n" +
		"  claude plugin install clawbridge@clawbridge --scope user\n" +
		"  the admin step, once per machine: clawbridge channel allow\n" +
		"Then start the agent with:\n" +
		"  CLAWBRIDGE_SESSION=agent CLAWBRIDGE_INSTANCE=inst-1 claude --channels plugin:clawbridge@clawbridge\n"
	if !strings.HasSuffix(errs, want) {
		t.Fatalf("%q", errs)
	}
	// Set up: only the launch line is left.
	writeSettings(t, filepath.Join(config, "settings.json"), `{"enabledPlugins":{"clawbridge@clawbridge":true},"extraKnownMarketplaces":{"clawbridge":{}}}`)
	if _, err = channel.WriteAllow(managed, false); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(config, "settings.json"))
	_, errs, err = install("chan", "--channel")
	if err != nil || !strings.HasSuffix(errs, "Channel setup (FR26): the plugin is installed and enabled, and this machine allowlists it.\n"+
		"Then start the agent with:\n  CLAWBRIDGE_SESSION=agent CLAWBRIDGE_INSTANCE=inst-1 claude --channels plugin:clawbridge@clawbridge\n") {
		t.Fatalf("%v %q", err, errs)
	}
	if after, _ := os.ReadFile(filepath.Join(config, "settings.json")); !bytes.Equal(before, after) || len(*ran) != 0 {
		t.Fatal("Claude Code's settings were edited, or claude was run")
	}
	// The plugin enabled only in another project's settings counts for that
	// project alone.
	writeSettings(t, filepath.Join(config, "settings.json"), `{"extraKnownMarketplaces":{"clawbridge":{}}}`)
	writeSettings(t, filepath.Join(dir, "chan", ".claude", "settings.json"), `{"enabledPlugins":{"clawbridge@clawbridge":true}}`)
	if _, errs, _ = install("chan", "--channel"); strings.Contains(errs, "plugin install") {
		t.Fatalf("%q", errs)
	}
	if _, errs, _ = install("other", "--channel"); !strings.Contains(errs, "plugin install") {
		t.Fatalf("%q", errs)
	}
	codex := []string{"--session", "agent", "--workspace", "w", "--project", "p", "mcp", "install", "--agent", "codex", "--config", filepath.Join(dir, "codex.toml"), "--channel"}
	if _, _, err = channelRun(t, codex...); err == nil || err.Error() != "Codex has no channels; it keeps the notify adapter" {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "codex.toml")); err == nil {
		t.Fatal("the refused install wrote the Codex entry")
	}
	if _, _, err = install("none", "--channel", "--hooks", "none"); err == nil || !strings.Contains(err.Error(), "--hooks none") {
		t.Fatal(err)
	}
}

// context hooks install --channel adds the turn hook; uninstall removes it.
func TestContextHooksChannel(t *testing.T) {
	dir := isolated(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	settings := filepath.Join(dir, "claude", "settings.json")
	if _, _, err := channelRun(t, "context", "hooks", "install", "--scope", "user"); err != nil {
		t.Fatal(err)
	}
	turn := "hook turn"
	if runtime.GOOS == "windows" {
		turn = `"turn"`
	}
	if data, _ := os.ReadFile(settings); strings.Contains(string(data), turn) {
		t.Fatalf("%s", data)
	}
	if _, _, err := channelRun(t, "context", "hooks", "install", "--scope", "user", "--channel"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(settings); !strings.Contains(string(data), turn) || !strings.Contains(string(data), "UserPromptSubmit") {
		t.Fatalf("%s", data)
	}
	if _, _, err := channelRun(t, "context", "hooks", "uninstall", "--scope", "user"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(settings); strings.Contains(string(data), turn) || strings.Contains(string(data), "UserPromptSubmit") {
		t.Fatalf("%s", data)
	}
}

// doctor's channel line reads, and only reads, the session's channel
// record, the project's turn hook, the plugin's settings and the
// machine's allowlist.
func TestDoctorChannelReads(t *testing.T) {
	managed, config, _ := channelMachine(t)
	project, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	st := doctorChannel("agent")
	if st.Asked || st.Marketplace || st.Enabled || st.Allowlisted || st.Record != nil || st.Err != nil || st.AllowErr != nil || !strings.HasSuffix(st.Path, "agent.channel.json") {
		t.Fatalf("nothing set up: %+v", st)
	}
	if err = applyHooksWith(ctxwatch.ScopeLocal, project, true, false, false, true, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(config, "settings.json"), []byte(`{"enabledPlugins":{"clawbridge@clawbridge":true},"extraKnownMarketplaces":{"clawbridge":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = channel.WriteAllow(managed, false); err != nil {
		t.Fatal(err)
	}
	dir, _ := contextDir()
	if err = ctxwatch.SaveChannelState(dir, "agent", ctxwatch.ChannelState{PID: os.Getpid(), ParentPID: 1, State: ctxwatch.ChannelUnconfirmed, Reason: ctxwatch.ChannelReasonUnconfirmed, UpdatedAt: "2026-10-30T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	st = doctorChannel("agent")
	if !st.Asked || !st.Marketplace || !st.Enabled || !st.Allowlisted || st.Record == nil || st.Record.State != ctxwatch.ChannelUnconfirmed || st.Err != nil {
		t.Fatalf("all set up: %+v", st)
	}
	if other := doctorChannel("other"); other.Record != nil || !other.Asked {
		t.Fatalf("another session: %+v", other)
	}
}
