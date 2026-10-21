package cli

import (
	"bytes"
	"clawbridge/internal/hostobserve"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCoordinationHostCLIValidationAndHelpAreConfigFree(t *testing.T) {
	d := isolated(t)
	t.Setenv("CLAWBRIDGE_CONFIG", filepath.Join(d, "malformed"))
	old := runHostObservation
	defer func() { runHostObservation = old }()
	runHostObservation = func(context.Context, hostobserve.Config) (hostobserve.Result, error) {
		t.Fatal("invalid flags reached host")
		return hostobserve.Result{}, nil
	}
	for _, args := range [][]string{{"host-status", "--help"}, {"host-wait", "--help"}, {"host-status", "--adapter", "codex", "--endpoint", "relative", "--thread", "11111111-2222-3333-4444-555555555555"}, {"host-wait", "--adapter", "codex", "--endpoint", "unix:///tmp/sock", "--thread", "11111111-2222-3333-4444-555555555555", "--for", "exit"}} {
		var out, diag bytes.Buffer
		err := Run(context.Background(), args, &out, &diag)
		if len(args) == 2 {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("invalid accepted", args)
		}
	}
	entries, err := os.ReadDir(d)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestCoordinationHostStatusAndWaitShareTerminalEnvelope(t *testing.T) {
	isolated(t)
	old := runHostObservation
	defer func() { runHostObservation = old }()
	runHostObservation = func(context.Context, hostobserve.Config) (hostobserve.Result, error) {
		return hostobserve.Result{Outcome: "matched", LastObservation: &hostobserve.Observation{Adapter: "codex", ThreadID: "11111111-2222-3333-4444-555555555555", State: "idle", ActiveFlags: []string{}, ObservedAt: "2026-10-21T00:00:00Z", QueueState: "unknown"}}, nil
	}
	for _, command := range []string{"host-status", "host-wait"} {
		args := []string{command, "--adapter", "codex", "--endpoint", "unix:///tmp/socket", "--thread", "11111111-2222-3333-4444-555555555555"}
		if command == "host-wait" {
			args = append(args, "--for", "idle")
		}
		var out, diag bytes.Buffer
		if err := Run(context.Background(), args, &out, &diag); err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Source string             `json:"source"`
			Data   hostobserve.Result `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || envelope.Source != "host-observation" || envelope.Data.Outcome != "matched" || envelope.Data.LastObservation == nil {
			t.Fatal(out.String(), err)
		}
	}
}
