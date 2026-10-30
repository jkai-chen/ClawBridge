package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearboard"
)

// markersSetup confirms and releases eng's assignment of identifierIssue,
// so the issue has a claim and a release marker row.
func markersSetup(t *testing.T) {
	t.Helper()
	proposeSetup(t)
	invoke(t, "register", "coord", "--instance", "coord-i")
	invoke(t, "--session", "coord", "--instance", "coord-i", "project", "coordinator-set", "coord")
	a := invoke(t, "--session", "eng", "--instance", "eng-i", "assignment", "propose", "--issue-uuid", identifierIssue, "--issue-identifier", "ENG-1", "--task-role", "engineer")["assignment"].(map[string]any)
	invoke(t, "--session", "coord", "--instance", "coord-i", "assignment", "confirm", "eng", "--revision", a["revision"].(string))
	revision := invoke(t, "--session", "eng", "--instance", "eng-i", "whoami")["session"].(map[string]any)["assignment"].(map[string]any)["revision"].(string)
	invoke(t, "--session", "eng", "--instance", "eng-i", "assignment", "release", "--revision", revision)
}

// fakeIdentifier stands in for the Linear read and counts its calls.
func fakeIdentifier(t *testing.T) *[]string {
	t.Helper()
	asked := []string{}
	previous := resolveIssueIdentifier
	t.Cleanup(func() { resolveIssueIdentifier = previous })
	resolveIssueIdentifier = func(_ context.Context, config Config, _ linearboard.Executor, identifier string) (string, string, error) {
		if config.LinearProfile != "native" {
			return "", "", errors.New("wrong profile " + config.LinearProfile)
		}
		asked = append(asked, identifier)
		return identifierIssue, identifier, nil
	}
	return &asked
}

func markerPhases(t *testing.T, data map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, raw := range data["markers"].([]any) {
		row := raw.(map[string]any)
		out[row["phase"].(string)] = row
	}
	return out
}

// By a UUID (any case) or an assignment ID the read makes no Linear call;
// --json is the server's result as it is.
func TestAssignmentMarkersByUUIDAndAssignment(t *testing.T) {
	markersSetup(t)
	asked := fakeIdentifier(t)
	for _, issue := range []string{identifierIssue, strings.ToUpper(identifierIssue)} {
		rows := markerPhases(t, invoke(t, "--session", "eng", "--instance", "eng-i", "assignment", "markers", "--issue", issue, "--json"))
		claim, release := rows["claim"], rows["close"]
		if len(rows) != 2 || claim == nil || release == nil || claim["issue_uuid"] != identifierIssue || claim["issue_identifier"] != "ENG-1" || release["comment_state"] != "waiting_on_claim" {
			t.Fatal(rows)
		}
		for _, k := range []string{"assignment_id", "comment_state", "label_state", "state_state", "state_skip_reason", "state_target", "state_landed", "created_at", "updated_at", "summary"} {
			if _, ok := claim[k]; !ok {
				t.Fatalf("no %s: %v", k, claim)
			}
		}
	}
	id := markerPhases(t, invoke(t, "assignment", "markers", "--issue", identifierIssue))["claim"]["assignment_id"].(string)
	if rows := markerPhases(t, invoke(t, "assignment", "markers", "--assignment", id)); len(rows) != 2 || rows["claim"]["assignment_id"] != id {
		t.Fatal(rows)
	}
	if rows := invoke(t, "assignment", "markers", "--assignment", "asg_unknown")["markers"].([]any); len(rows) != 0 {
		t.Fatal(rows)
	}
	if len(*asked) != 0 {
		t.Fatal("a Linear read for a UUID or an assignment:", *asked)
	}
}

// An identifier is the one the assignment recorded: the server matches it,
// with no Linear read and no --linear-profile needed (one given is fine).
func TestAssignmentMarkersByIdentifier(t *testing.T) {
	markersSetup(t)
	asked := fakeIdentifier(t)
	for _, args := range [][]string{
		{"assignment", "markers", "--issue", "ENG-1"},
		{"assignment", "markers", "--issue", "eng-1"},
		{"--linear-profile", "native", "assignment", "markers", "--issue", "ENG-1"},
	} {
		rows := markerPhases(t, invoke(t, args...))
		if len(rows) != 2 || rows["claim"]["issue_uuid"] != identifierIssue || rows["close"]["issue_identifier"] != "ENG-1" {
			t.Fatal(args, rows)
		}
	}
	if rows := invoke(t, "assignment", "markers", "--issue", "ENG-404")["markers"].([]any); len(rows) != 0 {
		t.Fatal(rows)
	}
	for _, args := range [][]string{
		{"assignment", "markers"},
		{"assignment", "markers", "--issue", identifierIssue, "--assignment", "asg_1"},
		{"assignment", "markers", "--issue", "ENG 1"},
		{"assignment", "markers", "--issue", identifierIssue, "extra"},
	} {
		rejectCLI(t, args...)
	}
	if len(*asked) != 0 {
		t.Fatal("a Linear read:", *asked)
	}
}

// The text view is one line per row, newest first: the phase (a close is a
// release), when, and the server's summary.
func TestAssignmentMarkersText(t *testing.T) {
	markersSetup(t)
	t.Setenv("CLAWBRIDGE_OUTPUT", "human")
	var out, errs bytes.Buffer
	if err := Run(context.Background(), []string{"assignment", "markers", "--issue", identifierIssue}, &out, &errs); err != nil {
		t.Fatal(err, errs.String())
	}
	got := out.String()
	for _, want := range []string{"Claim markers of ENG-1", "PHASE", "UPDATED", "SUMMARY", "claim", "release", "comment waiting_on_claim, label pending, state"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "close") || strings.Contains(got, `"markers"`) {
		t.Fatalf("not the text view:\n%s", got)
	}

	now := time.Date(2026, 10, 30, 10, 5, 0, 0, time.UTC)
	old := viewNow
	viewNow = func() time.Time { return now }
	t.Cleanup(func() { viewNow = old })
	skip := "not moved to Done: someone else had moved the issue, so it was left as it is; set it yourself if right"
	result := bus.Result{Source: "server", Data: map[string]any{"markers": []any{
		map[string]any{"phase": "close", "issue_uuid": identifierIssue, "issue_identifier": "ENG-1", "updated_at": "2026-10-30T10:04:00.000000000Z", "summary": skip},
		map[string]any{"phase": "claim", "issue_uuid": identifierIssue, "issue_identifier": "ENG-1", "updated_at": "2026-10-30T10:00:00.000000000Z", "summary": "comment done, label done, state done (moved)"},
	}}}
	var text bytes.Buffer
	if err := clirender.Write(&text, clirender.Human, "marker_status", result); err != nil {
		t.Fatal(err)
	}
	got = text.String()
	release, claim := strings.Index(got, "release"), strings.Index(got, "claim")
	if release < 0 || claim < 0 || release > claim || !strings.Contains(got, "1 min ago") || !strings.Contains(got, "5 min ago") {
		t.Fatalf("rows:\n%s", got)
	}
	if !strings.Contains(strings.Join(strings.Fields(got), " "), skip) {
		t.Fatalf("summary cut:\n%s", got)
	}
	text.Reset()
	if err := clirender.Write(&text, clirender.Human, "marker_status", bus.Result{Data: map[string]any{"markers": []any{}}}); err != nil || !strings.Contains(text.String(), "No claim or release markers") {
		t.Fatal(err, text.String())
	}
}

// A server older than the read says so, and asks for an upgrade.
func TestAssignmentMarkersOlderServer(t *testing.T) {
	err := markerStatusError(errors.New(`invalid: unsupported operation "marker_status"`))
	if err == nil || err.Error() != "the ClawBridge server is older than this client and has no marker read; upgrade the server" {
		t.Fatal(err)
	}
	err = markerStatusError(errors.New(`invalid: unsupported argument "issue_identifier" for marker_status`))
	if err == nil || err.Error() != "this server reads markers by issue UUID or assignment only; pass the issue UUID" {
		t.Fatal(err)
	}
	other := errors.New("invalid: marker_status takes issue_uuid or assignment_id, exactly one")
	if markerStatusError(other) != other {
		t.Fatal("rewrote another error")
	}
}
