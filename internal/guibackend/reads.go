package guibackend

import (
	"context"
	"encoding/json"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
	"clawbridge/internal/launchcmd"
)

// Scope is the routing a read is made with: what the TUI's config holds.
type Scope struct {
	Workspace, Project, Session, Instance string
}

// SessionsList is the TUI's Sessions screen request: the first page of
// list_sessions, 100 to a page. It answers {source, data} so the GUI can
// tell a mirror copy from a live answer.
func SessionsList(exec doctor.Executor, scope Scope) Method {
	return Method{Class: Read, Call: func(ctx context.Context, _ json.RawMessage) (any, error) {
		args := map[string]any{"limit": 100, "after": int64(0)}
		if scope.Session != "" && scope.Instance != "" {
			args["instance"] = scope.Instance
		}
		return exec.Execute(ctx, bus.Request{Operation: "list_sessions", Workspace: scope.Workspace, Project: scope.Project, Session: scope.Session, Args: args})
	}}
}

// DoctorChecks runs doctor's checks and answers the report.
func DoctorChecks(run func(context.Context) (doctor.Report, error)) Method {
	return Method{Class: Read, Call: func(ctx context.Context, _ json.RawMessage) (any, error) {
		return run(ctx)
	}}
}

// LaunchCommand is sessions.launchCommand {session, agent}: the shell line
// that starts the session's agent in the person's own terminal (FR44). The
// same code answers `clawbridge launch-command`; it refuses a session
// another principal owns, whatever the page shows.
func LaunchCommand(src launchcmd.Source) Method {
	return Method{Class: Read, Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Session string `json:"session"`
			Agent   string `json:"agent"`
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, &Error{Code: CodeParams, Message: "params must be {session, agent}"}
			}
		}
		if p.Session == "" {
			return nil, &Error{Code: CodeParams, Message: "session is required"}
		}
		if p.Agent == "" {
			p.Agent = launchcmd.ClaudeCode
		}
		if p.Agent != launchcmd.ClaudeCode && p.Agent != launchcmd.Codex {
			return nil, &Error{Code: CodeParams, Message: "agent must be claude-code or codex"}
		}
		return src.Command(ctx, p.Session, p.Agent)
	}}
}

// OutboxCountEvent is the event the page shows as the queued-writes count.
const OutboxCountEvent = "outbox.count"

// PushOutboxCount sends outbox.count {waiting} every interval until ctx
// ends (every time, so a tab opened later learns the count within one
// interval). count is nil in local mode, which has no outbox: nothing is
// sent.
func PushOutboxCount(ctx context.Context, s *Server, count func() int, interval time.Duration) {
	if count == nil {
		return
	}
	send := func() { _ = s.Notify(OutboxCountEvent, map[string]int{"waiting": count()}) }
	send()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send()
		}
	}
}
