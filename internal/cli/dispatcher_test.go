package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dispatcher show reads the policy, off by default; domains set takes a
// file at a revision; the domain edits take a domain and a session.
func TestCLIDispatcherPolicy(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	got := invoke(t, "dispatcher", "show")
	if got["revision"] != float64(0) || got["enabled"] != false || got["mode"] != "rules" {
		t.Fatal(got)
	}
	file := filepath.Join(t.TempDir(), "domains.json")
	if err := os.WriteFile(file, []byte(`{"domains": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if set := invoke(t, "dispatcher", "domains", "set", "--file", file, "--expected-revision", "0"); set["revision"] != float64(1) {
		t.Fatal(set)
	}
	rejectCLI(t, "dispatcher", "domains", "set", "--file", file, "--expected-revision", "0")
	rejectCLI(t, "dispatcher", "domains", "set", "--file", file)
	rejectCLI(t, "dispatcher", "domains", "set", "--expected-revision", "1")
	rejectCLI(t, "dispatcher", "domain", "trust", "payments", "--expected-revision", "1")
	rejectCLI(t, "dispatcher", "domain", "promote", "payments", "eng", "--expected-revision", "1")
	rejectCLI(t, "dispatcher", "domain", "trust", "nope", "eng", "--expected-revision", "1")
	rejectCLI(t, "dispatcher", "enable")
	var out, errs bytes.Buffer
	if e := Run(context.Background(), []string{"dispatcher"}, &out, &errs); e == nil || !strings.Contains(e.Error(), "usage") {
		t.Fatal(e)
	}
}

// dispatcher enable and disable reach the bus with their arguments: --engine
// is the mode, --confirm a boolean, and the refusals are the bus's own.
func TestCLIDispatcherEnable(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	refused := func(want string, args ...string) {
		t.Helper()
		var out, errs bytes.Buffer
		if e := Run(context.Background(), args, &out, &errs); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(args, e)
		}
	}
	refused("model setup", "dispatcher", "enable", "--engine", "model", "--expected-revision", "0", "--confirm")
	refused("link the task board", "dispatcher", "enable", "--engine", "rules", "--expected-revision", "0")
	refused("mode is rules or model", "dispatcher", "enable", "--engine", "fast", "--expected-revision", "0")
	refused("already off", "dispatcher", "disable", "--expected-revision", "0")
	refused("requires --expected-revision", "dispatcher", "disable")
}

func TestCLIContextWatchSetting(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	got := invoke(t, "project", "context-watch", "show")
	if got["revision"] != float64(0) || got["context_watch"].(map[string]any)["enabled"] != false {
		t.Fatal(got)
	}
	file := filepath.Join(t.TempDir(), "context-watch.json")
	doc := `{"enabled": true, "levels": [{"role": "*", "warn_tokens": 700000, "handover_tokens": 750000}], "extra_recipients": [], "handover_hint": "Update RESUME HERE."}`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	set := invoke(t, "project", "context-watch", "set", "--file", file, "--expected-revision", "0")
	if set["revision"] != float64(1) || set["context_watch"].(map[string]any)["handover_hint"] != "Update RESUME HERE." {
		t.Fatal(set)
	}
	rejectCLI(t, "project", "context-watch", "set", "--file", file, "--expected-revision", "0")
	rejectCLI(t, "project", "context-watch", "set", "--file", file)
	rejectCLI(t, "project", "context-watch", "set", "--expected-revision", "1")
	rejectCLI(t, "project", "context-watch", "enable")
	if err := os.WriteFile(file, []byte(`{"enabled": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rejectCLI(t, "project", "context-watch", "set", "--file", file, "--expected-revision", "1")
}

// The owner sets the project's watch budget and clears it; any member
// shows it.
func TestCLIProjectWatchBudget(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	if got := invoke(t, "project", "watch-budget", "show"); got["watch_budget"] != nil || got["budget_source"] != "default" {
		t.Fatal(got)
	}
	if got := invoke(t, "project", "watch-budget", "set", "120"); got["watch_budget"] != float64(120) || got["budget_source"] != "project" {
		t.Fatal(got)
	}
	if got := invoke(t, "project", "watch-budget", "show"); got["budget_limit"] != float64(120) {
		t.Fatal(got)
	}
	if got := invoke(t, "project", "watch-budget", "clear"); got["watch_budget"] != nil {
		t.Fatal(got)
	}
	rejectCLI(t, "project", "watch-budget", "set")
	rejectCLI(t, "project", "watch-budget", "set", "0")
	rejectCLI(t, "project", "watch-budget", "set", "2001")
	rejectCLI(t, "project", "linear-budget", "set", "5001")
	if got := invoke(t, "project", "linear-budget", "set", "2000"); got["budget_limit"] != float64(2000) {
		t.Fatal("linear-budget:", got)
	}
	rejectCLI(t, "project", "watch-budget", "set", "ten")
	rejectCLI(t, "project", "watch-budget", "clear", "5")
	rejectCLI(t, "project", "watch-budget")
}

// A session lowers its own levels below the project's, and clears them.
func TestCLIContextLevelsSet(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "eng", "--instance", "eng-i")
	file := filepath.Join(t.TempDir(), "context-watch.json")
	if err := os.WriteFile(file, []byte(`{"enabled": true, "levels": [{"role": "*", "warn_tokens": 700000, "handover_tokens": 750000}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	invoke(t, "project", "context-watch", "set", "--file", file, "--expected-revision", "0")
	as := []string{"--session", "eng", "--instance", "eng-i", "context", "levels", "set"}
	got := invoke(t, append(as, "--warn-tokens", "500000")...)
	if l := got["levels"].(map[string]any); l["warn_tokens"] != float64(500000) || l["handover_tokens"] != float64(750000) {
		t.Fatal(got)
	}
	rejectCLI(t, append(as, "--handover-tokens", "800000")...)
	rejectCLI(t, append(as, "--clear", "--warn-tokens", "1")...)
	rejectCLI(t, "--session", "eng", "--instance", "eng-i", "context", "levels", "show")
	if got = invoke(t, append(as, "--clear")...); got["levels"].(map[string]any)["warn_tokens"] != float64(700000) {
		t.Fatal(got)
	}
	// The local context commands still need no server.
	if out, err := runContextCmd(t, "show", "--session", "eng"); err != nil || !strings.Contains(out, "not bound") {
		t.Fatalf("%q %v", out, err)
	}
}
