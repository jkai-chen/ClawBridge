package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
)

// QueueState is what the client read locally about this session's message
// queue: the record its agent's MCP server keeps, and a compaction's hold.
type QueueState struct {
	Session string
	Path    string
	Record  *ctxwatch.QueueRecord
	// Hold is the mark of a compaction's hold, when one is set.
	Hold *ctxwatch.CompactionHold
	Err  error
}

// checkQueue reports how this session takes its messages (MESSAGE-QUEUE-
// DESIGN §5): its MCP server's stream, and the server's unacked and dead
// counts, a pure read. It warns, and never fails.
func checkQueue(ctx context.Context, s QueueState, config Config, executor Executor, now time.Time, add func(Check)) {
	c := Check{ID: "queue", Title: "Message queue", Status: Pass, Path: s.Path}
	var notes []string
	instance, from := config.Instance, "this run's --instance"
	switch {
	case s.Err != nil:
		c.Status = Warn
		notes = append(notes, s.Err.Error())
	case s.Record == nil:
		notes = append(notes, fmt.Sprintf("No MCP server of session %s is running on this machine.", s.Session))
	default:
		r := s.Record
		instance, from = r.Instance, "the MCP server's record"
		at, err := time.Parse(time.RFC3339, r.UpdatedAt)
		switch {
		case err != nil || now.Sub(at) > ctxwatch.QueueRecordFresh:
			c.Status = Warn
			notes = append(notes, "The MCP server's record is stale: the server stopped without removing it, or is stuck.")
		case r.Stream == "stream":
			notes = append(notes, fmt.Sprintf("The MCP server holds the message stream, with %d lease(s).", len(r.Leases)))
		case r.Stream == "polling":
			notes = append(notes, fmt.Sprintf("The MCP server polls the queue, with %d lease(s).", len(r.Leases)))
		case r.Stream == "connecting":
			notes = append(notes, "The MCP server is connecting to the message stream.")
		case r.Stream == "ended":
			c.Status = Warn
			notes = append(notes, "The MCP server's message stream ended: another consumer of the session replaced it, or its access was removed.")
			c.Action = "Restart the agent, and check that one agent runs the session."
		default:
			notes = append(notes, "The MCP server takes deliveries from the agent's first inbox call; the server may have no queue.")
		}
	}
	if s.Hold != nil {
		c.Status = Warn
		notes = append(notes, fmt.Sprintf("Its notifications are held for a compaction since %s.", s.Hold.HeldAt))
	}
	if executor != nil && instance != "" && config.Session != "" {
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		result, err := executor.Execute(bounded, bus.Request{Operation: "notification_status", Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: map[string]any{"instance": instance, "queue": true}})
		cancel()
		var status struct {
			Policy  string `json:"policy"`
			Unacked *int64 `json:"unacked_count"`
			Dead    *int64 `json:"dead_count"`
		}
		raw, _ := json.Marshal(result.Data)
		switch {
		case err != nil:
			c.Status = Warn
			code := safeCode(err)
			notes = append(notes, "The server's queue counts couldn't be read: "+code+".")
			switch code {
			case "invalid":
				c.Action = "The server refused session " + config.Session + " with instance " + instance + " (from " + from + "): run doctor with the session's current --instance (its whoami shows it), or restart its MCP server so its record names the current one."
			case "forbidden":
				c.Action = "Session " + config.Session + " belongs to another principal: run doctor as the person who registered it."
			case "not_found":
				c.Action = "The server has no session " + config.Session + " in this project: check --session and --project."
			case "unauthenticated", "device_auth_failed", "clock_skew":
				c.Action = "Sign in again on this machine (the auth checks above say how), then run doctor again."
			}
		case json.Unmarshal(raw, &status) != nil || status.Unacked == nil || status.Dead == nil:
			notes = append(notes, "The server has no message queue.")
		default:
			notes = append(notes, fmt.Sprintf("%d unacked, %d dead.", *status.Unacked, *status.Dead))
			if status.Policy != "accept" {
				c.Status = Warn
				notes = append(notes, fmt.Sprintf("The notification policy is %s: nothing is leased, so its messages wait, with no attempt counted and no dead letter, until it is accept.", status.Policy))
				c.Action = "Set the policy back to accept when the session should take messages."
			}
			if *status.Dead > 0 {
				c.Status = Warn
				c.Action = "The coordinator or a project owner settles dead letters: deadletters_list, then requeue or dismiss each."
			}
		}
	}
	c.Message = strings.Join(notes, " ")
	add(c)
}
