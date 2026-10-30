package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/faketool"
)

// The wake path an agent's mail takes, driven through the real binary as
// Claude Code would drive it on this OS: mcp install writes the MCP entry
// and the hooks; the MCP server is started from that entry as the agent's
// child, so it writes the queue record for the agent's process; the Stop
// hook runs exactly as installed (the exec form's argv on Windows, the
// shell text through sh elsewhere) and holds back a stop while mail waits;
// the command notifier runs its program once per batch. Windows CI runs it
// as its own step.
const (
	wakeAgent  = "wake-agent"
	wakeSender = "wake-sender"
	wakeTag    = "clawbridge-context-hook-v1"
	wakeMarker = "# clawbridge-context-hook v1"
	// The fixed texts the agent and its terminal may see (M5, M6).
	wakeStopReason = "1 ClawBridge message is waiting: call inbox, handle it, then ack"
	wakeNotice     = "1 ClawBridge message is waiting; call inbox"
)

func TestWakePathEndToEnd(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "clawbridge")
	if runtime.GOOS == "windows" {
		// The exec-form hook names an .exe.
		binary += ".exe"
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// A cold build on a Windows on ARM runner is slow.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/clawbridge")
	build.Dir = root
	output, err := build.CombinedOutput()
	cancel()
	if err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	if binary, err = filepath.EvalSymlinks(binary); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	claudeConfig := filepath.Join(dir, "claude-config")
	work := filepath.Join(dir, "work")
	for _, d := range []string{claudeConfig, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Only this test's settings reach the processes: its own state, Claude
	// Code config and, outside Windows, home. CLAWBRIDGE_SESSION is unset,
	// as in an agent started without it: the hook finds the record by the
	// agent's process.
	env := []string{}
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(strings.ToUpper(e), "=")
		if strings.HasPrefix(name, "CLAWBRIDGE_") || name == "CLAUDE_CONFIG_DIR" || (name == "HOME" && runtime.GOOS != "windows") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, "CLAWBRIDGE_HOME="+state, "CLAUDE_CONFIG_DIR="+claudeConfig)
	if runtime.GOOS != "windows" {
		env = append(env, "HOME="+filepath.Join(dir, "home"))
	}
	p := &processFixture{t: t, binary: binary, dir: work, db: filepath.Join(dir, "bus.db"), env: env}
	p.seed()
	p.run(cliActor(wakeSender, "register")...)

	// 1. mcp install writes the MCP entry and the hooks, in this OS's form.
	mcpJSON := filepath.Join(work, ".mcp.json")
	install := p.command("--session", wakeAgent, "mcp", "install", "--agent", "claude-code", "--config", mcpJSON)
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("mcp install: %v\n%s", err, output)
	}
	var mcpConfig struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	readJSON(t, mcpJSON, &mcpConfig)
	entry, ok := mcpConfig.MCPServers["clawbridge"]
	if !ok || !sameFile(entry.Command, binary) || !slices.Contains(entry.Args, wakeAgent) {
		t.Fatalf("the MCP entry does not run this binary for %s: %+v", wakeAgent, mcpConfig)
	}
	stop := wakeHooks(t, filepath.Join(work, ".claude", "settings.local.json"), binary)

	// 2. The MCP server, started from its entry as the agent's child,
	// registers and writes the record the hooks act on.
	server := startWakeServer(t, entry.Command, entry.Args, work, env)
	initializeRPC(t, server.rpcProcess)
	registered := mcpData(t, rpcSuccess(t, server.call("register_session", map[string]any{"role": "worker", "agent_kind": "claude-code"})))
	instance := stringField(t, registered, "instance")
	recordPath := filepath.Join(state, "context", wakeAgent+".queue.json")
	var record struct {
		Session  string `json:"session"`
		Instance string `json:"instance"`
		DB       string `json:"db"`
		MCPPID   int    `json:"mcp_pid"`
		AgentPID int    `json:"agent_pid"`
	}
	waitFor(t, 20*time.Second, "the MCP server's queue record", func() bool {
		data, err := os.ReadFile(recordPath)
		return err == nil && json.Unmarshal(data, &record) == nil && record.Instance == instance
	})
	if record.Session != wakeAgent || record.MCPPID != server.cmd.Process.Pid || record.AgentPID != os.Getpid() || record.DB != p.db {
		t.Fatalf("queue record %+v: want the server %d of agent %d on %s", record, server.cmd.Process.Pid, os.Getpid(), p.db)
	}

	transcript := filepath.Join(claudeConfig, "projects", "-wake-work", "00000000-0000-4000-8000-000000000001.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hook := func(active bool) string {
		t.Helper()
		return runStopHook(t, stop, work, env, transcript, active)
	}
	allows := func(when string, active bool) {
		t.Helper()
		if out := hook(active); out != "" {
			t.Fatalf("%s: the Stop hook wrote %q, want it to allow the stop silently", when, out)
		}
	}
	blocks := func(when, body string) {
		t.Helper()
		out := hook(false)
		var decision struct{ Decision, Reason string }
		if err := json.Unmarshal([]byte(out), &decision); err != nil || decision.Decision != "block" || decision.Reason != wakeStopReason {
			t.Fatalf("%s: the Stop hook wrote %q, want a block with %q", when, out, wakeStopReason)
		}
		if strings.Contains(out, body) || strings.Contains(out, wakeSender) {
			t.Fatalf("%s: the Stop hook's output carries the message: %q", when, out)
		}
	}
	agent := func(args ...string) any {
		t.Helper()
		return p.run(append([]string{"--session", wakeAgent, "--instance", instance}, args...)...)
	}
	ack := func(body string) {
		t.Helper()
		for _, m := range pageItems(t, "messages", agent("inbox")) {
			if object(t, m)["body"] == body {
				agent("mark-read", stringField(t, m, "id"))
				return
			}
		}
		t.Fatalf("%q is not in %s's inbox", body, wakeAgent)
	}

	allows("no mail", false)
	const first = "first private body for the wake test"
	p.run(cliActor(wakeSender, "send", wakeAgent, first, "--operation-id", "wake-first")...)
	blocks("an unacked message", first)
	// Claude Code's next Stop continues from the block; the same message
	// never blocks twice.
	allows("stop_hook_active", true)
	allows("the same message again", false)
	ack(first)
	allows("after the ack", false)
	// A new message blocks again, counted alone: the ack above was seen.
	const second = "second private body for the wake test"
	p.run(cliActor(wakeSender, "send", wakeAgent, second, "--operation-id", "wake-second")...)
	blocks("a second message after the first was acked", second)
	ack(second)
	allows("after the second ack", false)

	// 3. The command notifier runs its program once for a new batch.
	log := filepath.Join(t.TempDir(), "notifier-runs")
	t.Setenv(faketool.LogEnv, log)
	fake := faketool.Install(t, t.TempDir(), "fake wake", "append")
	notifyState := filepath.Join(t.TempDir(), "command-notify.json")
	notifier := p.command("--session", wakeAgent, "--instance", instance, "notify", "--adapter", "command", "--state", notifyState, "--interval", "1s", "--", fake, "--text", "{text}")
	notifier.Env = append(slices.Clip(env), "CLAWBRIDGE_FAKE_TOOL=append", faketool.LogEnv+"="+log)
	// A file, read only by name, so nothing races the process writing it.
	notifierLog := filepath.Join(t.TempDir(), "notifier-output")
	out, err := os.Create(notifierLog)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	notifier.Stdout, notifier.Stderr = out, out
	if err := notifier.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := stopOnCleanup(t, notifier)
	// Its first poll pins the session in the checkpoint; then the mail.
	waitFor(t, 20*time.Second, "the notifier's checkpoint", func() bool {
		data, err := os.ReadFile(notifyState)
		return err == nil && bytes.Contains(data, []byte("session"))
	})
	runs, err := faketool.ReadRuns(log)
	if err != nil || len(runs) != 0 {
		t.Fatalf("the notifier ran with no new mail: %+v %v", runs, err)
	}
	const third = "third private body for the notifier"
	p.run(cliActor(wakeSender, "send", wakeAgent, third, "--operation-id", "wake-third")...)
	waitFor(t, 30*time.Second, "the notifier's command", func() bool {
		runs, err = faketool.ReadRuns(log)
		return err == nil && len(runs) > 0
	})
	// Several polls later it has still run once for this batch.
	time.Sleep(4 * time.Second)
	if runs, err = faketool.ReadRuns(log); err != nil || len(runs) != 1 {
		said, _ := os.ReadFile(notifierLog)
		t.Fatalf("the notifier ran %d times for one message, want once: %+v %v\n%s", len(runs), runs, err, said)
	}
	got := runs[0]
	if !slices.Equal(got.Args, []string{"--text", wakeNotice}) || got.NoticeText != wakeNotice {
		t.Fatalf("the notifier's command got %q (notice %q), want --text %q", got.Args, got.NoticeText, wakeNotice)
	}
	for _, leak := range []string{third, first, second, wakeSender} {
		if strings.Contains(strings.Join(got.Args, " ")+got.Stdin+got.NoticeText, leak) {
			t.Fatalf("the notifier's command was given %q: %+v", leak, got)
		}
	}
	stopped()

	// The queue record is private to the user. On Windows that is its ACL,
	// which ctxwatch's own Windows tests check (FR15-F1); here only the POSIX
	// mode is asserted.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(recordPath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("queue record mode: %v %v", info, err)
		}
	}

	// The server ends with the agent's stdin. Its record may outlive it (the
	// process can exit before removing it), but a hook never acts on the
	// record of a server that is gone: mail waiting now changes nothing.
	server.close(t)
	p.run(cliActor(wakeSender, "send", wakeAgent, "mail after the server ended", "--operation-id", "wake-late")...)
	allows("the MCP server has ended", false)
}

// sameFile says whether a and b name the same file: a path written by the
// binary may differ in form (case, separators) from the one the test built.
func sameFile(a, b string) bool {
	x, err := os.Stat(a)
	if err != nil {
		return false
	}
	y, err := os.Stat(b)
	return err == nil && os.SameFile(x, y)
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, data)
	}
}

// wakeHook is one installed hook entry as Claude Code reads it.
type wakeHook struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

// wakeHooks checks the installed SessionStart, PreCompact and Stop hooks
// are this OS's form for binary, and returns the Stop hook.
func wakeHooks(t *testing.T, path, binary string) wakeHook {
	t.Helper()
	var settings struct {
		Hooks map[string][]struct {
			Hooks []wakeHook `json:"hooks"`
		} `json:"hooks"`
	}
	readJSON(t, path, &settings)
	var stop wakeHook
	for event, words := range map[string][]string{"SessionStart": {"context", "bind"}, "PreCompact": {"context", "compacting"}, "Stop": {"hook", "stop"}} {
		groups := settings.Hooks[event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s: want one ClawBridge hook, got %+v", event, groups)
		}
		h := groups[0].Hooks[0]
		if runtime.GOOS == "windows" {
			// The exec form: no shell, the .exe and its arguments.
			want := append(slices.Clip(words), "--from-hook", "--hook-tag", wakeTag)
			if h.Type != "command" || !filepath.IsAbs(h.Command) || !sameFile(h.Command, binary) || !slices.Equal(h.Args, want) {
				t.Fatalf("%s hook %+v, want the exec form %s %q", event, h, binary, want)
			}
		} else {
			q := "'" + binary + "'"
			want := "[ -x " + q + " ] || exit 0; CLAWBRIDGE_AGENT_PID=$PPID " + q + " " + strings.Join(words, " ") + " --from-hook " + wakeMarker
			if h.Type != "command" || h.Command != want || h.Args != nil {
				t.Fatalf("%s hook %+v, want the shell form %q", event, h, want)
			}
		}
		if h.Timeout <= 0 {
			t.Fatalf("%s hook has no timeout: %+v", event, h)
		}
		if event == "Stop" {
			stop = h
		}
	}
	return stop
}

// runStopHook runs the Stop hook as Claude Code does, from this process (the
// agent): the exec form's argv directly, the shell form through sh -c. It
// returns the hook's standard output; a hook that fails or outlasts its
// timeout fails the test.
func runStopHook(t *testing.T, h wakeHook, dir string, env []string, transcript string, active bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(h.Timeout)*time.Second+20*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if h.Args != nil {
		cmd = exec.CommandContext(ctx, h.Command, h.Args...)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", h.Command)
	}
	cmd.Dir, cmd.Env = dir, env
	input, _ := json.Marshal(map[string]any{"session_id": "00000000-0000-4000-8000-000000000001", "transcript_path": transcript, "cwd": dir,
		"permission_mode": "default", "hook_event_name": "Stop", "stop_hook_active": active})
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the Stop hook failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if stderr.Len() > 0 {
		t.Fatalf("the Stop hook wrote to stderr: %s", stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// wakeServer is the MCP server started from its installed entry.
type wakeServer struct {
	*rpcProcess
	input  interface{ Close() error }
	done   chan error
	cancel context.CancelFunc
}

func startWakeServer(t *testing.T, command string, args []string, dir string, env []string) *wakeServer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir, cmd.Env = dir, env
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := new(bytes.Buffer)
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &wakeServer{rpcProcess: &rpcProcess{t: t, cmd: cmd, in: json.NewEncoder(input), lines: make(chan []byte, 100), stderr: stderr}, input: input, done: make(chan error, 1), cancel: cancel}
	go func() {
		defer close(s.lines)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		for scanner.Scan() {
			s.lines <- append([]byte(nil), scanner.Bytes()...)
		}
		s.done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		input.Close()
		_ = cmd.Process.Kill()
		<-s.done
		cancel()
	})
	return s
}

// close ends the server as an agent that exits does: its stdin closes.
func (s *wakeServer) close(t *testing.T) {
	t.Helper()
	s.input.Close()
	select {
	case <-s.done:
		s.done <- nil
	case <-time.After(20 * time.Second):
		t.Fatal("the MCP server did not end with its stdin")
	}
}

// stopOnCleanup stops cmd by its handle at the latest when the test ends;
// the returned func stops it now.
func stopOnCleanup(t *testing.T, cmd *exec.Cmd) func() {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stop := func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Errorf("process %d did not stop", cmd.Process.Pid)
		}
	}
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, limit time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
