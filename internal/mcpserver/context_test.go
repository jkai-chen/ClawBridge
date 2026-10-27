package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/notify"
)

// The MCP server reports its session's context when given the binding
// directory, and not otherwise. report_context is never one of its tools.
func TestServerReportsContext(t *testing.T) {
	interval := notify.ContextInterval
	notify.ContextInterval = 10 * time.Millisecond
	t.Cleanup(func() { notify.ContextInterval = interval })
	dir := filepath.Join(t.TempDir(), "context")
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","sessionId":"s","message":{"model":"m","usage":{"input_tokens":5}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ctxwatch.SaveBinding(dir, "worker-1", ctxwatch.Binding{Agent: ctxwatch.ClaudeCode, Transcript: transcript, BoundAt: "2026-01-01T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	reported := func(contextDir string) bool {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := &recordingStore{}
		server := NewServerWithOptions(store, "w", "p", "worker-1", "i-1", Options{Context: ctx, ContextDir: contextDir})
		if server == nil {
			t.Fatal("no server")
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			store.mu.Lock()
			for _, c := range store.calls {
				if c.Operation == "report_context" && c.Session == "worker-1" && c.Args["instance"] == "i-1" {
					store.mu.Unlock()
					return true
				}
			}
			store.mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
		return false
	}
	if !reported(dir) {
		t.Fatal("never reported")
	}
	if reported("") {
		t.Fatal("reported without a binding directory")
	}
}
