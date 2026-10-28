package server

import (
	"encoding/json"
	"testing"

	"clawbridge/internal/bus"
)

// A queued write keeps the client's agent_marker for its deferred send, so
// intake attribution records it as the client sent it; a client that sent
// none stays one that sent none, never credited.
func TestOutboxKeepsTheAgentMarker(t *testing.T) {
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"own terminal": {map[string]any{"agent_marker": ""}, ""},
		"an agent":     {map[string]any{"agent_marker": "CLAUDECODE"}, "CLAUDECODE"},
		"older client": {map[string]any{}, bus.MarkerUnsent},
	} {
		raw, err := json.Marshal(outboxRequest{Operation: "comment", Args: map[string]any{}, AgentMarker: queuedMarker(c.args)})
		if err != nil {
			t.Fatal(err)
		}
		var q outboxRequest
		if err = json.Unmarshal(raw, &q); err != nil {
			t.Fatal(err)
		}
		got, err := writeMarker(q.request().Args)
		if err != nil || got != c.want {
			t.Fatal(name, got, err)
		}
	}
}
