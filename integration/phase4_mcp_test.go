package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
)

// This exercises real installed argv and stdio/HTTP transports with a fake
// gcloud executable. It does not exercise Google or an OS credential store.
func TestPhase4InstalledFreshProcessAndSelectedDoctor(t *testing.T) {
	p := newProcessFixture(t)
	dir := t.TempDir()
	var mu sync.Mutex
	var requests []bus.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/execute" || r.Header.Get("Authorization") != "Bearer synthetic-P4-transport-token" {
			t.Error("unexpected transport path or auth header (values suppressed)")
			w.WriteHeader(401)
			return
		}
		var req bus.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		var data any
		switch req.Operation {
		case "resolve_project":
			data = map[string]any{"workspace_id": "ws_pinned", "project_id": "prj_pinned"}
		case "list_projects":
			data = map[string]any{"projects": []map[string]any{{"id": "prj_pinned", "slug": "project"}}}
		case "whoami":
			data = map[string]any{"principal": "agent@project.iam.gserviceaccount.com", "online": true, "verified": true, "storage_mode": "cloud", "identity_provenance": "google-id-token", "workspace": map[string]any{"id": "ws_pinned"}, "project": map[string]any{"id": "prj_pinned"}}
		case "notification_status":
			// doctor's queue check: a pure read, with the queue's counts.
			if req.Args["queue"] != true {
				t.Error("doctor read notification status without the queue counts")
			}
			data = map[string]any{"policy": "accept", "unacked_count": 0, "dead_count": 0, "latest_seq": 0}
		case "project_overview":
			// doctor's coordinator check: a pure read of who coordinates.
			// Another session does, so there is no playbook check.
			data = map[string]any{"coordinator": map[string]any{"session_id": "ses_coord", "name": "coord"}}
		default:
			t.Errorf("read-only probe requested %s", req.Operation)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: data})
	}))
	defer server.Close()
	config := filepath.Join(dir, "trusted.toml")
	raw := []byte("mode='cloud'\nserver=" + quoteTOML(server.URL) + "\naudience='test-audience'\ncredential_source='gcloud'\naccount='human@example.test'\nimpersonate_service_account='agent@project.iam.gserviceaccount.com'\nprincipal='agent@project.iam.gserviceaccount.com'\nworkspace='ws_pinned'\nproject='prj_pinned'\nsession='operator'\ninstance='operator-instance'\ncache=" + quoteTOML(filepath.Join(dir, "mirror.db")) + "\n")
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte("#!/bin/sh\nprintf '%s' 'synthetic-P4-transport-token'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	agentConfig := filepath.Join(dir, "agent.json")
	env := []string{"HOME=" + dir, "PATH=" + dir, "CLAWBRIDGE_HOME=" + filepath.Join(dir, "state")}
	run := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, p.binary, args...)
		cmd.Dir = dir
		cmd.Env = env
		// Standard output is the result; mcp install reports the hooks it
		// adds on standard error.
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("CLI failed: %v: %s%s", err, out, stderr.Bytes())
		}
		return out
	}
	var installed install.Result
	if err := json.Unmarshal(run("--client-config", config, "mcp", "install", "--agent", "claude-code", "--config", agentConfig, "--command", p.binary, "--session", "mcp-agent", "--instance", "agent-instance"), &installed); err != nil {
		t.Fatal(err)
	}
	argsAny := installed.Entry["args"].([]any)
	args := make([]string, len(argsAny))
	for i, v := range argsAny {
		args[i] = v.(string)
	}
	// Fresh process receives no CLAWBRIDGE_CONFIG or routing environment; use
	// precisely the installed executable/argv, without fixture-injected DB flags.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, installed.Entry["command"].(string), args...)
	cmd.Dir = dir
	cmd.Env = env
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := new(bytes.Buffer)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rpc := &rpcProcess{t: t, cmd: cmd, in: json.NewEncoder(in), lines: make(chan []byte, 100), stderr: stderr}
	go func() {
		defer close(rpc.lines)
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			rpc.lines <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait(); cancel() })
	initializeRPC(t, rpc)
	result := object(t, rpcSuccess(t, rpc.call("whoami", map[string]any{})))
	if result["source"] != "server" {
		t.Fatalf("fresh MCP source=%v", result["source"])
	}
	var report doctor.Report
	if err := json.Unmarshal(run("doctor", "--client-config", config, "--mcp-agent", "claude-code", "--mcp-config", agentConfig, "--mcp-session", "mcp-agent", "--mcp-instance", "agent-instance"), &report); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, check := range report.Checks {
		if check.ID == "mcp.claude-code.clawbridge" {
			found = true
			if check.Status != doctor.Pass {
				t.Errorf("selected doctor route=%s: %s", check.Status, check.Message)
			}
		}
	}
	if !found {
		t.Error("selected MCP check missing")
	}
	mu.Lock()
	captured := append([]bus.Request(nil), requests...)
	mu.Unlock()
	for _, req := range captured {
		if req.Workspace != "ws_pinned" || req.Operation != "list_projects" && req.Project != "prj_pinned" {
			t.Errorf("scope drift: %+v", req)
		}
		if req.Operation == "whoami" && (req.Session != "mcp-agent" || req.Args["instance"] != "agent-instance") {
			t.Errorf("MCP actor drift: %+v", req)
		}
	}
	after, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(after, raw) {
		t.Error("MCP/doctor changed client config")
	}
	entryJSON, _ := json.Marshal(installed.Entry)
	if strings.Contains(string(entryJSON), "synthetic-P4-transport-token") {
		t.Error("token installed")
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "bus.db")); !os.IsNotExist(err) {
		t.Error("cloud process opened local authority")
	}
}
