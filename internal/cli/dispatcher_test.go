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
