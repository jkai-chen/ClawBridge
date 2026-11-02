package linearboard

import (
	"context"
	"testing"

	"clawbridge/internal/agentenv"
	"clawbridge/internal/bus"
)

type markerRecorder struct{ sent []bus.Request }

func (m *markerRecorder) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	m.sent = append(m.sent, r)
	return bus.Result{Data: map[string]any{}}, nil
}

// Every person-only sender in this package sends the agent marker, and
// only a person-only operation is one of these senders.
func TestPersonOnlySendersSendTheAgentMarker(t *testing.T) {
	old := agentenv.Environ
	defer func() { agentenv.Environ = old }()
	agentenv.Environ = func() []string { return []string{"CLAUDECODE=1"} }
	rec := &markerRecorder{}
	s := New(Config{Executor: rec})
	ctx := context.Background()
	scope := bus.Request{Workspace: "w", Project: "p"}
	team := "1111aaaa-1111-4111-8111-111111111111"
	_, _ = s.SetClosesV2(ctx, scope, "3333aaaa-3333-4333-8333-333333333331", ClosesHuman, 1, "cli")
	_, _ = s.SetCancelKindsV2(ctx, scope, []string{"a"}, 1, "cli")
	_, _ = s.SetLanePin(ctx, scope, LanePinInput{Kind: "principal", Key: "a@b.c", Lane: "x", Expected: 0})
	_, _ = s.SetLanePin(ctx, scope, LanePinInput{Kind: "principal", Key: "a@b.c", Clear: true, Expected: 1})
	_, _ = s.SetProjectKind(ctx, scope, ProjectKindInput{Team: team, Values: []string{"Engineering"}})
	_, _ = s.ProjectFlowResumeV2(ctx, scope, "3333aaaa-3333-4333-8333-333333333331")
	_, _ = s.AckProjectCreate(ctx, scope, "op-12345678")
	_, _ = s.RepairUnlinkV2(ctx, scope, 1)
	_, _ = s.RefreshNamesV2(ctx, scope)
	_, _ = s.SetLanes(ctx, scope, LanesSetInput{Unpinned: "read_only", Expected: 1})
	seen := map[string]bool{}
	for _, r := range rec.sent {
		if !bus.PersonOnly(r.Operation) {
			continue
		}
		seen[r.Operation] = true
		if got, ok := r.Args["agent_marker"].(string); !ok || got != "CLAUDECODE" {
			t.Errorf("%s sent agent_marker %v", r.Operation, r.Args["agent_marker"])
		}
	}
	for _, op := range []string{"status_closes_set", "status_cancel_kinds_set", "lane_pin_set", "lane_pin_clear", "project_kind_set", "project_flow_resume", "project_create_ack", "taskboard_clear_v2", "taskboard_lanes_set"} {
		if !seen[op] {
			t.Errorf("%s was not sent", op)
		}
	}
}
