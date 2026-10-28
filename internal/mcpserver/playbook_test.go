package mcpserver

import (
	"testing"

	"clawbridge/internal/bus"
)

// The coordinator's whoami says where its project's playbook is; any other
// session's whoami is the server's answer as it was.
func TestWhoamiNamesTheCoordinatorsPlaybook(t *testing.T) {
	coordinator := "ses_1"
	store := &queueStore{answer: func(r bus.Request) (bus.Result, error) {
		if r.Operation == "whoami" {
			return bus.Result{Source: "server", Data: map[string]any{"project": map[string]any{"coordinator_session": coordinator}, "session": map[string]any{"id": "ses_1"}}}, nil
		}
		return bus.Result{Source: "server", Data: map[string]any{}}, nil
	}}
	ctx, cs := queueSession(t, store, Options{})
	got := callTool(t, ctx, cs, "whoami", map[string]any{})["data"].(map[string]any)
	pb, ok := got["coordinator_playbook"].(map[string]any)
	if !ok || pb["path"] != "docs/COORDINATOR.md" {
		t.Fatalf("%+v", got)
	}
	coordinator = "ses_2"
	if got = callTool(t, ctx, cs, "whoami", map[string]any{})["data"].(map[string]any); got["coordinator_playbook"] != nil {
		t.Fatalf("another session's whoami gained the playbook: %+v", got)
	}
}
