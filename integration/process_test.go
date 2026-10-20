package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"time"
)

type processFixture struct {
	t               *testing.T
	binary, dir, db string
	scopeFromConfig bool
	env             []string
}

func newProcessFixture(t *testing.T) *processFixture {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "clawbridge")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/clawbridge")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	env := []string{}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLAWBRIDGE_") {
			env = append(env, e)
		}
	}
	env = append(env, "CLAWBRIDGE_HOME="+filepath.Join(dir, "state"))
	return &processFixture{t: t, binary: binary, dir: dir, db: filepath.Join(dir, "bus.db"), env: env}
}
func (p *processFixture) command(args ...string) *exec.Cmd {
	prefix := []string{"--db", p.db}
	if !p.scopeFromConfig {
		prefix = append(prefix, "--workspace", "w", "--project", "p")
	}
	full := append(prefix, args...)
	cmd := exec.Command(p.binary, full...)
	cmd.Dir = p.dir
	cmd.Env = p.env
	return cmd
}
func (p *processFixture) run(args ...string) any {
	p.t.Helper()
	cmd := p.command(args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		p.t.Fatalf("CLI %v: %v\nstderr: %s\nstdout: %s", args, err, stderr.String(), output)
	}
	var result struct {
		Source string `json:"source"`
		Data   any    `json:"data"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		p.t.Fatalf("CLI %v invalid JSON: %v: %s", args, err, output)
	}
	if result.Source != "local-authority" {
		p.t.Fatalf("CLI %v source=%q", args, result.Source)
	}
	return result.Data
}
func (p *processFixture) fail(args ...string) string {
	p.t.Helper()
	cmd := p.command(args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		p.t.Fatalf("CLI %v unexpectedly succeeded: %s", args, output)
	}
	if len(output) == 0 {
		p.t.Fatalf("CLI %v failed without diagnostic", args)
	}
	return string(output)
}
func (p *processFixture) seed() { p.run("setup", "--local"); p.run("project", "create", "p") }
func cliActor(session string, args ...string) []string {
	return append([]string{"--session", session, "--instance", session + "-instance"}, args...)
}

func TestProcessIntegration(t *testing.T) {
	p := newProcessFixture(t)
	p.seed()
	p.run("--project", "other", "project", "create", "other")
	p.run(cliActor("alice", "register")...)
	p.run(cliActor("bob", "register")...)
	t.Run("CLIExchange", func(t *testing.T) {
		local := *p
		p := &local
		p.t = t
		p.run(cliActor("alice", "register")...)
		p.run(cliActor("bob", "register")...)
		sent := p.run(cliActor("alice", "send", "bob", "hello from CLI", "--operation-id", "cli-send")...)
		id := stringField(t, sent, "id")
		p.run(cliActor("bob", "inbox")...)
		p.run(cliActor("bob", "mark-read", id)...)
		p.run(cliActor("alice", "status", id)...)
		reply := p.run(cliActor("bob", "reply", id, "CLI reply", "--operation-id", "cli-reply")...)
		if stringField(t, reply, "thread_id") != stringField(t, sent, "thread_id") {
			t.Fatalf("CLI reply lost thread")
		}
		p.fail(cliActor("alice", "send", "p/bob", "unsupported", "--operation-id", "cross")...)
		p.fail("--session", "alice", "--instance", "wrong", "inbox")
		p.fail("setup")
	})
	t.Run("CLILiteralMessageArguments", func(t *testing.T) {
		local := *p
		p := &local
		p.t = t
		p.run(cliActor("mcp", "register")...)
		sent := p.run(cliActor("alice", "send", "mcp", "install", "--operation-id", "literal-command-words")...)
		if object(t, sent)["body"] != "install" {
			t.Fatalf("literal body changed: %v", sent)
		}
		literal := p.run(cliActor("alice", "send", "--operation-id", "literal-flag", "--", "bob", "--workspace")...)
		if object(t, literal)["body"] != "--workspace" {
			t.Fatalf("flag-like body changed: %v", literal)
		}
	})
	t.Run("StoreAcrossCLIProcesses", func(t *testing.T) {
		local := *p
		p := &local
		p.t = t
		first, err := bus.Open(p.db)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Close()
		second, err := bus.Open(p.db)
		if err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		p.run(cliActor("persistent", "register")...)
		p.run(cliActor("alice", "send", "persistent", "message while stores are open", "--operation-id", "external-send")...)
		result, err := second.Execute(context.Background(), bus.Request{Operation: "inbox", Workspace: "w", Project: "p", Session: "persistent", Args: actor("persistent", nil)})
		if err != nil {
			t.Fatalf("long-lived Store after CLI writes: %v", err)
		}
		raw, _ := json.Marshal(result.Data)
		if !bytes.Contains(raw, []byte("message while stores are open")) {
			t.Fatalf("missing CLI delivery: %s", raw)
		}
	})
	t.Run("MCPStdioPinnedScope", func(t *testing.T) { testMCP(t, p) })
	t.Run("Installers", func(t *testing.T) { testInstallers(t, p) })
	t.Run("Resolution", func(t *testing.T) { testResolution(t, p) })
}

type rpcProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     *json.Encoder
	lines  chan []byte
	stderr *bytes.Buffer
	next   int
}

func startRPC(t *testing.T, p *processFixture, args ...string) *rpcProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	cmd := p.command(args...)
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	// CommandContext provides a timeout even if protocol negotiation stalls.
	bounded := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	bounded.Dir = cmd.Dir
	bounded.Env = cmd.Env
	cmd = bounded
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
	rpc := &rpcProcess{t: t, cmd: cmd, in: json.NewEncoder(input), lines: make(chan []byte, 100), stderr: stderr}
	go func() {
		defer close(rpc.lines)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		for scanner.Scan() {
			rpc.lines <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	t.Cleanup(func() { input.Close(); cmd.Process.Kill(); cmd.Wait(); cancel() })
	return rpc
}
func (r *rpcProcess) request(method string, params any) map[string]any {
	r.t.Helper()
	r.next++
	id := r.next
	if err := r.in.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		r.t.Fatal(err)
	}
	for {
		select {
		case line, ok := <-r.lines:
			if !ok {
				r.t.Fatalf("MCP stdout closed during %s", method)
			}
			var response map[string]any
			if err := json.Unmarshal(line, &response); err != nil {
				r.t.Fatalf("MCP stdout is not protocol JSON: %s", line)
			}
			if response["jsonrpc"] != "2.0" {
				r.t.Fatalf("invalid MCP envelope: %s", line)
			}
			if response["id"] == float64(id) {
				return response
			}
		case <-time.After(10 * time.Second):
			r.t.Fatalf("MCP timed out during %s", method)
		}
	}
}
func (r *rpcProcess) call(name string, args map[string]any) map[string]any {
	return r.request("tools/call", map[string]any{"name": name, "arguments": args})
}
func rpcSuccess(t *testing.T, response map[string]any) any {
	t.Helper()
	if response["error"] != nil {
		t.Fatalf("MCP protocol error: %v", response)
	}
	result := object(t, response["result"])
	if result["isError"] == true {
		t.Fatalf("MCP tool error: %v", result)
	}
	// SDKs may return structuredContent and/or the same JSON in text content.
	if data, ok := result["structuredContent"]; ok {
		return data
	}
	content, ok := result["content"].([]any)
	if !ok {
		t.Fatalf("missing MCP result content: %v", result)
	}
	for _, item := range content {
		entry := object(t, item)
		if entry["type"] == "text" {
			var value any
			if json.Unmarshal([]byte(fmt.Sprint(entry["text"])), &value) == nil {
				return value
			}
		}
	}
	t.Fatalf("missing JSON MCP tool result: %v", result)
	return nil
}
func rpcFailure(t *testing.T, response map[string]any) {
	t.Helper()
	if response["error"] != nil {
		return
	}
	if object(t, response["result"])["isError"] != true {
		t.Fatalf("MCP invalid call succeeded: %v", response)
	}
}
func mcpData(t *testing.T, value any) any {
	t.Helper()
	result := object(t, value)
	if result["source"] != "local-authority" {
		t.Fatalf("MCP source missing: %v", value)
	}
	return result["data"]
}

func initializeRPC(t *testing.T, r *rpcProcess) {
	t.Helper()
	initialized := r.request("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "clawbridge-independent-protocol-test", "version": "1"}})
	if initialized["error"] != nil {
		t.Fatalf("initialize: %v", initialized)
	}
	if err := r.in.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		t.Fatal(err)
	}
}

func testMCP(t *testing.T, p *processFixture) {
	local := *p
	p = &local
	p.t = t
	configPath := filepath.Join(p.dir, "mcp-context.toml")
	initialConfig := fmt.Sprintf(`workspace = "w"
project = "p"
db = %q
mode = "local"
`, p.db)
	if err := os.WriteFile(configPath, []byte(initialConfig), 0600); err != nil {
		t.Fatal(err)
	}
	pinned := *p
	pinned.scopeFromConfig = true
	pinned.env = append(append([]string{}, p.env...), "CLAWBRIDGE_CONFIG="+configPath)
	r := startRPC(t, &pinned, "--session", "mcp-agent", "mcp")
	initializeRPC(t, r)
	listed := r.request("tools/list", map[string]any{})
	tools := object(t, listed["result"])["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		names[stringField(t, tool, "name")] = true
	}
	for _, name := range []string{"register_session", "list_sessions", "send", "inbox", "mark_read", "reply", "update_session", "retire_session", "history", "whoami", "message_status"} {
		if !names[name] {
			t.Errorf("missing tool %s", name)
		}
	}
	registration := mcpData(t, rpcSuccess(t, r.call("register_session", map[string]any{"role": "worker", "agent_kind": "other"})))
	if stringField(t, registration, "instance") == "" {
		t.Fatal("registration did not bind instance")
	}
	before := mcpData(t, rpcSuccess(t, r.call("whoami", map[string]any{})))
	mcpData(t, rpcSuccess(t, r.call("list_sessions", map[string]any{})))
	mcpData(t, rpcSuccess(t, r.call("history", map[string]any{})))
	config := fmt.Sprintf("workspace = \"w\"\nproject = \"other\"\ndb = %q\nmode = \"local\"\n", p.db)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(configPath) })
	after := mcpData(t, rpcSuccess(t, r.call("whoami", map[string]any{})))
	if stringField(t, object(t, before)["project"], "id") != stringField(t, object(t, after)["project"], "id") {
		t.Fatalf("MCP scope drifted after config mutation")
	}
	rpcFailure(t, r.call("send", map[string]any{"to": "bob", "body": "override", "operation_id": "scope-inject", "project": "other"}))
	rpcFailure(t, r.call("send", map[string]any{"to": "bob", "body": "override", "operation_id": "instance-inject", "instance": "bob-instance"}))
	sent := mcpData(t, rpcSuccess(t, r.call("send", map[string]any{"to": "bob", "body": "hello via MCP", "operation_id": "mcp-send"})))
	id := stringField(t, sent, "id")
	p.run(cliActor("bob", "inbox")...)
	p.run(cliActor("bob", "reply", id, "reply from CLI to MCP", "--operation-id", "cli-mcp-reply")...)
	inbox := mcpData(t, rpcSuccess(t, r.call("inbox", map[string]any{})))
	raw, _ := json.Marshal(inbox)
	if !bytes.Contains(raw, []byte("reply from CLI to MCP")) {
		t.Fatalf("MCP did not receive CLI reply: %s", raw)
	}
	rpcFailure(t, r.call("send", map[string]any{"to": "missing", "body": "visible failure", "operation_id": "mcp-error"}))
	messages := pageItems(t, "messages", inbox)
	var replyID string
	for _, message := range messages {
		if object(t, message)["body"] == "reply from CLI to MCP" {
			replyID = stringField(t, message, "id")
		}
	}
	if replyID == "" {
		t.Fatal("MCP reply missing ID")
	}
	mcpData(t, rpcSuccess(t, r.call("mark_read", map[string]any{"ids": []string{replyID}})))
	replied := mcpData(t, rpcSuccess(t, r.call("reply", map[string]any{"message_id": replyID, "body": "MCP replies to CLI", "operation_id": "mcp-reply"})))
	if stringField(t, replied, "thread_id") != stringField(t, sent, "thread_id") || stringField(t, replied, "reply_to") != replyID {
		t.Fatalf("MCP reply ancestry wrong: %v", replied)
	}
	mcpData(t, rpcSuccess(t, r.call("message_status", map[string]any{"message_id": id})))
	mcpData(t, rpcSuccess(t, r.call("update_session", map[string]any{"notes": "MCP owns its session"})))
	// A replacement process explicitly resumes the known instance and fences the old one.
	replacement := startRPC(t, p, "--session", "mcp-agent", "--instance", stringField(t, registration, "instance"), "mcp")
	initializeRPC(t, replacement)
	resumed := mcpData(t, rpcSuccess(t, replacement.call("register_session", map[string]any{"resume": true})))
	if stringField(t, resumed, "instance") == stringField(t, registration, "instance") || stringField(t, resumed, "id") != stringField(t, registration, "id") {
		t.Fatalf("MCP resume did not rotate/fence: %v", resumed)
	}
	rpcFailure(t, r.call("inbox", map[string]any{}))
	mcpData(t, rpcSuccess(t, replacement.call("inbox", map[string]any{})))
	mcpData(t, rpcSuccess(t, replacement.call("retire_session", map[string]any{"successor": "bob"})))
	rpcFailure(t, replacement.call("send", map[string]any{"to": "bob", "body": "retired sender", "operation_id": "mcp-retired"}))

}

func testInstallers(t *testing.T, p *processFixture) {
	for _, agent := range []string{"claude-code", "codex"} {
		t.Run(agent, func(t *testing.T) {
			local := *p
			p := &local
			p.t = t
			path := filepath.Join(t.TempDir(), "config")
			original := []byte(`{"unrelated":{"keep":[1,2,3]},"mcpServers":{"existing":{"command":"do-not-change"}}}`)
			if agent == "codex" {
				original = []byte("model = \"keep-me\"\n[unrelated]\nvalue = 42\n[mcp_servers.existing]\ncommand = \"do-not-change\"\n")
			}
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"mcp", "install", "--agent", agent, "--config", path, "--command", p.binary, "--workspace", "w", "--project", "p", "--session", "installer-agent", "--db", p.db, "--name", "review-test"}
			cmd := p.command(append(args, "--dry-run")...)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("installer dry-run: %v: %s", err, output)
			}
			unchanged, _ := os.ReadFile(path)
			if !bytes.Equal(original, unchanged) {
				t.Fatal("dry-run changed config")
			}
			for i := 0; i < 2; i++ {
				cmd = p.command(args...)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("install iteration %d: %v: %s", i, err, output)
				}
				if i == 0 {
					unchanged, _ = os.ReadFile(path)
				} else {
					second, _ := os.ReadFile(path)
					if !bytes.Equal(unchanged, second) {
						t.Fatal("repeat installation changed config bytes")
					}
				}
			}
			installed, _ := os.ReadFile(path)
			if !bytes.Contains(installed, []byte("do-not-change")) || !bytes.Contains(installed, []byte("unrelated")) {
				t.Fatalf("installer lost unrelated data: %s", installed)
			}
			backups, _ := filepath.Glob(path + "*")
			backedUp := false
			for _, candidate := range backups {
				if candidate != path {
					data, _ := os.ReadFile(candidate)
					if bytes.Equal(data, original) {
						backedUp = true
					}
				}
			}
			if !backedUp {
				t.Error("no backup preserves original config bytes")
			}
			invalid := []byte("{ definitely invalid [\n")
			if err := os.WriteFile(path, invalid, 0600); err != nil {
				t.Fatal(err)
			}
			cmd = p.command(args...)
			if output, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("invalid config accepted: %s", output)
			}
			unchanged, _ = os.ReadFile(path)
			if !bytes.Equal(invalid, unchanged) {
				t.Fatal("invalid config replaced")
			}
		})
	}
}
