package cli

import (
	"reflect"
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
