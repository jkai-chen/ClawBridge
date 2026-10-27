package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
)

type queueCounts struct {
	data    map[string]any
	err     error
	request bus.Request
}

func (q *queueCounts) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	q.request = r
	return bus.Result{Source: "server", Data: q.data}, q.err
}
func (q *queueCounts) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "", "", nil
}

// The queue check reads the MCP server's record and the server's counts:
// a held policy, dead letters, an ended or stale stream warn; it never
// fails.
func TestCheckQueue(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	config := Config{Workspace: "w", Project: "p", Session: "agent", Instance: "configured"}
	record := func(stream string, age time.Duration) *ctxwatch.QueueRecord {
		return &ctxwatch.QueueRecord{Session: "agent", Instance: "inst-1", Stream: stream, UpdatedAt: now.Add(-age).Format(time.RFC3339), Leases: []ctxwatch.QueueLease{{MessageID: "m", LeaseID: "l"}}}
	}
	counts := func(policy string, unacked, dead int64) map[string]any {
		return map[string]any{"policy": policy, "unacked_count": unacked, "dead_count": dead}
	}
	cases := []struct {
		name   string
		state  QueueState
		data   map[string]any
		err    error
		status string
		says   []string
	}{
		{"streaming", QueueState{Session: "agent", Record: record("stream", time.Minute)}, counts("accept", 2, 0), nil, Pass, []string{"holds the message stream, with 1 lease", "2 unacked, 0 dead."}},
		{"polling", QueueState{Session: "agent", Record: record("polling", 0)}, counts("accept", 0, 0), nil, Pass, []string{"polls the queue"}},
		{"no server", QueueState{Session: "agent"}, counts("accept", 0, 0), nil, Pass, []string{"No MCP server of session agent"}},
		{"stale", QueueState{Session: "agent", Record: record("stream", 10*time.Minute)}, counts("accept", 0, 0), nil, Warn, []string{"stale"}},
		{"ended", QueueState{Session: "agent", Record: record("ended", 0)}, counts("accept", 0, 0), nil, Warn, []string{"ended"}},
		{"held", QueueState{Session: "agent", Record: record("stream", 0)}, counts("hold", 3, 0), nil, Warn, []string{"policy is hold", "no dead letter"}},
		{"compaction", QueueState{Session: "agent", Record: record("stream", 0), Hold: &ctxwatch.CompactionHold{Instance: "inst-1", HeldAt: "2026-10-27T23:50:00Z"}}, counts("hold", 0, 0), nil, Warn, []string{"held for a compaction since 2026-10-27T23:50:00Z"}},
		{"dead", QueueState{Session: "agent", Record: record("stream", 0)}, counts("accept", 0, 4), nil, Warn, []string{"0 unacked, 4 dead."}},
		{"old server", QueueState{Session: "agent", Record: record("off", 0)}, map[string]any{"policy": "accept"}, nil, Pass, []string{"no message queue"}},
		{"unreadable", QueueState{Session: "agent", Record: record("stream", 0)}, nil, errors.New("unavailable"), Warn, []string{"couldn't be read"}},
	}
	for _, tc := range cases {
		executor := &queueCounts{data: tc.data, err: tc.err}
		var got []Check
		checkQueue(context.Background(), tc.state, config, executor, now, func(c Check) { got = append(got, c) })
		if len(got) != 1 || got[0].ID != "queue" || got[0].Status != tc.status {
			t.Fatalf("%s: %+v", tc.name, got)
		}
		for _, s := range tc.says {
			if !strings.Contains(got[0].Message, s) {
				t.Fatalf("%s: %q lacks %q", tc.name, got[0].Message, s)
			}
		}
		// The counts are read as the MCP server's instance, or the
		// configured one without a server; always a pure read.
		want := "inst-1"
		if tc.state.Record == nil {
			want = "configured"
		}
		if executor.request.Operation != "notification_status" || executor.request.Args["instance"] != want || executor.request.Args["queue"] != true {
			t.Fatalf("%s: %+v", tc.name, executor.request)
		}
	}
}
