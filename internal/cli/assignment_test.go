package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestCLIAssignmentRoundTrip(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "eng", "--instance", "eng-i")
	invoke(t, "register", "coord", "--instance", "coord-i")
	set := invoke(t, "--session", "coord", "--instance", "coord-i", "project", "coordinator-set", "coord")
	if set["coordinator_session"] == nil {
		t.Fatal(set)
	}
	proposed := invoke(t, "--session", "eng", "--instance", "eng-i", "assignment", "propose", "--issue-uuid", "0d0d0d0d-0d0d-4d0d-8d0d-0d0d0d0d0d0d", "--issue-identifier", "SYN-4", "--task-role", "engineer", "--scope-path", "internal/cli/**", "--scope-path", "docs/usage.md")
	a := proposed["assignment"].(map[string]any)
	if a["state"] != "proposed" || !reflect.DeepEqual(a["scope_paths"], []any{"docs/usage.md", "internal/cli/**"}) {
		t.Fatal(a)
	}
	rejectCLI(t, "--session", "eng", "--instance", "eng-i", "assignment", "confirm", "eng", "--revision", a["revision"].(string))
	confirmed := invoke(t, "--session", "coord", "--instance", "coord-i", "assignment", "confirm", "eng", "--revision", a["revision"].(string))
	if confirmed["confirmed"] != true {
		t.Fatal(confirmed)
	}
	listed := invoke(t, "--session", "coord", "--instance", "coord-i", "sessions", "--scope-path", "internal/cli/cli.go", "--assignment-state", "confirmed")
	if rows := listed["sessions"].([]any); len(rows) != 1 || rows[0].(map[string]any)["name"] != "eng" {
		t.Fatal(listed)
	}
	if rows := invoke(t, "sessions", "--issue", "SYN-4")["sessions"].([]any); len(rows) != 1 {
		t.Fatal(rows)
	}
	revision := invoke(t, "--session", "eng", "--instance", "eng-i", "whoami")["session"].(map[string]any)["assignment"].(map[string]any)["revision"].(string)
	invoke(t, "--session", "coord", "--instance", "coord-i", "assignment", "release", "eng", "--revision", revision)
	cleared := invoke(t, "--session", "coord", "--instance", "coord-i", "project", "coordinator-clear")
	if cleared["coordinator_session"] != nil {
		t.Fatal(cleared)
	}
	rejectCLI(t, "--session", "eng", "--instance", "eng-i", "assignment", "bogus")
	rejectCLI(t, "--session", "coord", "--instance", "coord-i", "project", "coordinator-set")
}

// The positional argument of confirm and release names the target session,
// which help must say: it is not an assignment ID.
func TestAssignmentHelpNamesTheTargetSession(t *testing.T) {
	isolated(t)
	for _, sub := range []string{"confirm", "release"} {
		var out, errs bytes.Buffer
		_ = Run(context.Background(), []string{"assignment", sub, "--help"}, &out, &errs)
		help := errs.String()
		if !strings.Contains(help, "assignment "+sub+" ") || !strings.Contains(help, "TARGET_SESSION") || !strings.Contains(help, "not an assignment ID") || !strings.Contains(help, "-revision") {
			t.Fatalf("%s help does not explain its positional argument:\n%s", sub, help)
		}
	}
}

func TestPrincipalUnbindRequiresConfirmation(t *testing.T) {
	isolated(t)
	var out, errs bytes.Buffer
	err := Run(context.Background(), []string{"principal-unbind", "alice@example.test"}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatal("unbind ran without --confirm", err)
	}
}

func TestWorkspaceDomainsTakesDomainsOrClear(t *testing.T) {
	isolated(t)
	for _, args := range [][]string{{"workspace-domains"}, {"workspace-domains", "example.com", "--clear"}} {
		var out, errs bytes.Buffer
		if err := Run(context.Background(), args, &out, &errs); err == nil || !strings.Contains(err.Error(), "--clear") {
			t.Fatal("ambiguous workspace-domains accepted", args, err)
		}
	}
}

// The allow-list belongs to the workspace and an unbind to the server:
// neither needs a project, so neither resolves one first.
func TestWorkspaceDomainsAndUnbindDoNotResolveAProject(t *testing.T) {
	isolated(t)
	var out, errs bytes.Buffer
	if err := Run(context.Background(), []string{"workspace-domains", "--clear"}, &out, &errs); err == nil || !strings.Contains(err.Error(), "--workspace") {
		t.Fatal("workspace-domains ran without a workspace", err)
	}
	invoke(t, "setup", "--local", "--workspace", "w")
	for args, want := range map[string]string{"workspace-domains example.com --workspace w": "hosted authority", "principal-unbind alice@example.test --confirm": "only the server owner"} {
		err := Run(context.Background(), strings.Fields(args), &out, &errs)
		if err == nil || strings.Contains(err.Error(), "project") || !strings.Contains(err.Error(), want) {
			t.Fatal(args, "did not reach the authority without a project:", err)
		}
	}
}
