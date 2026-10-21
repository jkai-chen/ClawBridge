package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotificationCLIHelpAndValidationBeforeConfig(t *testing.T) {
	p := newProcessFixture(t)
	home := filepath.Join(p.dir, "isolated home")
	stateHome := filepath.Join(home, "state")
	if err := os.MkdirAll(stateHome, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(stateHome, "config.toml")
	poison := []byte("malformed = [ 'synthetic config sentinel'\n")
	if err := os.WriteFile(config, poison, 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(p.dir, "private notify", "state.json")
	for _, tc := range []struct {
		name    string
		args    []string
		want    string
		success bool
	}{
		{"help", []string{"notify", "--help"}, "adapter", true},
		{"missing-adapter", []string{"notify", "--state", state}, "adapter", false},
		{"unknown-adapter", []string{"notify", "--adapter", "shell", "--state", state}, "adapter", false},
		{"relative-state", []string{"notify", "--adapter", "json", "--state", "relative.json"}, "absolute", false},
		{"fast-interval", []string{"notify", "--adapter", "json", "--state", state, "--interval", "500ms"}, "interval", false},
		{"slow-interval", []string{"notify", "--adapter", "json", "--state", state, "--interval", "301s"}, "interval", false},
		{"thread-alias", []string{"notify", "--adapter", "codex", "--state", state, "--thread", "latest"}, "UUID", false},
		{"json-thread-empty", []string{"notify", "--adapter", "json", "--state", state, "--thread="}, "Codex-only", false},
		{"json-command-empty", []string{"notify", "--adapter", "json", "--state", state, "--codex-command="}, "Codex-only", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, p.binary, tc.args...)
			cmd.Dir = p.dir
			cmd.Env = []string{"HOME=" + home, "CLAWBRIDGE_HOME=" + stateHome, "PATH=" + p.dir}
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.success || !strings.Contains(string(output), tc.want) {
				t.Fatalf("validation/help wrong: err=%v output=%s", err, output)
			}
			if strings.Contains(string(output), "synthetic config sentinel") {
				t.Fatal("config content echoed")
			}
			if after, err := os.ReadFile(config); err != nil || !bytes.Equal(after, poison) {
				t.Fatal("validation changed config")
			}
			for _, path := range []string{state, state + ".lock", filepath.Join(stateHome, "bus.db")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("validation created state/authority: %s", filepath.Base(path))
				}
			}
		})
	}
}

func TestNotificationCLIInboxStatus(t *testing.T) {
	p := newProcessFixture(t)
	p.seed()
	for _, name := range []string{"alice", "bob"} {
		p.run(cliActor(name, "register")...)
	}
	sent := p.run(cliActor("alice", "send", "bob", "private body not notification metadata", "--operation-id", "notification-cli-send")...)
	result := object(t, p.run(cliActor("bob", "inbox-status")...))
	seq := object(t, sent)["sequence"]
	if len(result) != 6 || result["unread_count"] != float64(1) || result["latest_sequence"] != seq || result["new_count"] != float64(1) {
		t.Fatalf("wrong CLI metadata: %v", result)
	}
	result = object(t, p.run(cliActor("bob", "inbox-status", "--after", fmtSequence(t, seq))...))
	if result["new_count"] != float64(0) {
		t.Fatal("CLI after is not exclusive")
	}
	p.fail(cliActor("bob", "inbox-status", "--after", "-1")...)
}
func fmtSequence(t *testing.T, v any) string {
	t.Helper()
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("sequence type=%T", v)
	}
	return fmt.Sprintf("%.0f", n)
}
