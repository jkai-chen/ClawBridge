package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGuidedStartupNonterminalBoundaries(t *testing.T) {
	p := newProcessFixture(t)
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "authority must not be contacted", http.StatusInternalServerError)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name                  string
		args                  []string
		usage, fail, selector bool
	}{
		{name: "bare", usage: true},
		{name: "selected-missing", usage: true, selector: true},
		{name: "selected-invalid", usage: true, selector: true},
		{name: "help-long", args: []string{"--help"}, usage: true},
		{name: "help-short", args: []string{"-h"}, usage: true},
		{name: "help-command", args: []string{"help"}, usage: true},
		{name: "wizard-help", args: []string{"setup", "wizard", "--help"}, usage: true},
		{name: "wizard-nonterminal", args: []string{"setup", "wizard"}, fail: true},
		{name: "commandless-mode", args: []string{"--mode", "cloud"}, fail: true},
		{name: "commandless-scope", args: []string{"--workspace", "inherited"}, fail: true},
		{name: "commandless-json", args: []string{"--json"}, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			selected := filepath.Join(dir, "selected.toml")
			bad := []byte("invalid=[\n# synthetic-secret-must-not-echo\n")
			repo := filepath.Join(dir, ".clawbridge.toml")
			if err := os.WriteFile(repo, bad, 0600); err != nil {
				t.Fatal(err)
			}
			if tc.name == "selected-invalid" {
				if err := os.WriteFile(selected, bad, 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := append([]string{}, tc.args...)
			if tc.selector {
				args = append(args, "--client-config", selected)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, p.binary, args...)
			cmd.Dir = dir
			cmd.Env = []string{"HOME=" + dir, "PATH=" + dir, "TERM=xterm", "CLAWBRIDGE_HOME=" + state,
				"CLAWBRIDGE_CONFIG=" + filepath.Join(dir, "env-missing.toml"), "CLAWBRIDGE_SERVER=" + srv.URL,
				"CLAWBRIDGE_MODE=cloud", "CLAWBRIDGE_CREDENTIAL_SOURCE=gcloud", "CLAWBRIDGE_SESSION=unrequested-actor"}
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("nonterminal command blocked")
			}
			if (err != nil) != tc.fail {
				t.Errorf("unexpected exit result: %v", err)
			}
			if tc.usage && !strings.Contains(strings.ToLower(string(out)), "clawbridge") && !strings.Contains(strings.ToLower(string(out)), "usage of setup wizard") {
				t.Error("usage/help missing")
			}
			if tc.name == "wizard-nonterminal" && (!strings.Contains(strings.ToLower(string(out)), "terminal") || !strings.Contains(string(out), "setup join")) {
				t.Error("nonterminal wizard lacks terminal/setup-join guidance")
			}
			if strings.Contains(string(out), "synthetic-secret-must-not-echo") {
				t.Error("config content reflected")
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Error("noninteractive boundary opened state storage")
			}
			if raw, err := os.ReadFile(repo); err != nil || string(raw) != string(bad) {
				t.Error("repository config changed")
			}
			if raw, err := os.ReadFile(selected); tc.name == "selected-invalid" {
				if err != nil || string(raw) != string(bad) {
					t.Error("existing selected config changed")
				}
			} else if !os.IsNotExist(err) {
				t.Error("noninteractive boundary created selected config")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("noninteractive boundary contacted inherited authority")
	}
}
