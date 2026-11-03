package guibackend

import (
	"context"
	"fmt"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
	"clawbridge/internal/tui"
)

// readOps are the only bus operations the P2-5 pages call. Each is a read
// the TUI makes as the person, with no session. Anything else (a device
// ack, a budget set, a names refresh, a link) is refused before it is sent.
var readOps = map[string]bool{
	"list_projects":           true,
	"project_overview":        true,
	"project_threads":         true,
	"project_thread":          true,
	"linear_app_status":       true,
	"linear_write_status":     true,
	"intake_policy_get":       true,
	"intake_trust_list":       true,
	"intake_list":             true,
	"whoami":                  true,
	"dispatcher_model_doctor": true,
	"dispatcher_get":          true,
	"device_list":             true,
	"device_events":           true,
	"owner_transfer_get":      true,
	"invite_list":             true,
	"user_list":               true,
}

// readLinearOps are the LinearSource operations the pages call; the TUI's
// connect, logout, link, unlink, names-refresh, *-set and wizard browsing
// (teams, projects) are not among them.
var readLinearOps = map[string]bool{
	"profiles":       true,
	"issues":         true,
	"issue":          true,
	"intake":         true,
	"project-status": true,
	"roles":          true,
	"views":          true,
	"cancel-kinds":   true,
}

// ReadOnly wraps an executor so that only readOps pass through.
func ReadOnly(exec doctor.Executor) doctor.Executor { return readOnlyExec{exec} }

type readOnlyExec struct{ doctor.Executor }

func (r readOnlyExec) Execute(ctx context.Context, req bus.Request) (bus.Result, error) {
	if !readOps[req.Operation] {
		return bus.Result{}, fmt.Errorf("the UI does not run %q: it is not a read", req.Operation)
	}
	if req.Session != "" {
		return bus.Result{}, fmt.Errorf("the UI reads as the person, with no session")
	}
	return r.Executor.Execute(ctx, req)
}

// ReadOnlyLinear wraps a Linear source so that only readLinearOps pass.
func ReadOnlyLinear(src tui.LinearSource) tui.LinearSource { return readOnlyLinear{src} }

type readOnlyLinear struct{ tui.LinearSource }

func (r readOnlyLinear) Do(ctx context.Context, req tui.LinearRequest) (tui.LinearData, error) {
	if !readLinearOps[req.Operation] {
		return tui.LinearData{}, fmt.Errorf("the UI does not run Linear action %q: it is not a read", req.Operation)
	}
	if req.Roles != nil || req.CancelKinds != nil || req.Confirm || req.ExpectedRevision != 0 {
		return tui.LinearData{}, fmt.Errorf("the UI sends no change to Linear")
	}
	return r.LinearSource.Do(ctx, req)
}
