package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
)

// hookServer answers the queue hooks' calls and records them.
type hookServer struct {
	mu     sync.Mutex
	calls  []bus.Request
	status map[string]any
	err    error
	slow   bool
	// failOp fails only this operation.
	failOp string
}

func (s *hookServer) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, r)
	status, err, slow := s.status, s.err, s.slow
	if r.Operation == s.failOp {
		err = errors.New("unavailable")
	}
	s.mu.Unlock()
	if slow {
		<-ctx.Done()
		return bus.Result{}, ctx.Err()
	}
	if err != nil {
		return bus.Result{}, err
	}
	if r.Operation == "notification_status" {
		return bus.Result{Source: "server", Data: status}, nil
	}
	return bus.Result{Source: "server", Data: map[string]any{}}, nil
}

func (s *hookServer) ops() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		op := c.Operation
		switch c.Operation {
		case "set_notification_policy":
			op += ":" + fmt.Sprint(c.Args["policy"])
		case "nack":
			op += ":" + fmt.Sprint(c.Args["message_id"], "/", c.Args["lease_id"], "/", c.Args["delay_seconds"])
		}
		if c.Args["instance"] != "inst-1" || c.Session != "agent" || c.Workspace != "w" || c.Project != "p" {
			op += "!scope"
		}
		out = append(out, op)
	}
	return strings.Join(out, " ")
}

// queueHookSetup writes the MCP server's record as this test process, the
// child of the hook's agent, and routes the hooks to server.
func queueHookSetup(t *testing.T, server *hookServer) string {
	t.Helper()
	isolated(t)
	t.Setenv("CLAWBRIDGE_SESSION", "")
	parent, _ := ctxwatch.ParentPID(os.Getpid())
	t.Setenv("CLAWBRIDGE_AGENT_PID", strconv.Itoa(parent))
	dir, err := contextDir()
	if err != nil {
		t.Fatal(err)
	}
	record := ctxwatch.QueueRecord{Session: "agent", Workspace: "w", Project: "p", Instance: "inst-1", Server: "https://clawbridge.example.test",
		MCPPID: os.Getpid(), AgentPID: parent, Stream: "stream", UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Leases: []ctxwatch.QueueLease{{MessageID: "msg-1", LeaseID: "lse_1"}, {MessageID: "msg-2", LeaseID: "lse_2"}}}
	if err = ctxwatch.SaveQueueRecord(dir, record); err != nil {
		t.Fatal(err)
	}
	route := queueHookRoute
	queueHookRoute = func(context.Context, ctxwatch.QueueRecord) (queueExecutor, func(), error) {
		return server, func() {}, nil
	}
	t.Cleanup(func() { queueHookRoute = route })
	return dir
}

func stopRun(t *testing.T, input string) string {
	t.Helper()
	contextStdin = strings.NewReader(input)
	t.Cleanup(func() { contextStdin = os.Stdin })
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"hook", "stop", "--from-hook"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

const stopInput = `{"session_id":"00000000-0000-4000-8000-000000000001","transcript_path":"/t.jsonl","hook_event_name":"Stop","stop_hook_active":false}`

const blocked = `{"decision":"block","reason":"2 ClawBridge messages are waiting: call inbox, handle them, then ack"}` + "\n"

// M5: the Stop hook blocks a stop while messages wait, with fixed text and
// a count only, once per new message; never while the agent already
// continues from a Stop hook; and never for a policy other than accept, an
// older server, an error or a slow answer.
func TestStopHookBlocksOncePerMessage(t *testing.T) {
	server := &hookServer{status: map[string]any{"policy": "accept", "unacked_count": float64(2), "latest_seq": float64(7), "unread_count": float64(2)}}
	queueHookSetup(t, server)
	if out := stopRun(t, stopInput); out != blocked {
		t.Fatalf("%q", out)
	}
	if server.ops() != "notification_status" || server.calls[0].Args["queue"] != true {
		t.Fatal(server.ops(), server.calls[0].Args)
	}
	// The same mail doesn't block again: the agent may not be able to
	// handle it, and must not loop.
	if out := stopRun(t, stopInput); out != "" {
		t.Fatalf("blocked twice for one message: %q", out)
	}
	server.status["latest_seq"] = float64(8)
	if out := stopRun(t, stopInput); out != blocked {
		t.Fatalf("a new message didn't block: %q", out)
	}
	server.status["latest_seq"] = float64(9)
	if out := stopRun(t, strings.Replace(stopInput, `"stop_hook_active":false`, `"stop_hook_active":true`, 1)); out != "" {
		t.Fatalf("blocked while already continuing: %q", out)
	}
	if out := stopRun(t, strings.Replace(stopInput, `"stop_hook_active":false`, `"stop_hook_active":"yes"`, 1)); out != "" {
		t.Fatalf("an unreadable stop_hook_active blocked: %q", out)
	}
	for name, change := range map[string]func(){
		"held":       func() { server.status["policy"] = "hold" },
		"old server": func() { delete(server.status, "unacked_count") },
		"none":       func() { server.status["unacked_count"] = float64(0) },
		"error":      func() { server.err = errors.New("unavailable") },
		"slow":       func() { server.slow = true },
	} {
		saved := map[string]any{}
		for k, v := range server.status {
			saved[k] = v
		}
		change()
		start := time.Now()
		if out := stopRun(t, stopInput); out != "" {
			t.Fatalf("%s: %q", name, out)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("%s: took %v", name, time.Since(start))
		}
		server.status, server.err, server.slow = saved, nil, false
	}
	if out := stopRun(t, stopInput); out != blocked {
		t.Fatalf("%q", out)
	}
	if stopReason(1) != "1 ClawBridge message is waiting: call inbox, handle it, then ack" {
		t.Fatal(stopReason(1))
	}
}

// A hook acts only for its own agent's fresh record: another agent's, a
// stale one, or one whose MCP server isn't the agent's child is left alone.
func TestQueueHooksActOnlyForTheirAgent(t *testing.T) {
	server := &hookServer{status: map[string]any{"policy": "accept", "unacked_count": float64(2), "latest_seq": float64(7)}}
	dir := queueHookSetup(t, server)
	record, _, _ := ctxwatch.LoadQueueRecord(dir, "agent")
	for name, change := range map[string]func(r *ctxwatch.QueueRecord){
		"other agent": func(r *ctxwatch.QueueRecord) { r.AgentPID = os.Getppid() + 1 },
		"stale": func(r *ctxwatch.QueueRecord) {
			r.UpdatedAt = time.Now().Add(-ctxwatch.QueueRecordFresh - time.Minute).UTC().Format(time.RFC3339)
		},
		"not the agent's child": func(r *ctxwatch.QueueRecord) { r.MCPPID = os.Getppid() },
	} {
		r := record
		change(&r)
		if err := ctxwatch.SaveQueueRecord(dir, r); err != nil {
			t.Fatal(err)
		}
		if out := stopRun(t, stopInput); out != "" || server.ops() != "" {
			t.Fatalf("%s: %q %s", name, out, server.ops())
		}
	}
	// A record readable by others is not trusted. Windows has no file modes;
	// the record is protected by the user profile folder's ACL.
	if err := ctxwatch.SaveQueueRecord(dir, record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent.queue.json")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if out := stopRun(t, stopInput); out != "" {
			t.Fatalf("a world-readable record was used: %q", out)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Named by CLAWBRIDGE_SESSION, the record must still be the agent's.
	t.Setenv("CLAWBRIDGE_SESSION", "agent")
	if out := stopRun(t, stopInput); out != blocked {
		t.Fatalf("%q", out)
	}
}

// PreCompact holds the session's notifications, under a mark, before it
// gives back the held leases, so nothing is leased again meanwhile; the
// SessionStart after the compaction lifts that hold, and only that hold.
func TestCompactionHoldsGivesBackAndAccepts(t *testing.T) {
	server := &hookServer{status: map[string]any{"policy": "accept"}}
	dir := queueHookSetup(t, server)
	input := hookInput("/t.jsonl", `,"trigger":"auto"`)
	hookRun(t, input, "compacting", "--from-hook")
	if got := server.ops(); got != "notification_status set_notification_policy:hold nack:msg-1/lse_1/0 nack:msg-2/lse_2/0" {
		t.Fatal(got)
	}
	h, held := ctxwatch.LoadCompactionHold(dir, "agent")
	if !held || h.Instance != "inst-1" {
		t.Fatal(h, held)
	}
	server.calls = nil
	hookRun(t, hookInput("/t.jsonl", `,"source":"compact"`), "bind", "--from-hook")
	if got := server.ops(); got != "set_notification_policy:accept" {
		t.Fatal(got)
	}
	if _, held = ctxwatch.LoadCompactionHold(dir, "agent"); held {
		t.Fatal("the mark stayed")
	}
	// Without a mark of its own, bind changes no policy.
	server.calls = nil
	hookRun(t, hookInput("/t.jsonl", `,"source":"compact"`), "bind", "--from-hook")
	if got := server.ops(); got != "" {
		t.Fatal(got)
	}

	// A session the user held is not marked, so it is never set back; its
	// leases still go back.
	server.status["policy"] = "hold"
	hookRun(t, input, "compacting", "--from-hook")
	if got := server.ops(); got != "notification_status nack:msg-1/lse_1/0 nack:msg-2/lse_2/0" {
		t.Fatal(got)
	}
	if _, held = ctxwatch.LoadCompactionHold(dir, "agent"); held {
		t.Fatal("a user's hold was marked as the compaction's")
	}

	// A failed hold leaves no mark, and nothing is given back while
	// leasing could take it again.
	server.calls, server.status["policy"] = nil, "accept"
	server.failOp = "set_notification_policy"
	hookRun(t, input, "compacting", "--from-hook")
	if _, held = ctxwatch.LoadCompactionHold(dir, "agent"); held {
		t.Fatal("a failed hold left its mark")
	}
	if strings.Contains(server.ops(), "nack") {
		t.Fatal(server.ops())
	}
}

// A hook's route is the configured server only when it is the record's.
func TestQueueRouteRefusesAnotherServer(t *testing.T) {
	isolated(t)
	t.Setenv("CLAWBRIDGE_MODE", "cloud")
	t.Setenv("CLAWBRIDGE_SERVER", "https://configured.example.test")
	t.Setenv("CLAWBRIDGE_AUDIENCE", "audience")
	_, _, err := openQueueRoute(context.Background(), ctxwatch.QueueRecord{Server: "https://elsewhere.example.test"})
	if err == nil || !strings.Contains(err.Error(), "not the MCP server's") {
		t.Fatal("a record's server was used without being the configured one", err)
	}
	if _, _, err = openQueueRoute(context.Background(), ctxwatch.QueueRecord{Server: "https://configured.example.test"}); err != nil && strings.Contains(err.Error(), "not the MCP server's") {
		t.Fatal(err)
	}
}
