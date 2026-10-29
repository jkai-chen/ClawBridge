package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"clawbridge/internal/linearboard"
)

const identifierIssue = "0d0d0d0d-0d0d-4d0d-8d0d-0d0d0d0d0d0d"

func TestLinearIssueTakesAnIdentifierAndOtherCommandsKeepTheUUID(t *testing.T) {
	for _, ok := range []string{identifierIssue, "ENG-88", "OPS-5"} {
		if _, pos, err := parseLinear("issue", []string{ok, "--profile", "native", "--comments", "--resolve-labels"}, &bytes.Buffer{}); err != nil || pos[0] != ok {
			t.Fatalf("issue %s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"eng-88", "ENG-0", "ENG88", "x"} {
		if _, _, err := parseLinear("issue", []string{bad, "--profile", "native"}, &bytes.Buffer{}); err == nil || err.Error() != "issue requires its UUID or an identifier like ENG-88" {
			t.Fatalf("issue %q: %v", bad, err)
		}
	}
	for _, command := range []string{"update", "comment", "states"} {
		args := []string{"ENG-88", "--profile", "native", "--operation-id", "op", "--title", "t", "--body", "b"}
		if command == "states" {
			args = []string{"ENG-88", "--profile", "native"}
		}
		if _, _, err := parseLinear(command, args, &bytes.Buffer{}); err == nil {
			t.Fatalf("%s accepted an identifier", command)
		}
	}
}

func proposeSetup(t *testing.T) {
	t.Helper()
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "eng", "--instance", "eng-i")
}

func TestAssignmentProposeResolvesAnIdentifierAlone(t *testing.T) {
	proposeSetup(t)
	var asked []string
	previous := resolveIssueIdentifier
	t.Cleanup(func() { resolveIssueIdentifier = previous })
	resolveIssueIdentifier = func(_ context.Context, _ Config, _ linearboard.Executor, identifier string) (string, string, error) {
		asked = append(asked, identifier)
		return identifierIssue, identifier, nil
	}
	got := invoke(t, "--session", "eng", "--instance", "eng-i", "--linear-profile", "native", "assignment", "propose", "--issue-identifier", "ENG-88", "--task-role", "engineer")["assignment"].(map[string]any)
	if got["issue_uuid"] != identifierIssue || got["issue_identifier"] != "ENG-88" || len(asked) != 1 || asked[0] != "ENG-88" {
		t.Fatal(got, asked)
	}
}

func TestAssignmentProposeWithAUUIDMakesNoLinearRead(t *testing.T) {
	proposeSetup(t)
	calls := 0
	previous := resolveIssueIdentifier
	t.Cleanup(func() { resolveIssueIdentifier = previous })
	resolveIssueIdentifier = func(context.Context, Config, linearboard.Executor, string) (string, string, error) {
		calls++
		return "", "", errors.New("unexpected Linear read")
	}
	got := invoke(t, "--session", "eng", "--instance", "eng-i", "--linear-profile", "native", "assignment", "propose", "--issue-uuid", identifierIssue, "--issue-identifier", "ENG-88", "--task-role", "engineer")["assignment"].(map[string]any)
	if got["issue_uuid"] != identifierIssue || got["issue_identifier"] != "ENG-88" || calls != 0 {
		t.Fatal(got, calls)
	}
	got = invoke(t, "--session", "eng", "--instance", "eng-i", "assignment", "propose", "--issue-uuid", identifierIssue, "--task-role", "engineer")["assignment"].(map[string]any)
	if got["issue_uuid"] != identifierIssue || calls != 0 {
		t.Fatal(got, calls)
	}
}

func TestAssignmentProposeRefusesAStaleIdentifierAndNeedsAProfile(t *testing.T) {
	proposeSetup(t)
	previous := resolveIssueIdentifier
	t.Cleanup(func() { resolveIssueIdentifier = previous })
	resolveIssueIdentifier = func(context.Context, Config, linearboard.Executor, string) (string, string, error) {
		return "", "", errors.New("ENG-88 now resolves to OPS-5; use that identifier or the UUID")
	}
	rejectCLI(t, "--session", "eng", "--instance", "eng-i", "--linear-profile", "native", "assignment", "propose", "--issue-identifier", "ENG-88")
	rejectCLI(t, "--session", "eng", "--instance", "eng-i", "assignment", "propose", "--issue-identifier", "ENG-88")
	rejectCLI(t, "--session", "eng", "--instance", "eng-i", "--linear-profile", "native", "assignment", "propose", "--issue-identifier", "eng-88")
}
