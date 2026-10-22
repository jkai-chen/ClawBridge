package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestL1MCPAcceptedReceiptSurvivesLedgerPublicationFailure(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	f.call("taskboard_set", "w", "p", "", l1MappingArgs(0))
	p := &l1LedgerProvider{l1SyntheticProvider: new(l1SyntheticProvider)}
	cfg := l1ServiceConfig(t, f.store, p)
	var faultErr error
	var recordPath string
	var pending []byte
	p.afterWrite = func() {
		// The provider has accepted after pending was persisted. Introduce a
		// deterministic owned-temp publication fault without altering that record.
		faultErr = filepath.WalkDir(cfg.LedgerDirectory, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				if recordPath != "" {
					return errors.New("multiple synthetic pending records")
				}
				recordPath = path
			}
			return nil
		})
		if faultErr != nil {
			return
		}
		if recordPath == "" {
			faultErr = errors.New("pending not present before provider dispatch")
			return
		}
		pending, faultErr = os.ReadFile(recordPath)
		if faultErr != nil {
			return
		}
		if faultErr = os.Rename(recordPath, recordPath+".held"); faultErr != nil {
			return
		}
		faultErr = os.Mkdir(recordPath, 0700)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := mcpserver.NewServerWithOptions(f.store, "w", "p", "worker", "worker-instance", mcpserver.Options{Context: ctx, Taskboard: linearboard.New(cfg)})
	client := notificationMCPConnect(t, ctx, server, make(chan string, 1))
	args := map[string]any{"title": "synthetic accepted once", "operation_id": "mcp-accepted-save-fault"}
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_create", Arguments: args})
	if faultErr != nil {
		t.Fatal("fixture publication fault failed", faultErr)
	}
	if err != nil || !result.IsError {
		t.Fatalf("accepted save failure lost error outcome: result=%v err=%v", result, err)
	}
	var envelope struct {
		Source string
		Data   struct {
			Outcome string
			Receipt map[string]any
		}
	}
	b, _ := json.Marshal(result.StructuredContent)
	if json.Unmarshal(b, &envelope) != nil || envelope.Source != "linear" || envelope.Data.Outcome != "accepted_ledger_unrecorded" || envelope.Data.Receipt["id"] == nil {
		t.Fatal("MCP dropped known receipt or source/outcome on ledger-save error")
	}
	if p.writes.Load() != 1 {
		t.Fatal("healthy accepted mutation did not happen exactly once")
	}
	var stored struct{ State string }
	if json.Unmarshal(pending, &stored) != nil || stored.State != "pending" {
		t.Fatal("durable pending record was absent before injected failure")
	}
	if held, err := os.ReadFile(recordPath + ".held"); err != nil || string(held) != string(pending) {
		t.Fatal("failed save erased/modified durable pending record")
	}
	// Restore only the synthetic filesystem blocker, then exercise a fresh
	// public MCP request against the actual pending record; no operation replay.
	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(recordPath+".held", recordPath); err != nil {
		t.Fatal(err)
	}
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_create", Arguments: args})
	if err == nil && !result.IsError {
		t.Fatal("accepted-but-unrecorded pending operation retried as success")
	}
	if p.writes.Load() != 1 {
		t.Fatal("pending retry dispatched another provider mutation")
	}
	for _, raw := range l1LedgerBytes(t, cfg.LedgerDirectory) {
		if raw != string(pending) {
			t.Fatal("pending retry changed durable bytes")
		}
	}
}
