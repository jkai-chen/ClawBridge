package cli

import (
	"context"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/tui"
)

// An issue opened from a session (i) before a connection is chosen takes
// the only one; with none, it asks for one on the Linear screen, as
// intake does.
func TestLinearTUIIssueWithoutAConnectionAsks(t *testing.T) {
	// The state home isolated(t) gives, as the other Linear tests use: a
	// chmod'ed TempDir isn't private on Windows.
	isolated(t)
	dir, err := linearDirectory()
	if err != nil {
		t.Fatal(err)
	}
	s := &linearTUISource{directory: dir, manager: newLinearManager(dir)}
	for _, op := range []string{"issue", "intake"} {
		_, err := s.Do(context.Background(), tui.LinearRequest{Operation: op, IssueID: "00000000-0000-4000-8000-0000000000c1"})
		if err == nil || !strings.Contains(err.Error(), "choose your Linear connection on the Linear screen first") {
			t.Fatal(op, err)
		}
	}
}

// tuiStatusExecutor answers the server's project status operations.
type tuiStatusExecutor struct{ requests []bus.Request }

func (e *tuiStatusExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	e.requests = append(e.requests, r)
	return bus.Result{Source: "server", Data: map[string]any{"linear_project": r.Args["linear_project"], "preview": true, "expected_status_id": "s-1",
		"needs_allow_open": true, "status": map[string]any{"id": "s-1", "name": "Started", "type": "started"},
		"issues": map[string]any{"total": float64(3), "open": float64(1), "complete": true}, "to": map[string]any{"id": "s-2", "name": "Done", "type": "completed"}}}, nil
}

// U61: the TUI reads and sets a project's status as linear project status
// does: a read with the TUI's session, a set as the person with none,
// refused in a TUI with a session or under an agent.
func TestLinearTUIProjectStatus(t *testing.T) {
	old := closesEnv
	t.Cleanup(func() { closesEnv = old })
	closesEnv = func() []string { return []string{"HOME=/x"} }
	// The state home isolated(t) gives: a chmod'ed TempDir isn't private on
	// Windows.
	isolated(t)
	dir, err := linearDirectory()
	if err != nil {
		t.Fatal(err)
	}
	const lp = "00000000-0000-4000-8000-0000000000c1"
	e := &tuiStatusExecutor{}
	s := &linearTUISource{directory: dir, manager: newLinearManager(dir), executor: e, scope: bus.Request{Workspace: "ws", Project: "prj", Session: "sess", Args: map[string]any{}}}
	d, err := s.Do(context.Background(), tui.LinearRequest{Operation: "project-status", LinearProject: lp, Suggest: true})
	if err != nil || d.ProjectStatus == nil || d.ProjectStatus.Status.Name != "Started" || d.ProjectStatus.Issues.Open != 1 || d.ProjectStatus.To.Name != "Done" || !d.ProjectStatus.NeedsAllowOpen {
		t.Fatal(err, d.ProjectStatus)
	}
	if r := e.requests[0]; r.Operation != "linear_app_project_status" || r.Session != "sess" || r.Args["suggest"] != true || r.Args["linear_project"] != lp {
		t.Fatal(r)
	}
	set := tui.LinearRequest{Operation: "project-status-set", LinearProject: lp, StatusType: "completed", OperationID: "tui-1", Confirm: true, ExpectedStatus: "s-1", AllowOpen: true}
	if _, err = s.Do(context.Background(), set); err == nil || !strings.Contains(err.Error(), "open the TUI without --session") || len(e.requests) != 1 {
		t.Fatal("a set with a session:", err)
	}
	s.scope.Session = ""
	closesEnv = func() []string { return []string{"CLAUDECODE=1"} }
	if _, err = s.Do(context.Background(), set); err == nil || !strings.Contains(err.Error(), "CLAUDECODE says an agent runs this TUI") || len(e.requests) != 1 {
		t.Fatal("a set under an agent:", err)
	}
	closesEnv = func() []string { return []string{"HOME=/x"} }
	if _, err = s.Do(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	r := e.requests[1]
	want := map[string]any{"linear_project": lp, "type": "completed", "operation_id": "tui-1", "confirm": true, "expected_status_id": "s-1", "allow_open": true}
	for k, v := range want {
		if r.Args[k] != v {
			t.Fatal(k, r.Args)
		}
	}
	if r.Operation != "linear_app_project_status_set" || r.Session != "" || r.Args["suggest"] != nil {
		t.Fatal(r)
	}
	// A confirm without allow_open sends none.
	set.AllowOpen = false
	if _, err = s.Do(context.Background(), set); err != nil || e.requests[2].Args["confirm"] != true || e.requests[2].Args["allow_open"] != nil {
		t.Fatal(err, e.requests[2].Args)
	}
	// A preview sends no confirm and no expected status.
	set.Confirm = false
	if _, err = s.Do(context.Background(), set); err != nil || e.requests[3].Args["confirm"] != nil || e.requests[3].Args["expected_status_id"] != nil || e.requests[3].Args["allow_open"] != nil {
		t.Fatal(err, e.requests[3].Args)
	}
}
