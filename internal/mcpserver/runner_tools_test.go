package mcpserver

import (
	"slices"
	"testing"

	"clawbridge/internal/runner"
)

// G9-9: every tool that writes is classified for the runner's agent:
// either denied to it (runner.AgentLinearWriteTools, AgentDeniedTools) or
// one of the session's own. A new write tool fails here until it is
// placed, so it never reaches a runner's agent by default.
func TestRunnerClassifiesEveryWriteTool(t *testing.T) {
	sessionsOwn := []string{
		"register_session", "send", "reply", "inbox", "mark_read", "ack", "set_notification_policy",
		"update_session", "retire_session", "assignment_propose", "assignment_release",
	}
	p := &recordingBoardV2{recordingBoard: recordingBoard{data: map[string]any{"ok": true}}}
	ctx, cs := boardSession(t, p)
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
			continue
		}
		if !slices.Contains(runner.AgentLinearWriteTools, tool.Name) && !slices.Contains(runner.AgentDeniedTools, tool.Name) && !slices.Contains(sessionsOwn, tool.Name) {
			t.Errorf("write tool %s is neither denied to a runner's agent nor one of the session's own", tool.Name)
		}
	}
	for _, name := range append(append([]string{}, runner.AgentLinearWriteTools...), runner.AgentDeniedTools...) {
		found := false
		for _, tool := range listed.Tools {
			found = found || tool.Name == name
		}
		if !found {
			t.Errorf("the runner denies %s, which no longer exists", name)
		}
	}
}
