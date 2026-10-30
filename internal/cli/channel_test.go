package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/ctxwatch"
)

// channelBinary builds clawbridge once for the channel process tests.
func channelBinary(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	binary := filepath.Join(t.TempDir(), "clawbridge")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/clawbridge")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	return binary
}

// `clawbridge channel serve` over real pipes: the handshake declares the
// channel capability and no tools, stdout carries nothing but JSON-RPC
// lines, and the server ends when stdin closes, leaving its record. With
// the session a literal ${...}, it writes no state at all.
func TestChannelServeProcess(t *testing.T) {
	binary := channelBinary(t)
	for _, session := range []string{"agent", "${CLAWBRIDGE_SESSION}"} {
		home := t.TempDir()
		cmd := exec.Command(binary, "channel", "serve")
		var env []string
		for _, kv := range os.Environ() {
			// Hermetic: no provider or endpoint from the developer's shell.
			if !strings.HasPrefix(kv, "CLAUDE_CODE_USE_") && !strings.HasPrefix(kv, "ANTHROPIC_") && !strings.HasPrefix(kv, "CLAWBRIDGE_") {
				env = append(env, kv)
			}
		}
		cmd.Env = append(env, "CLAWBRIDGE_HOME="+home, "CLAWBRIDGE_SESSION="+session, "CLAWBRIDGE_CONFIG="+filepath.Join(home, "none.toml"))
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		lines := make(chan string, 16)
		go func() {
			defer close(lines)
			s := bufio.NewScanner(stdout)
			for s.Scan() {
				lines <- s.Text()
			}
		}()
		next := func() map[string]any {
			t.Helper()
			select {
			case line, ok := <-lines:
				var m map[string]any
				if !ok || json.Unmarshal([]byte(line), &m) != nil || m["jsonrpc"] != "2.0" {
					t.Fatalf("not a JSON-RPC line: %q (stderr %s)", line, stderr.String())
				}
				return m
			case <-time.After(10 * time.Second):
				t.Fatal("no answer; stderr:", stderr.String())
			}
			return nil
		}
		io.WriteString(stdin, `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`+"\n")
		init := next()
		result, _ := init["result"].(map[string]any)
		caps, _ := result["capabilities"].(map[string]any)
		exp, _ := caps["experimental"].(map[string]any)
		if _, ok := exp["claude/channel"]; !ok || init["id"] != float64(0) || result["protocolVersion"] != "2025-06-18" {
			t.Fatal(init)
		}
		io.WriteString(stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
		io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
		if tools := next(); tools["id"] != float64(1) || len(tools["result"].(map[string]any)["tools"].([]any)) != 0 {
			t.Fatal(tools)
		}
		io.WriteString(stdin, `{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n")
		if ping := next(); ping["id"] != float64(2) {
			t.Fatal(ping)
		}
		stdin.Close()
		ended := make(chan error, 1)
		go func() { ended <- cmd.Wait() }()
		select {
		case err := <-ended:
			if err != nil {
				t.Fatal(err, stderr.String())
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatal("the server didn't end when stdin closed")
		}
		for line := range lines {
			t.Fatalf("stdout after the answers: %q", line)
		}
		if strings.TrimSpace(stderr.String()) != "" {
			t.Fatal("stderr:", stderr.String())
		}
		dir := filepath.Join(home, "context")
		st, ok, err := ctxwatch.LoadChannelState(dir, "agent")
		if session == "agent" && (err != nil || !ok || st.State != ctxwatch.ChannelInactive || st.PID != cmd.Process.Pid || st.ParentPID != os.Getpid()) {
			t.Fatal(st, ok, err)
		}
		if session != "agent" {
			if entries, _ := os.ReadDir(home); len(entries) != 0 {
				t.Fatal("state written without a session:", entries)
			}
		}
	}
}

// The command takes no flags but --help.
func TestChannelCommandUsage(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"channel", "serve", "--help"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), "clawbridge channel serve") {
		t.Fatal(err, out.String())
	}
	for _, args := range [][]string{{"channel"}, {"channel", "serve", "--session", "x"}, {"channel", "other"}} {
		if err := Run(context.Background(), args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "clawbridge channel serve") {
			t.Fatal(args, err)
		}
	}
}
