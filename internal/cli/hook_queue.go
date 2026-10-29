package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
	"clawbridge/internal/remote"
)

// The message queue's agent hooks (MESSAGE-QUEUE-DESIGN §5). They run as
// their own processes and act through the record the agent's MCP server
// keeps: its instance, its route and the leases it holds. Each fails open,
// silently: an error, an old server, no record or no answer in time
// changes nothing.

// stopHookTimeout bounds the Stop hook's question to the server (M5).
const stopHookTimeout = 2 * time.Second

// compactingTimeout bounds the queue's part of the PreCompact hook, inside
// the hook's own limit.
const compactingTimeout = 3 * time.Second

// queueExecutor is what a hook asks: the hosted client or the local store.
type queueExecutor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}

// queueHookNow is the hooks' clock; queueHookRoute opens a record's route.
var (
	queueHookNow   = time.Now
	queueHookRoute = openQueueRoute
)

// openQueueRoute opens the route a record names: its local database, or
// the configured hosted server when that is the record's server. A record
// is data: a server it names that isn't the configured one is refused.
func openQueueRoute(ctx context.Context, r ctxwatch.QueueRecord) (queueExecutor, func(), error) {
	if r.DB != "" {
		store, err := bus.Open(r.DB)
		if err != nil {
			return nil, nil, err
		}
		return store, func() { store.Close() }, nil
	}
	c, _, err := resolveConfig(ctx, Config{})
	if err != nil {
		return nil, nil, err
	}
	if c.Mode != "cloud" || c.Server != r.Server {
		return nil, nil, errors.New("the configured server is not the MCP server's")
	}
	client, err := remote.New(remoteConfig(c))
	if err != nil {
		return nil, nil, err
	}
	return client, func() {}, nil
}

func queueRequest(r ctxwatch.QueueRecord, operation string, args map[string]any) bus.Request {
	args["instance"] = r.Instance
	return bus.Request{Operation: operation, Workspace: r.Workspace, Project: r.Project, Session: r.Session, Args: args}
}

// hookRecord is the queue record this hook's agent may act on.
func hookRecord(session string) (string, ctxwatch.QueueRecord, bool) {
	dir, err := contextDir()
	if err != nil {
		return "", ctxwatch.QueueRecord{}, false
	}
	r, ok := ctxwatch.HookQueueRecord(dir, session, hookAgentPID(), queueHookNow())
	return dir, r, ok
}

// runHook runs `clawbridge hook stop --from-hook`.
func runHook(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	// The exec form (Windows) ends with --hook-tag TAG, which only marks the
	// entry as ClawBridge's.
	if len(args) == 4 && args[2] == "--hook-tag" {
		args = args[:2]
	}
	if len(args) != 2 || args[0] != "stop" || args[1] != "--from-hook" {
		return errors.New("usage: clawbridge hook stop --from-hook (Claude Code's Stop hook, installed by mcp install)")
	}
	stopHook(ctx, os.Getenv("CLAWBRIDGE_SESSION"), stdout)
	return nil
}

// stopReason is the only text the Stop hook gives the agent: fixed words
// and a count, never a sender, subject or body (M5), since Claude Code
// reads the reason as instructions.
func stopReason(waiting int64) string {
	if waiting == 1 {
		return "1 ClawBridge message is waiting: call inbox, handle it, then ack"
	}
	return fmt.Sprintf("%d ClawBridge messages are waiting: call inbox, handle them, then ack", waiting)
}

// stopHook keeps an agent from going idle on mail it hasn't handled: with
// unacked messages it blocks the stop, once per new message. It never
// blocks while the agent already continues from a Stop hook, nor for a
// session whose policy isn't accept, nor for any instance but the MCP
// server's own (M5).
func stopHook(ctx context.Context, session string, stdout io.Writer) {
	in, ok := ctxwatch.ReadHookInput(contextStdin)
	if !ok || in.StopHookActive {
		return
	}
	dir, r, ok := hookRecord(session)
	if !ok {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, stopHookTimeout)
	defer cancel()
	e, done, err := queueHookRoute(bounded, r)
	if err != nil {
		return
	}
	defer done()
	result, err := e.Execute(bounded, queueRequest(r, "notification_status", map[string]any{"queue": true}))
	if err != nil || bounded.Err() != nil {
		return
	}
	data, _ := result.Data.(map[string]any)
	unacked, okUnacked := number(data["unacked_count"])
	latest, okLatest := number(data["latest_seq"])
	// An older server has no queue counts.
	if data["policy"] != "accept" || !okUnacked || !okLatest || unacked <= 0 {
		return
	}
	if m, ok := ctxwatch.LoadStopMark(dir, r.Session); ok && m.Instance == r.Instance && latest <= m.LatestSeq {
		return
	}
	// Without the mark it could block again for the same message: fail open.
	if ctxwatch.SaveStopMark(dir, r.Session, ctxwatch.StopMark{Instance: r.Instance, LatestSeq: latest}) != nil {
		return
	}
	out, _ := json.Marshal(map[string]string{"decision": "block", "reason": stopReason(unacked)})
	fmt.Fprintln(stdout, string(out))
}

// number reads a count from a local result (int64) or a hosted one
// (float64).
func number(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// compactingQueue is the queue's part of the PreCompact hook: hold the
// session's notifications, so nothing new is leased, then give back the
// leases the MCP server holds, without counting an attempt. The agent
// forgets them in the compaction; they are offered again after it. Only a
// policy at accept is held, under a mark, so `context bind` and the MCP
// server set back only a hold of their own.
func compactingQueue(ctx context.Context, session string) {
	dir, r, ok := hookRecord(session)
	if !ok {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, compactingTimeout)
	defer cancel()
	e, done, err := queueHookRoute(bounded, r)
	if err != nil {
		return
	}
	defer done()
	status, err := e.Execute(bounded, queueRequest(r, "notification_status", map[string]any{}))
	if err != nil {
		return
	}
	if data, _ := status.Data.(map[string]any); data["policy"] == "accept" {
		// The mark first: a hold without one would never be lifted.
		if ctxwatch.SaveCompactionHold(dir, r.Session, ctxwatch.CompactionHold{Instance: r.Instance, HeldAt: queueHookNow().UTC().Format(time.RFC3339)}) != nil {
			return
		}
		if _, err = e.Execute(bounded, queueRequest(r, "set_notification_policy", map[string]any{"policy": "hold"})); err != nil {
			ctxwatch.RemoveCompactionHold(dir, r.Session)
			return
		}
	}
	for _, l := range r.Leases {
		if bounded.Err() != nil {
			return
		}
		_, _ = e.Execute(bounded, queueRequest(r, "nack", map[string]any{"message_id": l.MessageID, "lease_id": l.LeaseID, "delay_seconds": 0}))
	}
}

// bindQueue is the queue's part of the SessionStart hook: a hold the
// compaction set is lifted, so the session's deliveries are offered again.
// A session that started after a crash mid-compaction is lifted too.
func bindQueue(ctx context.Context, session string) {
	dir, r, ok := hookRecord(session)
	if !ok {
		return
	}
	if _, held := ctxwatch.LoadCompactionHold(dir, r.Session); !held {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, compactingTimeout)
	defer cancel()
	e, done, err := queueHookRoute(bounded, r)
	if err != nil {
		return
	}
	defer done()
	if _, err = e.Execute(bounded, queueRequest(r, "set_notification_policy", map[string]any{"policy": "accept"})); err == nil {
		ctxwatch.RemoveCompactionHold(dir, r.Session)
	}
}
