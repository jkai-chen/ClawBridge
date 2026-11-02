package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// FR57: a rearm vouches for the issue's text only when the person reads it
// and confirms in their own terminal.

type rearmEnv struct {
	reads  int
	stderr bytes.Buffer
}

func rearmSetup(t *testing.T, env []string, terminal bool) *rearmEnv {
	t.Helper()
	e := &rearmEnv{}
	prevRead, prevEnv, prevTerm := readRearmIssue, closesEnv, stdinTerminal
	t.Cleanup(func() { readRearmIssue, closesEnv, stdinTerminal = prevRead, prevEnv, prevTerm })
	closesEnv = func() []string { return env }
	stdinTerminal = func() bool { return terminal }
	readRearmIssue = func(context.Context, Config, linearboard.Executor, string) (linearprovider.IssueV2, error) {
		e.reads++
		return linearprovider.IssueV2{Issue: linearprovider.Issue{ID: identifierIssue, Identifier: "ENG-88", Title: "a title", Description: strings.Repeat("x", 600)}, DescriptionKnown: true, DescriptionSHA256: strings.Repeat("d", 64), UpdatedAt: "2026-10-23T10:00:00Z"}, nil
	}
	return e
}

func (e *rearmEnv) run(config Config, confirmed bool, answer string) map[string]any {
	a := map[string]any{"issue_uuid": identifierIssue}
	vouchRearm(context.Background(), config, nil, a, confirmed, strings.NewReader(answer), &e.stderr)
	return a
}

func TestRearmSendsOnlyTheConfirmedUpdateTime(t *testing.T) {
	e := rearmSetup(t, nil, false)
	a := e.run(Config{LinearProfile: "native"}, true, "")
	if a["updated_at"] != "2026-10-23T10:00:00Z" || a["agent_marker"] != "" || len(a) != 3 {
		t.Fatal(a)
	}
	out := e.stderr.String()
	if !strings.Contains(out, "ENG-88") || !strings.Contains(out, "a title") || !strings.Contains(out, "vouches for this title and description as they are now") || strings.Count(out, "x") < 500 || strings.Count(out, "x") > 520 {
		t.Fatal(out)
	}
}

func TestRearmAtATerminalAsksAndDeclineSendsNoHashes(t *testing.T) {
	e := rearmSetup(t, nil, true)
	a := e.run(Config{LinearProfile: "native"}, false, "y\n")
	if a["updated_at"] == nil || !strings.Contains(e.stderr.String(), "[y/N]") {
		t.Fatal(a, e.stderr.String())
	}
	for _, answer := range []string{"n\n", "\n", ""} {
		e = rearmSetup(t, nil, true)
		a = e.run(Config{LinearProfile: "native"}, false, answer)
		if a["updated_at"] != nil || !strings.Contains(e.stderr.String(), "not vouched for") {
			t.Fatalf("answer %q: %v\n%s", answer, a, e.stderr.String())
		}
	}
}

func TestRearmWithoutATerminalOrConfirmSendsNoHashes(t *testing.T) {
	e := rearmSetup(t, nil, false)
	if a := e.run(Config{LinearProfile: "native"}, false, "y\n"); a["updated_at"] != nil || !strings.Contains(e.stderr.String(), "not vouched for") {
		t.Fatal(a, e.stderr.String())
	}
}

func TestRearmByASessionOrAnAgentNeverReadsOrVouches(t *testing.T) {
	e := rearmSetup(t, nil, true)
	if a := e.run(Config{LinearProfile: "native", Session: "coord"}, true, "y\n"); a["updated_at"] != nil || e.reads != 0 {
		t.Fatal("session:", a, e.reads)
	}
	for _, marker := range []string{"CLAUDECODE", "CODEX_HOME", "CLAWBRIDGE_SESSION", "CLAWBRIDGE_INSTANCE"} {
		e = rearmSetup(t, []string{marker + "=1"}, true)
		a := e.run(Config{LinearProfile: "native"}, true, "y\n")
		if a["updated_at"] != nil || e.reads != 0 || a["agent_marker"] != marker {
			t.Fatal(marker, a, e.reads)
		}
	}
	e = rearmSetup(t, nil, true)
	if a := e.run(Config{}, true, ""); a["updated_at"] != nil || e.reads != 0 {
		t.Fatal("no profile:", a)
	}
}

func TestRearmWhenTheReadFailsSendsNoHashes(t *testing.T) {
	e := rearmSetup(t, nil, true)
	readRearmIssue = func(context.Context, Config, linearboard.Executor, string) (linearprovider.IssueV2, error) {
		return linearprovider.IssueV2{}, context.DeadlineExceeded
	}
	if a := e.run(Config{LinearProfile: "native"}, true, ""); a["updated_at"] != nil || !strings.Contains(e.stderr.String(), "could not be read") {
		t.Fatal(a, e.stderr.String())
	}
}
