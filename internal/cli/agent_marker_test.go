package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/agentenv"
	"clawbridge/internal/bus"
)

// The generic path sends the marker: with an agent's variable set, the
// server refuses dispatcher enable and the dispatcher stays off.
func TestCLISendsTheAgentMarkerOnAPersonOnlyOp(t *testing.T) {
	dir := isolated(t)
	db := filepath.Join(dir, "cb.db")
	t.Setenv("CLAWBRIDGE_DB", db)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	s, err := bus.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	team := "1111aaaa-1111-4111-8111-111111111111"
	if _, err = s.Execute(context.Background(), bus.Request{Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{team}, "linear_default_team_id": team, "linear_project_ids": []string{"3333aaaa-3333-4333-8333-333333333331"}}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	old := agentenv.Environ
	defer func() { agentenv.Environ = old }()
	agentenv.Environ = func() []string { return []string{"CODEX_SANDBOX=seatbelt"} }
	var out, errs bytes.Buffer
	err = Run(context.Background(), []string{"dispatcher", "enable", "--engine", "rules", "--expected-revision", "0", "--confirm"}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "the agent marker CODEX_* is set") || strings.Contains(err.Error(), "seatbelt") {
		t.Fatal(err)
	}
	out.Reset()
	agentenv.Environ = func() []string { return nil }
	if err = Run(context.Background(), []string{"dispatcher", "show"}, &out, &errs); err != nil || !strings.Contains(out.String(), `"enabled":false`) {
		t.Fatal("a refused enable changed the dispatcher:", out.String(), err)
	}
}

// Every person-only operation has a client sender that sends the marker:
// the list is the bus's table, so the two can't drift. A sender is a source
// file outside the bus that names the operation; at least one such file must
// build its args with bus.WithAgentMarker (or agentenv.Marker), or reach the
// one generic path that does for every person-only op (bus.PersonOnly). The
// linearboard behaviour test and the CLI end-to-end test above check the
// values sent.
func TestEveryPersonOnlyOpHasAMarkerSendingClient(t *testing.T) {
	var files []string
	for _, d := range []string{"cli", "tui", "linearboard", "remote", "dispatchermodel"} {
		matches, _ := filepath.Glob(filepath.Join("..", d, "*.go"))
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				files = append(files, m)
			}
		}
	}
	sends := func(src string) bool {
		return strings.Contains(src, "WithAgentMarker(") || strings.Contains(src, "agentenv.Marker()") || strings.Contains(src, "bus.PersonOnly(")
	}
	// Operations the client reaches by a flag table, not by writing the name.
	viaCLI := map[string]bool{"dispatcher_chat_leads": true, "dispatcher_disable": true, "dispatcher_model_key_delete": true, "intake_trust_remove": true}
	for _, op := range bus.PersonOnlyOps() {
		found, marks := false, false
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			src := string(raw)
			if strings.Contains(src, `"`+op+`"`) {
				found = true
				marks = marks || sends(src)
			}
		}
		if !found && !viaCLI[op] {
			t.Errorf("no client sender found for person-only op %s", op)
		}
		if found && !marks {
			t.Errorf("no source file that names %s sends the agent marker", op)
		}
	}
}
