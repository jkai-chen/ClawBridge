package guibackend

import (
	"context"
	"encoding/json"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
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
