package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"clawbridge/internal/bus"
)

func TestRunnerLaunchParses(t *testing.T) {
	const issue = "11111111-1111-4111-8111-111111111111"
	v, id, err := parseRunnerLaunch([]string{issue, "--profile", "lin_x", "--role", "reviewer", "--model", "claude-opus-5-5", "--dry-run"}, &bytes.Buffer{})
	if err != nil || id != issue || v.role != "reviewer" || v.model != "claude-opus-5-5" || !v.dryRun {
		t.Fatal(v, id, err)
	}
	if v, _, err = parseRunnerLaunch([]string{issue, "--profile", "lin_x"}, &bytes.Buffer{}); err != nil || v.role != "engineer" || v.model != "" {
		t.Fatal("defaults", v, err)
	}
	for name, args := range map[string][]string{
		"identifier":   {"FIX-12", "--profile", "lin_x"},
		"no issue":     {"--profile", "lin_x"},
		"two issues":   {issue, issue, "--profile", "lin_x"},
		"no profile":   {issue},
		"spaced role":  {issue, "--profile", "lin_x", "--role", "a b"},
		"unknown flag": {issue, "--profile", "lin_x", "--bypass"},
	} {
		if _, _, err := parseRunnerLaunch(args, &bytes.Buffer{}); err == nil {
			t.Fatal(name, "accepted")
		}
	}
}

type pagedSessions struct{ pages []map[string]any }

func (p *pagedSessions) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	page := p.pages[0]
	p.pages = p.pages[1:]
	return bus.Result{Source: "server", Data: page}, nil
}

// Session names come from every page, local and decoded alike.
func TestRunnerSessionNamesWalkEveryPage(t *testing.T) {
	ex := &pagedSessions{pages: []map[string]any{
		{"sessions": []map[string]any{{"name": "repo-engineer-1"}}, "has_more": true, "next_cursor": 5},
		{"sessions": []any{map[string]any{"name": "repo-engineer-2"}}, "has_more": false},
	}}
	names, err := sessionNames(context.Background(), ex, bus.Request{})
	if err != nil || !names["repo-engineer-1"] || !names["repo-engineer-2"] || len(names) != 2 {
		t.Fatal(names, err)
	}
}

// The runner's MCP entry is the one mcp install writes, naming the
// session, its instance and the Linear profile.
func TestRunnerMCPEntryIsMCPInstalls(t *testing.T) {
	t.Setenv("CLAWBRIDGE_HOME", t.TempDir())
	db := filepath.Join(t.TempDir(), "local.db")
	entry, err := runnerMCPEntry(context.Background(), Config{Mode: "local", DB: db, Workspace: "w", Project: "p"}, "repo-engineer-1", "run-abc", "lin_x")
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	raw, _ := entry["args"].([]any)
	for _, a := range raw {
		s, _ := a.(string)
		args = append(args, s)
	}
	want := map[string]string{"--session": "repo-engineer-1", "--instance": "run-abc", "--linear-profile": "lin_x", "--workspace": "w", "--project": "p"}
	for i := 0; i+1 < len(args); i++ {
		if v, ok := want[args[i]]; ok && args[i+1] == v {
			delete(want, args[i])
		}
	}
	if len(want) != 0 || entry["type"] != "stdio" || args[0] != "mcp" {
		t.Fatal(entry, want)
	}
}

// The launch's runner has every hook it needs: a missing one refuses the
// launch after the session is registered (found in the live re-spike).
func TestNewRunnerIsComplete(t *testing.T) {
	r := newRunner(t.TempDir())
	if r.Git == nil || r.Terminal == nil || r.Close == nil || r.ClaudeVersion == nil {
		t.Fatal("the runner lacks a hook", r)
	}
}
