package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/clirender"
)

// The format through Run (live/CLI-OUTPUT-PLAN.md §2): a pipe gets JSON;
// CLAWBRIDGE_OUTPUT=human gets the human layout, with data escaped; --json
// beats it, and --json=false asks for human anywhere; a bad value stops.
func TestRunOutputFormat(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "a", "--instance", "a-one")
	invoke(t, "update", "--session", "a", "--instance", "a-one", "--notes", "notes\x1b[31m")
	run := func(args ...string) (string, error) {
		var out, errs bytes.Buffer
		err := Run(context.Background(), args, &out, &errs)
		return out.String(), err
	}
	t.Setenv("CLAWBRIDGE_OUTPUT", "human")
	out, err := run("sessions")
	if err != nil || strings.HasPrefix(out, "{") || strings.Contains(out, "\x1b") || !strings.Contains(out, `notes\x1b[31m`) || strings.Contains(out, "local-authority") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run("sessions", "--json"); err != nil || !strings.HasPrefix(out, `{"source":"local-authority"`) {
		t.Fatalf("--json: %v %s", err, out)
	}
	t.Setenv("CLAWBRIDGE_OUTPUT", "")
	if out, err := run("sessions"); err != nil || !strings.HasPrefix(out, "{") {
		t.Fatalf("a pipe: %v %s", err, out)
	}
	if out, err := run("sessions", "--json=false"); err != nil || strings.HasPrefix(out, "{") {
		t.Fatalf("--json=false: %v %s", err, out)
	}
	t.Setenv("CLAWBRIDGE_OUTPUT", "tables")
	if out, err := run("sessions"); !errors.Is(err, clirender.ErrOutputEnv) || out != "" {
		t.Fatalf("a bad CLAWBRIDGE_OUTPUT: %v %s", err, out)
	}
	// A message body after -- is never read as the flag.
	t.Setenv("CLAWBRIDGE_OUTPUT", "")
	if jsonFlag([]string{"send", "b", "--operation-id", "x", "--", "--json=false"}) != clirender.FlagUnset ||
		jsonFlag([]string{"sessions", "--json=0"}) != clirender.FlagFalse || jsonFlag([]string{"--json"}) != clirender.FlagTrue {
		t.Fatal("jsonFlag")
	}
}
